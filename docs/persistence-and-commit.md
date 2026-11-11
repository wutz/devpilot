# 持久化与镜像提交机制深潜

本文详细定义 DevPilot 最核心的两大底层组件：**`devpilot-init`（容器内 OverlayFS 切根启动器）** 与 **`devpilot-commit`（非特权流式 OCI 层构建器）**，并重点剖析 Linux 内核叠层限制、`RuntimeClass: nvidia` 驱动子挂载遮蔽、Whiteout/Opaque 元数据转换及 CSI 快照热提交的实现规范。

---

## 1. Rootdisk PVC 目录布局与元数据契约

每个 `DevBox` 拥有一块独立的 `ReadWriteOnce` 块存储卷（默认挂载到主容器启动期的临时挂载点 `/mnt/.devpilot-rootdisk`，切根完成后对普通用户隐藏）：

```text
/mnt/.devpilot-rootdisk/
├── upper/                  # OverlayFS upperdir：保存容器根文件系统的一切新增、修改与 whiteout
├── work/                   # OverlayFS workdir：必须与 upper/ 位于同一文件系统（原子 rename 要求）
├── merged/                 # 组装目标挂载点（切根后成为新的 /）
└── .meta/
    ├── base-image.json     # 首次启动锁定的底层镜像摘要、原始 Entrypoint/Cmd/Env 快照
    └── generation.json     # 最近一次挂载时间、Rebase 世代号与 cleanUnmount 标记
```

### `.meta/base-image.json` 结构

```json
{
  "imageRef": "cr.example.com/public/pytorch:2.4.0-cuda12.4-ubuntu22.04",
  "imageDigest": "sha256:8f4e2c9a...",
  "createdAt": "2026-11-11T08:00:00Z",
  "rebaseCount": 0
}
```

**不变量（Invariant）**：
- 当 `upper/` 非空且 `.meta/base-image.json` 存在时，新拉起的 Pod 底层镜像摘要**必须**等于 `imageDigest`。
- 若检测到不一致（例如管理员强行修改了未锁 digest 的镜像 tag），`devpilot-init` 拒绝挂载并以清晰退出码报错，防止底层 `lowerdir` inode 变动破坏 `upper/` 中的硬链接或动态库符号。

---

## 2. `devpilot-init`：内核态 OverlayFS 组装与安全切根

### 2.1 注入方式

为兼容任意用户镜像（Ubuntu、Debian、Rocky Linux 甚至精简镜像，无需用户提前在镜像内安装任何组件）：
1. Operator 为开发机 Pod 注入一个极小的 `initContainer`（`devpilot-init` 镜像，仅含单个 `CGO_ENABLED=0` 静态编译的 Go 二进制，体积 < 10MB），将其拷贝到内存 `emptyDir` 卷 `/run/devpilot/devpilot-init`。
2. Operator 通过读取基础镜像配置（或由控制器缓存镜像元数据）获取原镜像的 `ENTRYPOINT` 与 `CMD`，将主容器的 `command` 改写为：
   ```yaml
   command: ["/run/devpilot/devpilot-init", "--"]
   args: ["<original-entrypoint-and-cmd>"]
   ```

### 2.2 Linux 内核 OverlayFS 叠层深度分析（`s_stack_depth`）

在 `containerd`（Linux 5.15 内核）下启动容器时，容器初始的 `/` 本身已经是一个内核 `overlay` 挂载（`s_stack_depth = 1`）：

```text
容器初始 / (overlay, s_stack_depth = 1)
  ├─ lowerdir = containerd 只读镜像层 (layerN:...:layer1)
  └─ upperdir = containerd 临时可写层 (随 Pod 销毁)
```

Linux 内核（`fs/overlayfs/super.c`）规定最大文件系统叠层深度 `FILESYSTEM_MAX_STACK_DEPTH = 2`，且自 Linux 4.19 起支持将一层只读 `overlay` 作为另一层 `overlay` 的 `lowerdir`：

