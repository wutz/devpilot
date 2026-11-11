# 架构总览与方案选型

## 1. 背景、目标与非目标

### 1.1 背景

机器学习研发高度依赖交互式开发机（DevBox）：算法工程师在 GPU 容器中通过 VS Code Remote-SSH 或 JupyterLab 调试代码，期间频繁执行 `apt install` 安装系统库（如 `ffmpeg`、`libgl1`、`ninja-build`）、通过 `conda`/`uv`/`pip` 构建 Python 环境、编译自定义 CUDA 算子（如 FlashAttention），并在确认环境可用后希望将其保存为容器镜像，直接用于后续多机分布式训练（如 `TrainPilot` `TrainTask`）。

标准 Kubernetes 工作负载（`Deployment` / `StatefulSet` / `Sandbox`）在停止并删除 Pod 释放 GPU 时，容器可写层（containerd ephemeral snapshot）会随 Pod 销毁；若仅将 `/root` 或 `/workspace` 挂载到 PVC，则 `/usr`、`/etc`、`/opt` 中的系统包与动态链接库全部丢失，也无法将这些分散修改直接提交为完整镜像。

### 1.2 设计目标

- **全根文件系统（Rootfs）修改关机持久化**：容器内任意目录（除显式挂载的临时内存盘与共享盘外）的新增、修改、权限变更与文件删除（whiteout）在停机或被抢占驱逐后 100% 保留。
- **停机零算力占用与自由跨节点调度**：关机或中断后立即删除 Pod 释放 GPU/CPU/内存并停止算力计费；再次启动时无需绑定原宿主机，可在集群任意空闲 GPU 节点秒级拉起。
- **不耦合宿主机运行时的镜像固化（Commit & Push）**：无需在每个 GPU 节点运行挂载 `/run/containerd/containerd.sock` 的特权 DaemonSet；支持停机离线提交与运行中在线热提交（基于 CSI `VolumeSnapshot`），无需拉取几十 GB 基础镜像即可秒级流式追加新层推送到 Harbor。
- **原生契合现有 AI 平台生态**：独立自包含运行（不强依赖外部 API Server），同时原生兼容 `sshgateway`（SSH 路由）、`workspace`（多租户项目与 Harbor 凭证）、`trainpilot`（固化后的镜像无缝投递训练任务，对齐抢占式与运行段计费语义）。

### 1.3 非目标（v1）

- **不做虚拟机级内核热迁移（CRIU / Live Migration）**：关机与抢占持久化针对文件系统状态（磁盘持久化），内存中的进程状态在关机后不保留。
- **不在开发机内提供 Docker-in-Docker 守护进程**：构建 Dockerfile 交由独立的 `imagebuild` Operator（rootless BuildKit）负责，开发机专注于交互式编码调试与环境固化。
- **不内置多机分布式训练编排**：多机 Gang Scheduling、RDMA 自动注入与故障自愈由 `trainpilot` 负责。

---

## 2. 技术路线对比与选型依据

在 K8s 上实现「容器关机持久化 + 提交镜像推送」主要有四条技术路线：