1. `devpilot-init` 首先创建私有挂载传播（`mount("", "/", "", MS_PRIVATE|MS_REC, "")`），确保所有挂载操作严格限制在当前容器的 Mount Namespace 内。
2. 将容器初始 `/` 只读绑定挂载到 `/run/devpilot/base-lower`（`MS_BIND` + `MS_RDONLY | MS_REMOUNT`，不包含子挂载 `MS_REC`，保持纯净基础镜像视图）。
3. 执行内核 `overlay` 挂载：
   ```c
   mount("overlay", "/mnt/.devpilot-rootdisk/merged", "overlay", 0,
         "lowerdir=/run/devpilot/base-lower,"
         "upperdir=/mnt/.devpilot-rootdisk/upper,"
         "workdir=/mnt/.devpilot-rootdisk/work,"
         "index=off,metacopy=off");
   ```
   - 此时 `merged` 的 `s_stack_depth = base-lower(1) + 1 = 2 <= FILESYSTEM_MAX_STACK_DEPTH(2)`，完全符合 Linux 5.15+ 内核约束，获得**原生内核级读写与 page-cache 性能**。
   - 显式设置 `metacopy=off,index=off`：
     - `metacopy=off` 确保 `chmod`/`chown` 修改文件权限时在 `upper/` 中落盘完整文件内容（而非仅写 `trusted.overlay.metacopy` 元数据空壳），使后续非特权的 `devpilot-commit` 直接打包 `upper/` 即可拿到完整文件数据，避免产生损坏的 0 字节文件！
     - `index=off` 允许该块存储 PVC 跨节点重新挂载到不同机器的 `base-lower`（不同节点 containerd 解压出的底层文件系统 UUID 不同，若开启 `index=on` 跨节点挂载会因 origin fh 校验失败报错 `ESTALE`）。

> **兜底机制**：若未来遇到特殊定制内核或嵌套环境（如容器运行在已叠层的特殊存储之上导致 `mount("overlay")` 返回 `EINVAL` 且栈深超限），`devpilot-init` 支持可选自动降级至随二进制内置的静态 `fuse-overlayfs`，并在 Pod Event 中发出性能降级提示。

### 2.3 解决 `RuntimeClass: nvidia` 与 K8s 子挂载遮蔽陷阱（关键）

当 Pod 指定 `runtimeClassName: nvidia` 时，在 `devpilot-init` 启动**之前**，宿主机上的 `nvidia-container-toolkit`（OCI prestart hook）和 `kubelet` 已经在容器初始 `/` 上完成了数十个关键的 **子挂载（Submounts）**：

- **GPU 驱动与工具**：`/usr/bin/nvidia-smi`、`/usr/bin/nvidia-debugdump`、`/usr/lib/x86_64-linux-gnu/libcuda.so.550.xx`、`/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.*`、`/proc/driver/nvidia`、`/dev/nvidia0`、`/dev/nvidiactl`、`/dev/nvidia-uvm` 等；
- **Kubelet 标准挂载**：`/proc`、`/sys`、`/dev`、`/dev/pts`、`/dev/shm`（训练大共享内存）、`/etc/hosts`、`/etc/hostname`、`/etc/resolv.conf`、`/var/run/secrets/kubernetes.io/serviceaccount`；
- **用户附加数据卷**：例如共享高速存储 `/root/shared-nvme`（`shared-vast-cs` / `shared-juice`）。

**陷阱原因**：Linux OverlayFS 的 `lowerdir` 只读取该挂载点自身文件系统的目录树，**绝不会穿透子挂载点**。例如初始 `/usr/bin/nvidia-smi` 是一个来自宿主机的单文件 bind mount，在底层基础镜像里该路径要么不存在、要么是空占位文件；若直接 `pivot_root` 进入 `merged`，容器内执行 `nvidia-smi` 或 `torch.cuda.is_available()` 会立即报错找不到驱动！

**精确迁移算法**：

1. `devpilot-init` 解析 `/proc/self/mountinfo`，筛选出挂载点位于 `/` 下、且**不属于** `/mnt/.devpilot-rootdisk` 与 `/run/devpilot` 的所有直接与间接子挂载点集合 $M$。
2. 将集合 $M$ 按挂载目标路径的目录层级深度升序排序（例如先 `/dev`，再 `/dev/pts`、`/dev/shm`、`/dev/nvidia0`）。
3. 遍历每个挂载项 $m \in M$：
   - **仅当目标路径在 `merged` 中尚不存在时**（`os.Lstat` 返回 `ENOENT`）才创建空占位文件或目录（基础镜像或 containerd 初始层已有的挂载点在 `merged` 中天然可见，避免无谓触发 `upper/` copy-up 产生 0 字节覆盖文件）；
   - 执行 `mount(m.target, "/mnt/.devpilot-rootdisk/merged" + m.target, "", MS_BIND | MS_REC, "")` 将原挂载（含只读属性、设备节点、驱动 `.so` 库、共享存储 PVC）原封不动迁移至新根下。