| 对比维度 | 路线 A：PVC 持久化 OverlayFS `upperdir` + 流式 OCI 层追加（**本项目采用**） | 路线 B：节点 DaemonSet 调 `containerd.sock` 执行 `commit`（如 Sealos DevBox 类方案） | 路线 C：仅挂载 `/root`、`/workspace` PVC + Dockerfile 重建 | 路线 D：KubeVirt 虚拟机开发机 |
| --- | --- | --- | --- | --- |
| `apt`/`/usr`/`/etc` 系统修改保留 | **完整保留**（OverlayFS 覆盖全根文件系统） | 完整保留（停机前自动 commit 为快照镜像） | **丢失**（仅保留挂载目录） | 完整保留（整块 VM 根盘） |
| 关机速度 | **秒级**（直接卸载 PVC，无需打包上传整盘） | **慢**（每次关机都需扫描整个 rootfs 打包推仓库，动辄数分钟） | 秒级 | 秒级 |
| 跨节点重新调度启动速度 | **秒级**（基础镜像节点已有缓存/Spegel P2P + 块存储 PVC 直接 attach） | **慢**（新节点需重新拉取上一次关机临时生成的巨大快照镜像） | 秒级 | 较快，但有 Guest OS 内核启动开销 |
| 提交为标准 OCI 容器镜像 | **天然支持**（`upperdir` 本身就是相对于基础镜像的精确增量层，直接流式转 OCI 层追加） | 支持（节点 agent 调 `ctr/nerdctl commit` 并 push） | 无法捕获手工交互式修改 | **不支持**（VM 磁盘镜像无法直接作为容器镜像跑 `TrainTask`） |
| 宿主机安全边界 | **干净**（无需挂载宿主机 `containerd.sock`，无需节点特权 Agent，Commit Job 完全非特权） | **重**（需全节点高权限 DaemonSet 直连 containerd socket） | 最轻 | 需 KVM 虚拟化栈支持 |
| PyTorch / CUDA I/O 性能 | **原生内核级**（内核态 `overlay` + NVMe 块存储，零 FUSE 开销） | 原生内核级 | 原生内核级 | 存在 Virtio 虚拟化损耗 |

**结论**：结合 `bz1-dev1` 集群已具备高性能分布式块存储（Ceph RBD `block-nvme`、VAST Block `block-vast-cs`）及其对应的 CSI `VolumeSnapshotClass`（`csi-rbdplugin-snapclass`、`vast-snapshot-cs`），**路线 A** 同时兼顾了关机速度、跨节点调度弹性、I/O 性能、OCI 镜像直出能力与集群安全边界，是最契合机器学习平台的架构。

> **为什么不用「共享文件系统（NFS / VAST NFS / CephFS）+ `fuse-overlayfs`」承载根盘？**
> 1. **内核 `overlay` 协议硬约束**：NFSv3/v4（含 VAST NFS `shared-vast-cs`）不支持 `trusted.overlay.*` xattr 与内核 `RENAME_WHITEOUT`，Linux 内核直接拒绝将其作为原生 `overlay` 的 `upperdir`（`EINVAL`），若强用共享存储只能退化到用户态 `fuse-overlayfs`。
> 2. **守护进程单点故障（SPOF）**：`fuse-overlayfs` 是运行在容器内的用户态守护进程，一旦用户训练/预处理打满容器内存 Cgroup 触发 OOM 波及该进程或发生崩溃，整个容器根盘立即报 `Transport endpoint is not connected` 彻底瘫痪且无法恢复。
> 3. **NFS 语义冲突与元数据延迟放大**：NFSv3 删除打开中的文件会产生 `.nfsXXXX`（silly rename）残留，导致 `pip`/`conda`/`apt` 删除非空目录时偶发 `EBUSY / Directory not empty`；同时 `import torch` 数万次串行 `stat()` 经由 FUSE 上下文切换叠加网络 RTT 会导致启动与 IDE 索引显著变慢。
>
> 因此职责划分非常清晰：**根盘（`rootdisk`）坚持采用 `ReadWriteOnce` 块存储格式化 `ext4/xfs` 跑内核态 `overlay`**；海量数据集与多机共享目录则挂载 `ReadWriteMany` 共享文件系统（`shared-vast-cs` / `shared-juice`）。

---

## 3. 组件与部署拓扑

```text
┌──────────────────────────────────── devpilot-system ────────────────────────────────────┐
│  devpilot-controller-manager (Deployment, 2 replicas, leader election)                  │
│   ├─ DevBoxReconciler      协调 Pod、rootdisk PVC、Service、SSHRoute、HTTPRoute、计费段 │
│   ├─ DevBoxCommitReconciler 协调 VolumeSnapshot、克隆 PVC、隔离 Commit Job、可选 Rebase │
│   ├─ WebhookServer         准入校验与默认值填充（镜像摘要解析、规格合法性、不可变字段） │
│   └─ DashboardServer       轻量看板（实例状态、开关机、保存镜像操作入口，可选开启）     │
└──────────────────┬───────────────────────────────────────────────────┬──────────────────┘
                   │                                                   │
                   ▼                                                   ▼
┌────────────── 用户/项目命名空间 (<ns>) ──────────────┐ ┌────── 构建命名空间 (<ns>-build) ──────┐
│  DevBox CR: pytorch-dev                              │ │  Secret: registry-push (只对构建可见) │
│   ├─ PVC: pytorch-dev-rootdisk (RWO, block-nvme)     │ │  PVC: commit-xxx-snap (RO, 来自快照)  │
│   ├─ Pod: pytorch-dev (RuntimeClass: nvidia)         │ │  Job: commit-xxx                      │
│   │   ├─ init: devpilot-init 注入 /run/devpilot      │ │   └─ devpilot-commit (非特权 UID 65532)│
│   │   └─ main: overlay 切根 + sshd + jupyter + CMD   │ │       流式打包 upper/ → 推送 Harbor   │
│   ├─ Secret: pytorch-dev-ssh-hostkey (持久化主机密钥)│ └───────────────────┬───────────────────┘
│   ├─ Service: pytorch-dev (22 / 8888 / 6006)         │                     │ HTTPS OCI Distribution API
│   ├─ SSHRoute ──► sshgateway-system (端口 2222)      │                     ▼
│   └─ HTTPRoute / Ingress ──► 外部访问 (:7443)        │        Harbor / GHCR 容器镜像仓库
└──────────────────────────────────────────────────────┘
```

### 仓库交付物规划（M1+）

| 二进制 / 镜像 | 构建来源 | 职责 |
| --- | --- | --- |
| `devpilot-manager` | `cmd/manager` | Operator 控制器、Webhook 与可选看板，发布为 `ghcr.io/wutz/devpilot:<ver>` |
| `devpilot-init` | `cmd/devpilot-init`（`CGO_ENABLED=0` 静态编译） | 注入到用户开发机 Pod 内执行 OverlayFS 挂载、NVIDIA/PVC 子挂载迁移、`pivot_root` 及进程托管，零动态库依赖（兼容 Ubuntu/CentOS/Alpine 任意基础镜像） |
| `devpilot-commit` | `cmd/devpilot-commit` | 在非特权 Commit Job 中读取 `upper/`、转换 whiteout/opaque、通过 `go-containerregistry` 远端追加层推送到镜像仓库 |

---

## 4. 关键设计决策（D1–D12）