4. 执行安全切根（`pivot_root`）：
   ```go
   os.MkdirAll("/mnt/.devpilot-rootdisk/merged/.old_root", 0700)
   unix.PivotRoot("/mnt/.devpilot-rootdisk/merged", "/mnt/.devpilot-rootdisk/merged/.old_root")
   unix.Chdir("/")
   unix.Unmount("/.old_root", unix.MNT_DETACH)
   os.Remove("/.old_root")
   ```
   切根并懒卸载 `/.old_root` 后，容器内的用户（即使是 `root`）在 `/proc/self/mountinfo` 中再也无法直接访问原始 `/mnt/.devpilot-rootdisk/upper` 裸目录，杜绝用户误删 `.meta/` 或直接篡改 `workdir` 破坏内核 OverlayFS 状态。

### 2.4 PID 1 进程托管与优雅停机刷盘

切根完成后，`devpilot-init` 继续作为容器的 **PID 1（类似 `tini`）** 运行：
1. 按配置启动受管后台服务（如内置/容器内 `sshd`、JupyterLab）；
2. 拉起用户的启动命令（默认 `sleep infinity` 或用户自定义脚本）；
3. 负责回收僵尸进程（`waitpid(-1, WNOHANG)`）；
4. **优雅关机钩子**：收到 Kubelet 发来的 `SIGTERM` 时，向子进程组转发信号，等待业务进程退出（或超时前），主动调用 `unix.Sync()` 强制将内核缓冲区脏页刷入块存储 PVC，更新健康卸载标记后退出，确保停机持久化零丢数据。

---

## 3. `devpilot-commit`：非特权流式 OCI 镜像层构建

### 3.1 为什么不需要 `containerd.sock` 和本地基础镜像

OCI Image Specification 中，一个容器镜像本质上由三部分组成：
1. **Layer Blobs**：一系列按序叠加的 `.tar.gz` 或 `.tar.zstd` 文件系统归档；
2. **Config JSON**：记录 `rootfs.diff_ids`（每层解压后 tar 的 SHA256）、`Env`、`Cmd`、`Entrypoint`、`WorkingDir`、`Labels`；
3. **Manifest JSON**：记录 Config 与各 Layer 压缩后摘要（digest）及大小的索引文件。

由于开发机运行期的所有根盘修改都精确沉淀在块存储 PVC 的 `upper/` 目录中，且 `metacopy=off` 保证了修改过的文件在 `upper/` 中拥有完整内容，因此 **`upper/` 天然就是相对于基础镜像（`base-image.json`）的标准增量文件系统层**！

借助 Google `go-containerregistry`（`v1/remote` + `v1/mutate`）：
- `devpilot-commit` 只需通过 HTTPS 从 Harbor 读取几 KB 的基础镜像 Manifest 与 Config JSON；
- **完全不需要拉取基础镜像原有的几十 GB CUDA/PyTorch 层**（Harbor 在同一仓库或跨项目 mount blob 时直接在服务端秒级引用已有层 digest）；
- 仅需流式读取 `upper/` 生成单层增量 Tar 流，边计算未压缩 `diff_id` 与压缩后 `digest` 边流式上传至 Harbor，最后提交更新后的 Config 与 Manifest。

### 3.2 OverlayFS 元数据到 OCI Tar 标准格式的精确映射

Linux 内核 OverlayFS 在磁盘 `upper/` 上记录“删除”和“目录替换”的方式与 OCI Image Layer Tar 规范不同，`devpilot-commit` 在流式打包时必须做如下精确转换：

| 文件系统操作场景 | `upper/` 磁盘物理表现 | OCI Layer Tar 规范输出 | 处理细节 |
| --- | --- | --- | --- |
| 新增或修改普通文件 `/usr/local/bin/uv` | 普通文件，完整内容与 mode/uid/gid | Tar 普通条目 `usr/local/bin/uv` | 保留权限、UID/GID、mtime、符号链接目标与普通扩展属性 |
| 删除底层镜像已有的文件 `/etc/motd` | **字符设备节点（Character Device）**，主次设备号 `major=0, minor=0`（`stat.Mode & S_IFCHR` 且 `Rdev == 0`） | 空普通文件 `etc/.wh.motd`（大小 0，权限 `0600`） | 绝不能把 `0:0` 字符设备原样写进 Tar，否则普通 tar/containerd 解压时无法识别删除语义甚至因无 `CAP_MKNOD` 失败 |
| 删除底层目录并重建同名新目录 `/opt/conda`（Opaque Directory） | 目录带有扩展属性 `trusted.overlay.opaque = "y"` 或 `user.overlay.opaque = "y"` | 先写入目录条目 `opt/conda/`，紧接着写入空文件 `opt/conda/.wh..wh..opq`，再写入目录内新文件 | `devpilot-commit` 同时检查 `trusted.overlay.opaque` 与 `user.overlay.opaque`；注意：在非 root 读取 `trusted.*` xattr 时受内核 `CAP_SYS_ADMIN` 限制——见下方 3.3 节解决方案 |
| 硬链接（Hardlinks） | 同一 `upper/` 内相同 `(dev, ino)` 且 `nlink > 1` | 首个路径写 `TypeReg`，后续同 inode 路径写 `TypeLink` 指向首个路径 | 维护内存 `map[uint64]string`（inode → 首个归档路径），避免重复存储 Conda 硬链接包缓存 |

### 3.3 关键细节：`trusted.overlay.opaque` 的非特权读取处理

Linux 内核规定 `trusted.*` 命名空间的扩展属性（xattr）只有具备 `CAP_SYS_ADMIN` 的进程通过 `getxattr` 才能看到；无 `CAP_SYS_ADMIN` 的普通容器进程调用 `llistxattr` 时内核会静默隐藏 `trusted.overlay.opaque`！

为兼顾**安全性**与**Opaque 目录 100% 准确捕获**，采用双重保障设计：
1. **运行期同步标记（主机制）** + **关机/快照前元数据清单生成**：
   - 在 `devpilot-init`（本身在容器内拥有受控 `CAP_SYS_ADMIN`）收到快照前 `sync` 指令或优雅停机 `SIGTERM` 时，快速扫描一次 `upper/` 目录树（仅 `readdir` 目录节点，不读文件内容，十万目录毫秒级完成），将所有含 `trusted.overlay.opaque=y` 的相对目录路径写入 `.meta/opaque-dirs.json`。
2. **`devpilot-commit` 读取**：
   - `devpilot-commit` 完全以非特权运行（`allowPrivilegeEscalation: false`、`capabilities.drop: ["ALL"]`、`seccompProfile: RuntimeDefault`），结合 `.meta/opaque-dirs.json` 与 `user.overlay.opaque` xattr 生成 `.wh..wh..opq`。
   - 即便发生极端断电未更新 `opaque-dirs.json`，也可在隔离构建命名空间 `<ns>-build`（与 `imagebuild` 相同的受控命名空间）为扫描器单独授予只读挂载下的 `CAP_SYS_ADMIN` 读取 `trusted.overlay.opaque`，两者互为兜底。

### 3.4 默认排除列表（Exclude Rules）

避免把运行时噪音、APT/Pip 下载缓存包或平台注入的临时敏感凭证固化进镜像，`devpilot-commit` 内置默认排除规则（同时允许用户在 `DevBoxCommit.spec.excludePaths` 中追加）：

- **运行时与临时目录**：`tmp/**`、`var/tmp/**`、`run/**`、`var/run/**`、`proc/**`、`sys/**`、`dev/**`、`.old_root/**`
- **子挂载空占位文件剔除**：排除 `etc/hosts`、`etc/hostname`、`etc/resolv.conf` 以及任何由 `nvidia-container-toolkit` 首次注入时在 `upper/` 留下的 0 字节驱动占位文件（如 0 字节的 `usr/bin/nvidia-smi` 或 `usr/lib/*/libcuda.so*`），确保产出的镜像在任意环境干净通用
- **平台注入凭证与动态配置**：
  - `etc/ssh/ssh_host_*_key*`（避免多台基于该镜像创建的开发机共享同一 SSH Host 私钥）
  - `root/.ssh/authorized_keys` 中带有 `# managed-by:devpilot` 标记的公钥行（保留用户自行写入的其他配置，剥离平台网关身份公钥与个人公钥）
  - `run/secrets/**`、`var/run/secrets/**`
  - Jupyter 运行时生成的临时 token 文件（`root/.local/share/jupyter/runtime/**`）