| # | 决策 | 理由 | 代价与约束 |
| --- | --- | --- | --- |
| **D1** | 使用内核态 `overlay` 而非 `fuse-overlayfs` | ML 场景 import PyTorch/Transformers 与编译算子涉及数十万次 `stat`/`open`，内核态 `overlay` 无用户态上下文切换损耗 | 容器需授予受控的 `CAP_SYS_ADMIN`（限制在私有 mount namespace 且禁止 `privileged` 与 `hostPath`） |
| **D2** | 直接以容器初始 `/` 作为 `lowerdir`，不依赖 K8s `ImageVolume` | 实测 `bz1-dev1`（K8s 1.32.5）中 `ImageVolume` 仍处于未启用的 Alpha 门控；利用容器自身 `/` 零集群配置依赖，且 Linux 4.19+ 原生支持栈深 `s_stack_depth = 2` | `devpilot-init` 必须在 `pivot_root` 前处理好初始 `/` 上的子挂载迁移 |
| **D3** | `pivot_root` 前自动扫描 `/proc/self/mountinfo` 递归 `rbind` 子挂载 | `RuntimeClass: nvidia` 的 OCI prestart hook 会将 `nvidia-smi`、`libcuda.so.*` bind-mount 到初始 `/`；OverlayFS `lowerdir` 不会透传子挂载，显式 `rbind` 才能保证 GPU 驱动与共享存储 PVC 100% 可见 | `devpilot-init` 需严格按挂载路径父子深度排序迁移，并排除 `/mnt/rootdisk` 本身防止循环挂载 |
| **D4** | 首次启动将基础镜像解析为不可变 `repo@sha256:...` 写入 `.meta/base-image.json` | 若底层基础镜像 tag（如 `:latest`）被覆盖更新，重启后 `lowerdir` 改变会导致已有 `upper/` 出现 OverlayFS 未定义行为或动态库损坏 | 用户若想升级基础镜像，必须选择「重置系统盘」或先 Commit 再切换 |
| **D5** | rootdisk PVC 强制使用 `ReadWriteOnce` 块存储（如 `block-nvme` / `block-vast-cs`） | OverlayFS 的 `upperdir` 与 `workdir` 要求底层文件系统完整支持 POSIX `rename`、`RENAME_WHITEOUT`（字符设备 `0:0`）及 `xattr`，NFS/部分共享文件系统不满足或元数据延迟高；块存储完美匹配单实例独占语义 | 在线热提交不能跨节点多挂读写，需借助 CSI `VolumeSnapshot` 克隆临时只读卷 |
| **D6** | 在线热提交采用 `sync` + CSI `VolumeSnapshot` → 临时只读 PVC → 独立 Commit Job | `bz1-dev1` 的 Ceph RBD 与 VAST Block 均已配置 `VolumeSnapshotClass`；COW 快照秒级完成，Commit Job 可调度到任意普通 CPU 节点并发压缩上传，完全不抢占 GPU 节点 CPU/内存与磁盘带宽 | 若某个集群的 StorageClass 不支持 CSI 快照，自动降级为同节点 `podAffinity` 只读挂载或要求停机离线提交 |
| **D7** | 镜像提交采用 `go-containerregistry` 远端 Manifest 追加而非本地 `docker/nerdctl commit` | 无需拉取动辄 15–30 GB 的 PyTorch/CUDA 基础镜像层，仅流式上传增量 `upper/` tar 流（通常几百 MB 到数 GB）并重写几 KB 的 Manifest/Config JSON，速度快一个数量级且无需特权 | 若多次反复 Commit 且不压缩，镜像层数会递增（通过可选 `rebase` 与层数阈值提醒控制） |
| **D8** | Commit Job 在隔离命名空间（`<ns>-build` 或 `devpilot-system`）运行 | 沿用 `imagebuild` 的安全模型：开发机用户对项目命名空间有 `pods/exec` 权限，隔离构建命名空间可确保镜像仓库 Push Secret 永不暴露给终端用户 | 跨命名空间访问快照时，由 Operator 在构建命名空间创建绑定只读快照源或临时 PVC |
| **D9** | `sshd` 运行在切根后的主容器内部，而非独立 Sidecar 容器 | `forgeai` 现有 Sandbox 的 `sshd` Sidecar 只能看到共享 PVC，看不到主容器内的 Python 环境、GPU 和正在运行的进程；开发机的核心诉求是 VS Code Remote-SSH 直连完整开发环境 | `devpilot-init` 负责在无 `sshd` 的精简镜像中兜底或按需托管内置轻量 SSH 服务，并将 HostKey 持久化在 K8s Secret 中保证重启不触发客户端 `WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED` |
| **D10** | 显式区分 `Stopped`（主动关机）与 `Interrupted`（抢占回收）两种非运行态 | 对齐 `ai-prd` §6 规范：抢占式实例被高优任务回收时转入 `Interrupted`，保留 rootdisk PVC 与共享盘，截断计费段并记录回收原因，累计连续停机时长（默认 15 天自动清理） | 状态机比普通 Deployment 多一个终态分支 |
| **D11** | 计费段（`status.segments[]`）与 `Money` 定点字符串模型对齐 `trainpilot` | 避免浮点数精度漂移；每次开机 `Running` 至停机/中断/释放生成一条闭合账单段，平台计费系统可直接拉取对账 | CRD Status 需限制历史段最大保留条数或归档摘要，防止超过 etcd 1.5MB 限制 |
| **D12** | 生态松耦合：核心 CRD 零外部强依赖，`sshgateway` / `workspace` 通过可选开关集成 | 既可在纯净 K8s 集群一键 Helm 安装使用，也能在部署了 `workspace`、`sshgateway` 的多租户智算平台上无缝自动创建 `SSHRoute` 和复用 `project-registry` | 控制器对可选外部 CRD 采用动态发现或配置开关，避免未安装时启动失败 |