- **可选缓存清理（`spec.cleanPackageCache: true`，默认开启）**：
  - `var/cache/apt/archives/*.deb`、`var/cache/apt/archives/partial/**`
  - `root/.cache/pip/**`、`home/*/.cache/pip/**`

---

## 4. 离线提交 vs 在线热提交（CSI `VolumeSnapshot`）

```text
情况 A：DevBox 处于 Stopped / Interrupted（离线提交）
  rootdisk PVC (未挂载) ──(ReadOnlyMany / RWO 只读挂载)──► Commit Job ──► Harbor

情况 B：DevBox 处于 Running（在线热提交，不中断 GPU 进程）
  1. Operator 调用 Pod 内 devpilot-init 管理接口执行 sync + 刷新 .meta/opaque-dirs.json
  2. 创建 VolumeSnapshot (引用 csi-rbdplugin-snapclass 或 vast-snapshot-cs)
  3. 等待 VolumeSnapshot.status.readyToUse == true (Ceph RBD / VAST 秒级 COW 快照)
  4. 从 VolumeSnapshot 恢复临时只读 PVC: commit-<name>-ro
  5. 启动 Commit Job (可调度在任意普通 CPU 节点) 流式打包并推送到 Harbor
  6. 成功后删除临时 PVC 与 VolumeSnapshot (通过 OwnerReference + Finalizer 双重保障清理)
```

### 在线一致性保障

- **默认模式（`consistency: Sync`）**：触发内核 `sync()` 将文件系统页缓存刷盘后立即打 CSI 快照，对开发机内正在运行的 GPU 训练零停顿，满足 99% 交互式配好环境后保存镜像的场景（此时 `apt`/`conda` 命令已执行完毕）。
- **严格冻结模式（`consistency: Freeze`，可选）**：通过 `devpilot-init` 对 `/mnt/.devpilot-rootdisk` 执行 `FIFREEZE` ioctl 冻结文件系统写操作，创建 `VolumeSnapshot` 对象后立即 `FITHAW` 解冻（设置硬超时 3 秒自动解冻防死锁），实现严格的崩溃一致性快照。
- **无 CSI Snapshot 环境降级**：当 StorageClass 未配置对应的 `VolumeSnapshotClass` 时，Operator 通过 `podAffinity` 将 Commit Job 调度到 `DevBox` 所在同一宿主机节点，以 `readOnly: true` 共享挂载该 RWO 卷完成构建；若 CSI 驱动连同节点多 Pod 挂载也拒绝，则明确报错提示用户先关机再提交。

---

## 5. 可选自动变基（Rebase）防膨胀机制

如果用户在数月内对同一台开发机连续执行了十几次环境大改与 `DevBoxCommit`：
- 若不清理，`upper/` 会累积历史所有变更；
- 若开启 `DevBoxCommit.spec.rebaseDevBox: true`：
  - **数据零丢失安全约束（Invariant）**：`rebaseDevBox: true` **仅允许在离线停机提交（`DevBox` 处于 `Stopped` / `Interrupted`，此时无并发写入）时生效**！若对运行中的开发机做在线热快照提交，用户在快照时刻之后继续写入 `upper/` 的新文件并未包含在该快照镜像里，一旦重启清空 `upper/` 会导致快照后的增量修改静默丢失，因此控制器显式拒绝在在线热提交时启用 `rebaseDevBox: true`。
  - **停机变基流程**：
    1. `devpilot-commit` 将静态一致的 `base + upper` 固化推送到 Harbor 得到新镜像 `repo:tag@sha256:new_digest`；
    2. Operator 将 `DevBox` 的 `status.pendingRebaseImage` 记录为该新镜像摘要；
    3. 下次用户开机启动 `DevBox` 时，Operator 以 `new_digest` 拉起 Pod，`devpilot-init` 校验新底座镜像就绪后，原子清理旧 `upper/` + `work/` 并重写 `.meta/base-image.json`（`rebaseCount + 1`）。
    4. 自此 `upper/` 归零轻装上阵，文件系统状态 100% 连续无损（配合 `bz1-dev1` 已部署的 `spegel` P2P 镜像分发加速节点拉取）。