---

## 5. 核心生命周期流程

### 5.1 创建与首次启动

1. 用户提交 `DevBox` CR（指定镜像、GPU/CPU/内存规格、系统盘大小与 StorageClass、共享存储 PVC、SSH 公钥 / Web 服务开关）。
2. `DevBoxReconciler`：
   - 解析并锁定初始镜像摘要（若未指定 `@sha256:...`，通过仓库或首次 Pod `containerStatuses[0].imageID` 固化到 `status.activeImageDigest`）；
   - 创建专属 rootdisk PVC（`<name>-rootdisk`，`ReadWriteOnce`，带 OwnerReference 随 `DevBox` 删除而回收，或配置进回收站）；
   - 生成持久化 SSH HostKey Secret（`<name>-hostkey`，整个 `DevBox` 生命周期不变）；
   - 创建 Service、可选 `SSHRoute` 与 Gateway `HTTPRoute` / Ingress（端口 `7443`）；
   - 创建开发机 Pod，`status.phase` 推进至 `Starting` → `Running`，开启首条计费段。

### 5.2 主动关机与跨节点再启动

1. 用户将 `spec.desiredState` 设为 `Stopped`（或触发闲置自动关机 `idleShutdownMinutes`）。
2. `DevBoxReconciler` 将阶段置为 `Stopping`，优雅终止 Pod（默认 `terminationGracePeriodSeconds: 30` 让容器内进程落盘退出），闭合当前计费段（`endReason: UserStopped` 或 `IdleTimeout`）。
3. Pod 删除后块存储 PVC 自动 detach，`status.phase` 变为 `Stopped`，记录 `status.stoppedAt`（启动 15 天停机保留倒计时）。
4. 用户改回 `spec.desiredState: Running`：Operator 使用锁定的 `status.activeImageDigest` 重新创建 Pod，调度器挑选任意空闲 GPU 节点，PVC 重新挂载，`devpilot-init` 挂载 `upper/` 恢复全部环境，开启新计费段并清零连续停机计时。

### 5.3 抢占式实例被回收（Spot Interruption）

1. 当抢占式（`billingMode: Spot`）开发机所在节点被高优任务抢占或触发驱逐（Pod 出现 Eviction / Preemption 或节点打上 draining 污点并删除 Pod）且 `spec.desiredState` 仍为 `Running` 时：
2. Operator 不会盲目原地无限拉起，而是闭合计费段（`endReason: Preempted`），将 `spec.desiredState` 同步收敛或保持状态为 `Interrupted`，记录 `status.interruptedAt` 并发送 K8s Event。
3. rootdisk PVC 与共享盘完整保留；用户后续手动点击启动时，若目标分区有空闲库存则恢复 `Running`，无库存则提示资源不足并继续保留 `Interrupted`。

### 5.4 提交环境为新镜像（Commit & Push）

1. 用户创建 `DevBoxCommit` CR，指向目标 `DevBox` 并填写目标镜像仓库与 Tag（如 `pytorch-2.4-flashattn:v1`）。
2. 若 `DevBox` 处于 `Stopped`：Operator 直接将 rootdisk PVC 只读挂载给 Commit Job。
3. 若 `DevBox` 处于 `Running`：Operator 先在容器内触发文件系统 `sync`，随后创建 CSI `VolumeSnapshot`，待 `readyToUse` 后恢复出临时只读 PVC 挂载给 Commit Job。
4. `devpilot-commit` 流式扫描 `upper/`、转换 whiteout、过滤临时文件与密钥、上传增量层并追加 Manifest 到 Harbor，回写 `status.digest` 与 `status.images`。
5. 若设置了 `spec.rebaseDevBox: true`，Operator 在镜像推送成功后标记 `DevBox` 待 Rebase：在下一次重启时自动把底层镜像切换为新镜像 digest 并清空 `upper/`。
