# 共享存储承载可选 `disk.img` 根盘架构设计与实现说明

> **状态**：**已实现（实验性可选功能，Helm Chart 中 `loopCSI.enabled` 默认为 `false`）**
> **关联组件**：[`DevBoxReconciler`](../internal/controller/devbox_controller.go)、[`DevBoxCommitReconciler`](../internal/controller/devboxcommit_controller.go)、`devpilot-init`（[`cmd/devpilot-init`](../cmd/devpilot-init/main.go)）、`devpilot-commit`（[`cmd/devpilot-commit`](../cmd/devpilot-commit/main.go)）、[`devpilot-loop-csi`](../cmd/devpilot-loop-csi/main.go)、[`internal/loopdev`](../internal/loopdev)、[`internal/loopcsi`](../internal/loopcsi)、[`internal/ext4`](../internal/ext4)
> **目标存储后端**：IBM Storage Scale（GPFS）、VAST Data（NFSv4.1 / NFSv4.2 / RDMA）、CephFS

---

## 1. 核心结论与定位边界（Executive Summary）

### 1.1 可行性结论：工程上完全可行，且已打通核心代码链路

在缺乏独立分布式块存储 CSI（或单节点云盘/块设备 Attach 数量受限）但具备共享并行文件系统（**IBM Storage Scale / GPFS、VAST Data、CephFS**）的 Kubernetes 集群中，**将每个 `DevBox` 的持久化根盘封装为共享存储池上的独立稀疏镜像文件 `disk.img`（内部格式化为 `ext4`），在宿主机侧通过内核 Loopback（开启 `LO_FLAGS_DIRECT_IO`）挂载并向容器暴露标准持久化目录，在容器内运行内核态 `overlay` 承载 `upperdir` 与 `workdir`**——该方案已完成完整编码实现。

这一范式（File-backed Loopback Image on Parallel FS）与超算领域 Singularity/Apptainer `--overlay disk.img` 及虚拟化文件系统盘同源，现已作为 DevPilot 的可选 CSI 后端（驱动名 `loop.devpilot.wutz.dev`）落地。

### 1.2 核心收益

相比「直接在共享存储目录上跑 OverlayFS / `fuse-overlayfs`」，`disk.img` 方案同时解决了语义、性能、隔离与回收四类问题：

1. **完整满足内核态 `overlay` 文件系统契约**：上层内核 `overlay` 始终面对标准本地 `ext4`，原生支持 `d_type`、`trusted.overlay.*` 扩展属性、`0:0` 字符设备 whiteout、原子 `RENAME_WHITEOUT` 与 POSIX 硬链接，彻底避开 NFS silly rename（`.nfsXXXX`）或 `EINVAL` 拒挂问题。
2. **元数据操作本地化收敛**：`apt install`、`conda`、`uv/pip` 解包与构建过程中大量小文件的 inode/dentry 创建与 `stat`/`unlink`/`rename`，全部在计算节点本机内核 `ext4` 页缓存、dcache/icache 与 `jbd2` 中完成，向底层共享存储转化为对单个大文件 `disk.img` 的 4 KiB 对齐块读写（Data I/O），大幅缓解并行存储元数据服务器（MDS / Metanode / CNode）压力。
3. **天然硬性容量与 Inode 隔离**：固定逻辑大小的 `disk.img` 由内部 `ext4` 超块硬性封顶总容量与 Inode 数量，无需依赖底层共享存储配置目录级 Project Quota 即可防止单实例写爆全局池。
4. **O(1) 整卷删除清理**：删除根盘或执行停机 `reset-rootdisk` 时，在共享存储侧只需删除单卷目录下的 `disk.img` 等少数文件，无需递归 `unlink` 数十万小文件。

### 1.3 架构定位：**可选互补后端，而非替代原生块存储**

- **已具备高性能块存储 CSI 的集群（如 Ceph RBD `block-nvme`、VAST Block `block-vast-cs`）**：**继续优先使用原生块存储**。原生块设备链路更短、存储侧原生具备单写者独占锁，无需部署额外节点 Loop CSI 组件。
- **纯共享存储集群或受限于单机块设备挂盘配额的集群**：启用 `loopCSI.enabled=true`，使 DevPilot 在保持容器**非特权安全边界**与**内核态 `overlay`** 的前提下跑通全生命周期。

---

## 2. 已验证范围与未验证范围（Verification Status）

为避免将本地验证外推为生产集群结论，当前交付状态严格区分如下：

| 验证维度 | 状态 | 覆盖内容与具体证据 |
| --- | --- | --- |
| **Go 静态检查与单元测试（含 `-race`）** | **已通过** | `make lint-fix`（`golangci-lint` 0 告警）、`make test` 全量通过：`internal/ext4`（`clean`/`legacy`/`features`/`tiny`/`dirty`/`dirty-legacy` 夹具、内存 jbd2 重放及 fuzz 种子）、`internal/commit`（`Ext4Source` 单元测试）、`cmd/devpilot-commit`、`internal/loopcsi`（CSI Controller/Node/租约/快照单测）、`internal/initroot`（freeze/thaw 单测）、`internal/controller`（envtest 下 Sync→Freeze 自动提升、Freeze 超时/异常删除快照等）。 |
| **Linux 交叉编译与 `vet`** | **已通过** | `GOOS=linux go vet ./...` 与 `GOOS=linux go build ./...` 全量通过（涵盖所有 `//go:build linux` 系统调用代码）。 |
| **真实 Linux 内核 Loop E2E** | **已通过** | 通过 [`hack/loop-csi-e2e.sh`](../hack/loop-csi-e2e.sh) 在特权容器内以 Docker 宿主机 `ext4` 后端盘运行 [`TestE2ERealKernel`](../internal/loopcsi/e2e_linux_test.go) 与 [`TestLinuxAttach`](../internal/loopdev/loopdev_linux_test.go)（含 GPFS 守卫改动后重跑通过）：实测真实 `/dev/loop*` 绑定、`DirectIO=true` 回读校验、OFD 写锁拒绝第二写者与 `raw` 读、ext4 MMP 约 21s 阻塞拒挂、在线 `NodeExpandVolume`（1 GiB → 2 GiB）、`FIFREEZE` → `CreateSnapshot` → `FITHAW`、`raw` 只读发布后由 `ext4.OpenFile` + `commit.WriteLayer(&commit.Ext4Source{})` 正确打包增量文件/`0:0` whiteout/`trusted.overlay.opaque`/`.meta/base-image.json`、卸载 `FITRIM` 回收（`66988 KiB -> 1388 KiB`）以及跨节点重新挂载。 |
| **Helm Chart 语法与模板渲染** | **已通过** | `make helm-lint`、`helm package` 与 `make lint-config` 通过；开启 `loopCSI.enabled=true` 的 `helm template`（覆盖 `omitted`、`off`、`false`、`"off"`、`preferred`、`required`）渲染全部通过。 |
| **真实集群 + VAST NFSv3 池（2 个 GPU 节点）** | **已通过（12/12）** | 用 [`hack/loop-csi-cluster-verify.sh`](../hack/loop-csi-cluster-verify.sh) 在开发集群的两个 GPU 节点上部署驱动，池目录是两节点共同挂载的 VAST NFSv3 子目录（`vers=3,nconnect=16,hard`，脚本会校验两节点挂载源一致并跨节点读回标记文件，否则拒绝运行）。通过项：创建稀疏 `disk.img`、挂载并写入 256 MiB 与 500 个小文件、在线扩容 2Gi → 4Gi、第二节点挂载被驱动以 `FailedPrecondition ... is in use` 拒绝、快照后恢复内容摘要一致、卸载后跨节点接管内容摘要一致。 |
| **同一次验证中的未覆盖项** | **未验证** | 未测：节点断网后的租约驱逐、网络抖动下 `hard` 挂载的行为、吞吐与延迟（本次只做功能验证，没有性能数据）、`DevBoxCommit` 的 `ExecFreezer` 全链路、`FICLONE`。T4 只证明第二写者被拒，没有区分是 OFD 锁、租约还是 MMP 起的作用。 |
| **卸载后 `FITRIM` 回收（VAST NFSv3）** | **未回收** | 卸载前后 `disk.img` 实际占用同为 308994048 字节（文件大小 4294967296），NFSv3 池上空间不会随 `FITRIM` 归还。容量规划要按高水位计。 |
| **GPFS 池（`directIO: required` 崩溃定位与防护）** | **已根因定位并完成运行时防护** | 在 GPFS（`/fs1`，IBM Storage Scale 5.2.3.5，内核 5.15.0-94-generic）上首次尝试以 `O_DIRECT` 发布 loop 卷时，两台节点触发内核空指针崩溃并保留 ERST pstore 记录：内核 `loop` 工作线程（`current->mm == NULL`）经 `loop_workfn` → `lo_rw_aio` 发起异步 `O_DIRECT` I/O，命中 `mmfs26` `gpfs_f_aio_{read,write}_internal`（`file.c:2769/2911`）进入 `CXI_READ_AIO`（`RSI=0x400`），而 `rdwrInternal`（`file.c:2003`）因 `current->mm == NULL` 跳过 `cxiMapUserBuffers`，随后 `dioRdWr + 0x58c` 在 AIO 路径直接解引用未初始化的 `pageListsP->pageListPP[0]` 导致 `Kernel panic`。现已在 [`loopdev`](../internal/loopdev/loopdev_linux.go) 与 [`loopcsi`](../internal/loopcsi/node.go) 落地 `statfs(0x47504653)` 守卫：禁止在 GPFS 上以 `O_DIRECT` 或 `LO_FLAGS_DIRECT_IO` 打开/绑定 loop 设备，`directIO: required` 直接返回 `FailedPrecondition`，`directIO: preferred` 记录告警并安全回退为缓冲 loop。 |
| **GPFS 池（单节点与跨节点全套实测）** | **单节点 + 跨节点均实测通过** | 在 GPFS 节点 `gn-10-243-145-103` 与 `gn-10-243-145-106`（Scale 5.2.3.5 + Linux 5.15.0-94-generic，`vfs_iter_read` 路径，不受 `IJ60020` 影响；控制器在 `mn-10-243-144-51`）上使用 `verify3-1118` 镜像对 `/fs1/devpilot-loop-verify` 完成 8 项实测：**T1** `directIO: required` 被 `FailedPrecondition` 干净拒绝且节点保持 `Ready`；**T2** `directIO: preferred` 自动回退至 `/dev/loop7`（`DIO=0`）正常读写与卸载；**T3** `directIO: off`（同步验证 Helm YAML 1.1 修复）完成稀疏创建（初始 32 MiB）、256 MiB + 500 小文件 + symlink、在线扩容 `2Gi → 4Gi`、`VolumeSnapshot` + 恢复摘要一致（`50e4a505...`）及 `FITRIM` 回收（`322961408` → `50331648` 字节）；**T4** 跨节点 RW 与 `ModeRaw` 冲突均被 `FailedPrecondition` 拒绝；**T5** 重启 `103` 上 `loop-csi-node` 后插件恢复状态（`Recovered read-write volume`），跨节点 OFD 锁仍生效且租约心跳恢复续期；**T6** 卸载后 `106` 成功接管且摘要一致（`4c5c990f...`），`ModeRaw` 在 `103` 零 loop 设备暴露 `/data/disk.img` 并通过 `F_RDLCK` 互斥 `106` 写者；**T7** 无锁态写伪造活跃租约被 `live lease` 阻塞并在 45s TTL 过期后自动接管；**T8** 在 `256Mi` 容器内存上限下写入 `1.5 GiB` + 1000 小文件并校验 SHA-256 全部 `OK`，挂载维持 `rw`，`dmesg` 无报错。**仍未覆盖**：长周期/全节点级别高内存压力回收压测、`DevBoxCommit` CR 经 `ExecFreezer` 触发 GPFS 快照的全链路、以及 Option B（NBD）编码实现。 |
| **CephFS 池** | **未验证** | 开发集群的 StorageClass 中只有 Ceph RBD，没有 CephFS，本次没有测 CephFS；`.snap` 快照策略仅有单元测试。 |
| **服务端协议策略** | **已确认** | 该 VAST 服务端拒绝 NFSv4.x，只能使用 NFSv3；NFSv3 下 `O_DIRECT` 的 loop 挂载与跨节点 OFD 锁拒绝已在上面的 12 项中实测可用。 |

---

## 3. 路径对比：为什么选择 `disk.img` + 宿主机 Loop CSI

| 评估维度 | 路径 1：共享目录直接做 `overlay` `upperdir` | 路径 2：共享目录 + 容器内 `fuse-overlayfs` | 路径 3：`disk.img`（`ext4`）+ `LO_FLAGS_DIRECT_IO` + 内核 `overlay`（**已实现**） |
| --- | --- | --- | --- |
| **NFS / VAST NFS 兼容性** | **不可行**：内核 `overlay` 校验 `RENAME_WHITEOUT` 与 `trusted.*` xattr 返回 `EINVAL` | 可挂载，但受 NFS silly rename（`.nfsXXXX`）干扰，包管理器删改活跃文件偶发 `EBUSY` | **兼容**：NFS 仅承载单个 `disk.img` 的 4 KiB 块级读写 |
| **GPFS / CephFS 兼容性** | 受限于 `trusted.*` xattr 透传及跨节点目录语义，且重放元数据 RPC 放大延迟 | 可挂载 | **兼容** |
| **用户态守护进程单点故障（SPOF）** | 无 | **高风险**：容器 Cgroup OOM 杀掉 `fuse-overlayfs` 后，整个 `/` 报 `ENOTCONN` 不可恢复 | **无用户态 SPOF**：`overlay` → `ext4` → `loop` → 存储客户端全链位于内核态 |
| **小文件安装与树操作特性** | 每次文件/目录创建与 `unlink` 直击远程 MDS/Metanode | FUSE 用户态上下文切换与远程元数据 RTT 叠加 | **元数据全本地化**：`ext4` 内存页缓存与 dcache 处理绝大多数小文件操作，合并为异步块写 |
| **单实例容量与 Inode 硬限额** | 依赖存储侧目录 Quota | 依赖存储侧目录 Quota | **`disk.img` 内部 `ext4` 天然硬隔离** |
| **Commit 隔离读取复杂度** | 低 | 低 | **高**：已通过 `devpilot-commit` 内置纯 Go `ext4` 用户态只读解析器彻底消除循环挂载需求 |

---

## 4. 稳定性分析：五大底层风险与代码级防线

把文件系统镜像放在网络共享存储上通过 `loop` 挂载，核心风险在于**并发双写损坏、内存回收死锁与高水位膨胀**。以下为代码中实际落地的工程防线。

### 4.1 风险一：多节点并发双挂载脑裂（Split-Brain）导致 `ext4` 损毁

共享存储本质上是 `ReadWriteMany`，而可写 `ext4` 严禁任何时刻存在两个写者。除 `DevBoxReconciler` 保持确定性单 Pod 生命周期外，[`NodeServer.publishReadWrite`](../internal/loopcsi/node.go) 与 [`ControllerServer`](../internal/loopcsi/controller.go) 实现了**三层单写者围栏**：

1. **护栏 1 — 直接持在 `disk.img` 上的 OFD 独占写锁（`F_OFD_SETLK`）**：
   - [`lock_linux.go`](../internal/loopcsi/lock_linux.go) 直接对打开的 `disk.img` 文件描述符调用 `fcntl(F_OFD_SETLK, F_WRLCK)`（只读 `raw` 发布则申请共享读锁 `F_RDLCK`）。
   - **关键设计**：在调用 `LOOP_CONFIGURE` / `LOOP_SET_FD` 时，将这个**已持锁的 open file description** 交给内核 `loop` 驱动引用，随后节点插件关闭自己的用户态 fd（详见 [`AttachFile`](../internal/loopdev/loopdev_linux.go)）。因为 Linux OFD 锁绑定在内核 `struct file`（open file description）而非进程 PID 上，只要 `loop` 设备处于挂载状态，**即使 `devpilot-loop-csi` Pod 重启或升级，跨节点独占写锁也持续有效**；当卸载触发 `LO_FLAGS_AUTOCLEAR` 解绑时，锁由内核自动释放。不再额外维护独立的 `disk.img.lock` 文件。
   - **存储协议依赖**：要求 CephFS（MDS 会话锁）、GPFS（分布式 Token 锁）与 VAST（**必须使用 `nfsvers=4.1` 或 `4.2`，严禁 NFSv3 或 `nolock`**）正确向上游锁管理器传播 `fcntl` 字节范围锁。
2. **护栏 2 — 旁路心跳租约文件（`disk.img.lease`）**：
   - [`lease.go`](../internal/loopcsi/lease.go) 以原子临时文件 + `rename` 方式每 `leaseInterval`（默认 `10s`）刷新 `disk.img.lease`（记录 `node`、`bootID`、`volumeID`、`device`、`acquired`、`heartbeat`）。
   - 若因误配 `nolock` 或 NFS 服务端故障切换未恢复锁状态导致锁失效，新节点在 `heartbeat` 距当前小于 `leaseTTL`（默认 `45s`）时立即以 `FailedPrecondition` 拒绝挂载。
   - **时钟前提**：租约超时判定依赖节点间时钟同步（**需部署 NTP/chrony**），`45s` TTL 留有裕度以吸收常规时钟偏差。
3. **护栏 3 — `ext4` 内置 MMP（Multi-Mount Protection）**：
   - 创建镜像时默认启用 `-O ...,mmp -E ...,mmp_update_interval=5`（可通过 StorageClass 参数 `mmp: "false"` 关闭）。
   - 在极端情况下即便前两道锁均被绕过，内核挂载读写 `ext4` 前会检查并等待约 `2 * 5 + 1` 秒以上（实测 E2E 中约 `21s`）确认无其他节点更新 MMP 块，检测到活跃写入即报错拒绝挂载。

> **残留脑裂窗口说明**：三层围栏已覆盖绝大多数管理网分区与插件重启场景；但在极端场景（例如底层挂载配置了 `nolock` **且** 节点时钟严重漂移超出 `leaseTTL` **且** 存储/节点半死导致 MMP 块停止刷新但仍在发包）下，仍需依赖基础设施级节点 Fencing（如 IPMI/BMC 断电或存储客户端踢除）。

### 4.2 风险二：双重页缓存（Double Caching）与内存回收死锁（`PF_MEMALLOC`）

- **机制**：若 `loop` 工作在缓冲模式，同一数据会同时驻留上层 `ext4` Page Cache 与下层网络文件系统 Page Cache；内存紧张时 `kswapd` 经 `ext4` → `loop` → 网络文件系统回写脏页，网络栈申请内存又反向阻塞内存回收，可能引发节点卡死。
- **实现防线（[`loopdev_linux.go`](../internal/loopdev/loopdev_linux.go)）**：
  1. [`OpenBacking`](../internal/loopdev/loopdev_linux.go) 优先尝试以 `O_RDWR | O_DIRECT` 打开 `disk.img`（若底层文件系统在 `open` 阶段返回 `EINVAL`，回落常规打开并继续依赖 `LO_FLAGS_DIRECT_IO`）。
  2. `mkfs.ext4 -b 4096` 强制 4 KiB 块大小，[`AttachFile`](../internal/loopdev/loopdev_linux.go) 使用 `LOOP_CONFIGURE` 显式设置 `Size = 4096` 与 `LO_FLAGS_DIRECT_IO | LO_FLAGS_AUTOCLEAR`（5.8 之前老内核回退 `LOOP_SET_FD` + `LOOP_SET_BLOCK_SIZE` + `LOOP_SET_DIRECT_IO`）。
  3. **拦截静默降级**：Linux 内核在对齐不满足时会静默剥离 `LO_FLAGS_DIRECT_IO` 而不返回错误。[`AttachFile`](../internal/loopdev/loopdev_linux.go) 绑定后立即调用 `LOOP_GET_STATUS64` 回读 `Flags`，在默认策略 `directIO: required` 下若发现 `DirectIO == false`，立即执行 `LOOP_CLR_FD` 清理并返回 `ErrDirectIOUnavailable`（CSI 侧转为 `FailedPrecondition`，并提示可显式设为 `preferred`）。

### 4.3 风险三：GPFS 的双重冲突（`mmfs26` 内核 Direct I/O 崩溃 + 高版本内核 APAR `IJ60020`）

1. **已定位的 `mmfs26` Direct I/O 内核空指针崩溃（IBM Storage Scale `5.2.3-5` + Linux `5.15.0-94-generic`）**：
   - 经对节点 `/usr/lpp/mmfs/src/gpl-linux/` GPL 源码与 `/lib/modules/5.15.0-94-generic/extra/mmfs26.ko` 反汇编核验，完整崩溃链为：
     1. 内核 `loop` 在 `kworker` 线程（`current->mm == NULL`）经 `loop_workfn` → `lo_rw_aio` 发起异步 `kiocb` 读写（`ITER_BVEC`）；
     2. 当 `disk.img` 以 `O_DIRECT` 打开（`vinfoP->viIsDIO = true`）且未设 `O_SYNC`、`iov_count == 1` 时，`gpfs_f_aio_read_internal` / `gpfs_f_aio_write_internal`（`file.c:2769` / `2911`）走入 `CXI_READ_AIO` / `CXI_WRITE_AIO`（传入 `CXI_RDWROPTS_AIO = 0x400`，与两次崩机现场寄存器 `RSI: 0000000000000400` 完全一致）；
     3. `rdwrInternal`（`file.c:2003`）仅在 `vinfoP->viIsDIO && (current->mm != NULL)` 时调用 `cxiMapUserBuffers`，在 `kworker` 上直接跳过，留下 `pageLists = {.plCount = 0, .pageListPP = NULL}`；
     4. `mmfs26.ko` 中 `_Z7dioRdWr... + 0x58c` 的 AIO 分支未校验 `pageListsP->plCount > 0` 直接解引用 `pageListsP->pageListPP[0]`，立即触发 `BUG: kernel NULL pointer dereference` 与整机 panic。
   - **为什么 `O_SYNC` 也不能启用 `O_DIRECT`**：即使跳过 `CXI_READ_AIO`，`dioRdWr` 仍会调用 `Buffer::attachUio` → `cxiMapUserBuffer`（`cxiIOBuffer.c:1013`），后者在 `current->mm == NULL` 时直接返回 `-ENOMEM`。
   - **为什么早期手动回放未复现**：仅对 `disk.img` 做 `open`/`flock` 而不绑定 `/dev/loop*` 挂载 `ext4`，以及仅使用 `losetup --direct-io=on`（不向 `open()` 传 `O_DIRECT`），均不会触发 `viIsDIO && !is_sync_kiocb`。

2. **高版本内核缓冲 Loop 缺陷（IBM APAR `IJ60020` / Doc `7289248`）**：
   - 在合入上游提交 `f2fed441c69b`（`"loop: stop using vfs_iter_{read,write} for buffered I/O"`，覆盖 `6.15+`、`6.12.25+`、Ubuntu `6.14.4+`、RHEL `5.14.0-597+`、SLES `6.4.0-150700.53.6+`）的内核上，Scale `< 5.2.3.10` / `< 6.0.1.2` 的缓冲 `loop` 存在数据损坏风险。
   - 开发集群 `bz1-dev1` 当前为 Ubuntu `5.15.0-94-generic`（`/proc/kallsyms` 确认仍走 `vfs_iter_read`），因此 **单节点在关闭 `directIO` 时可安全使用 GPFS Pagepool 缓冲 `loop`**（`gpfs_f_rdwr_iter` 走 `kmap(bv_page)` + `UIO_SYSSPACE`，不会进入 `dioRdWr`）。

3. **已落地的运行时 GPFS 防线与降级策略（[`loopdev.go`](../internal/loopdev/loopdev.go)、[`loopdev_linux.go`](../internal/loopdev/loopdev_linux.go)、[`node.go`](../internal/loopcsi/node.go)）**：
   - [`OpenBacking`](../internal/loopdev/loopdev_linux.go) 在 `dio=true` 时先通过 `statfs` 校验魔数 `GPFSSuperMagic (0x47504653)`，命中 GPFS 直接拒绝以 `O_DIRECT` 打开并返回 [`ErrGPFSDirectIOUnsafe`](../internal/loopdev/loopdev.go)。
   - [`AttachFile`](../internal/loopdev/loopdev_linux.go) 通过 `Fstatfs` 与 `fcntl(F_GETFL)` 二次兜底校验：任何携带 `O_DIRECT` 的 GPFS 描述符一律拒绝绑定；未带 `O_DIRECT` 时，[`validateGPFSDirectIOMode`](../internal/loopdev/loopdev.go) 仅放行 `preferred`/`off`（强制 `wantDIO = false`），拒绝 `required`。
   - [`NodeServer.publishReadWrite`](../internal/loopcsi/node.go)：`directIO: required` 返回 `FailedPrecondition`；`directIO: preferred` 记录明确告警（提示仅在不受 `IJ60020` 影响的内核安全）并自动降级为 `directIO: off` 重试；`directIO: "off"` 直接走缓冲模式。

4. **后续演进方向（Option B & Option C）**：
   - **Option B — 用户态 `O_DIRECT` NBD 块设备（中长期可实现）**：若未来升级至 Linux `6.14+` 或需要在 GPFS 上彻底消除双重页缓存，可引入用户态 NBD 服务端（集群节点已加载 `nbd` 模块）以普通用户态进程（持有合法 `current->mm`）对 `disk.img` 执行同步 `pread`/`pwrite` + `O_DIRECT` 并通过 `/dev/nbdX` 挂载 `ext4`，同时规避 `mmfs26` 内核线程空指针、缓冲双重缓存与 `IJ60020`。工程代价为需管理每卷 NBD 常驻进程生命周期、崩溃恢复与持锁语义。
   - **Option C — IBM 官方 PMR 修复材料**：若向 IBM Storage Scale 提交 PMR，可附带 OS/内核（Ubuntu 22.04.5 / `5.15.0-94-generic`）、Scale 版本（`5.2.3-5`）、触发条件（`loop_workfn` → `lo_rw_aio` + `O_DIRECT` + `current->mm == NULL` + `!O_SYNC` + `iov_count == 1`）、源码位置（`file.c:2003, 2769, 2911`；`cxiIOBuffer.c:1013`）及汇编崩溃点（`_Z7dioRdWr... + 0x58c` 处 `RSI=0x400` 访问空 `pageListsP->pageListPP`）。

### 4.4 风险四：存储网络瞬断与 `ext4` 错误修复

- **挂载参数**：底层共享存储务必使用 `hard` 挂载（禁止 `soft`），避免网络抖动直接向 `loop` 抛 `EIO`。内层 `ext4` 挂载固定附带 `noatime,errors=remount-ro,noinit_itable`（[`ext4MountOptions`](../internal/loopcsi/node.go)）：遇到不可恢复 I/O 错误时只读保护防止元数据继续损毁；`NodeGetVolumeStats` 检测到只读重挂时通过 `VolumeCondition{Abnormal: true}` 上报异常。
- **自动修复**：下次 [`NodeServer.publishReadWrite`](../internal/loopcsi/node.go) 打开镜像前检查超级块 `Errors` 标志，若存在错误记录则自动执行 `e2fsck -p disk.img` 修复后再挂载。

### 4.5 风险五：稀疏高水位膨胀与打洞回收（`FITRIM`）

- **镜像创建**：[`createImage`](../internal/loopcsi/image.go) 创建稀疏文件并执行 `mkfs.ext4 -F -q -b 4096 -m 0 -O ^orphan_file[,mmp] -E lazy_itable_init=1,lazy_journal_init=1,nodiscard`（在 E2E 中 1 GiB 稀疏镜像初始物理占用约 `668 KiB`，GPFS 4 MiB 块大小下 2 GiB 初始占用约 `32 MiB`）。
- **禁用实时 `-o discard`**：不在 `ext4` 挂载期开启实时 `discard`，避免用户大量 `rm` 小文件时同步触发海量远端打洞 RPC 拖慢日常交互。
- **卸载期批量 `FITRIM`**：当 `trimOnUnmount: true`（默认开启）时，[`NodeServer.teardown`](../internal/loopcsi/node.go) 在卸载前先确保 `Thaw`，随后执行 `ioctl(FITRIM)`（默认最小范围 `trimMinLength = 1 MiB`）。若底层共享存储不支持 `FALLOC_FL_PUNCH_HOLE`（如 NFSv3/NFSv4.1）导致失败，仅记录信息日志并继续正常卸载，不影响停机流程；在 GPFS `/fs1` 上实测 `FITRIM` 可正常打洞归还空间（`322961408` 字节 → `50331648` 字节）。
- **空间回收边界（`rebaseDevBox` vs `reset-rootdisk`）**：
  - `DevBoxCommit.spec.rebaseDevBox: true` 在下次开机时调用 [`initroot.RebaseUpper`](../internal/initroot/baseimage.go) 清理已有 `disk.img` 内的 `upper/`、`work/`、`merged/`，并未删除原 `disk.img`；因此在**支持打洞的后端**（如 CephFS / GPFS / NFSv4.2）上可通过随后一次卸载 `FITRIM` 回收空间，而在**不支持打洞的后端**（如 NFSv3 / NFSv4.1）上，彻底将高水位物理占用归零需在停机态使用 `devpilot.wutz.dev/action: reset-rootdisk` 注解（控制器直接删除 rootdisk PVC，下次开机重建全新稀疏 `disk.img`）。

---

## 5. 性能特征与三大共享存储对比（定性评估）

> **注意**：下表基于各存储系统的内核 I/O 路径与协议机制分析；受限于当前未在真实 GPFS / VAST / CephFS 集群运行性能基准压测，**具体吞吐与耗时需在目标集群 PoC 阶段实测验证**，此处不预设具体百分比数字。

### 5.1 典型 AI 开发机负载的 I/O 路径差异

| 开发机操作场景 | 共享目录直挂 / `fuse-overlayfs` 路径特征 | `disk.img`（`ext4` + `loop`）路径特征 |
| --- | --- | --- |
| **`import torch, transformers`（热缓存）** | 存在 FUSE 用户态/内核态上下文切换，或受限于远程属性缓存有效期（`actimeo`）引发反复 `GETATTR` | 完全命中节点本机内核 `overlay` + `ext4` Page Cache 与 dcache，无网络与用户态切换 |
| **`import torch`（冷启动首次读）** | 大量独立小文件引发串行 `LOOKUP`/`OPEN`/`READ` 元数据 RPC | 经 `ext4` 块层预读（readahead）连续拉取 `disk.img` 块区间，大幅减少 RPC 次数 |
| **`conda` / `uv pip` 安装大型环境** | 数万个小文件的创建、写、关闭、`rename` 全部转化为串行远程 MDS/CNode/Metanode RPC，是共享存储最弱环节 | 所有目录/inode 变更在节点本地 `ext4` 内存完成，合并为连续或批量的块写与少量 `jbd2` 提交 |
| **CUDA 算子并行编译（`ninja`/`nvcc`）** | 临时 `.o` 文件反复建删直接冲击远程元数据服务 | 临时文件多在本地 `ext4` 页缓存周期内被消费或删除，不触发远端元数据操作 |
| **停机刷盘与快照克隆** | 依赖整个共享目录树操作或子卷快照；删卷需遍历 `unlink` | 卸载仅刷新 `ext4` 脏块；`CreateSnapshot` 在支持 `FICLONE` 或 CephFS `.snap` 的后端上为元数据指针级操作，在无 reflink 后端上需执行稀疏复制 |

### 5.2 GPFS vs VAST Data vs CephFS 特性与适配矩阵

> 下表中的协议支持情况与版本建议（如 VAST NFSv4.2 `FICLONE`、GPFS 跨节点锁与 `IJ60020` 修复版本、CephFS `.snap` 行为）**需在目标集群 PoC 时逐项实测确认**。

| 对比维度 | IBM Storage Scale（GPFS） | VAST Data（NFS） | CephFS（内核 `ceph.ko`） |
| --- | --- | --- | --- |
| **客户端传输栈** | 专有内核模块 `mmfslinux` | Linux NFSv3 / NFSv4.1 / v4.2 客户端（建议开启 `hard`、`nconnect` / RDMA） | Linux 原生内核客户端 `ceph.ko`（不建议用 `ceph-fuse` 承载池） |
| **`LO_FLAGS_DIRECT_IO`** | **禁止开启**：`5.2.3-5 + 5.15.0-94` 触发 `mmfs26` 内核 panic；驱动守卫默认拒绝 `required`，仅支持 `directIO: "off"`（或 `preferred` 降级）走缓冲模式（需确认内核不受 `IJ60020` 影响） | 支持（VAST NFSv3 已实测 `DIO=1` 稳定通过） | 支持（依赖服务端/客户端块对齐） |
| **`F_OFD_SETLK` 跨节点锁** | **已实测可用**（GPFS 分布式 Token 锁 + 45s 租约已跨 `103`/`106` 实测 RW/ModeRaw 互斥、重启自愈与过期接管） | **已实测可用**（VAST NFSv3 + NLM + 租约 + MMP 已跨两节点实测通过；生产仍推荐 `nfsvers=4.1/4.2` 且禁用 `nolock`） | 支持 MDS 会话锁 |
| **`FITRIM` 打洞回收** | **已实测支持**（`/fs1` 上 `308 MiB → 48 MiB`） | NFSv3 / NFSv4.1 不回收（已实测）；NFSv4.2 取决于服务端 `DEALLOCATE` | 支持（映射为 RADOS 对象回收，已在内核路径契合度最高） |
| **在线 `CreateSnapshot` 策略** | 无 Linux `FICLONE`，走 `strategy: auto/copy`（配合 Freeze，已在 `/fs1` 单节点实测通过） | 无 `FICLONE` 时走稀疏 `copy`（已在 VAST NFSv3 实测通过）；NFSv4.2 支持 `CLONE` 时可走 `reflink` | 优先配置 `strategy: cephfs-snapdir`（在卷目录下创建 `.snap/<name>` 毫秒级目录快照） |
| **需重点核查事项** | 必须配置 `directIO: "off"` 且核查内核不受 `IJ60020` 影响（若升级至 `6.14+` 需先升级 Scale 至 `>=5.2.3.10`/`6.0.1.2` 或采用 NBD 方案）；调大 `--commit-freeze-timeout` 适配大镜像拷贝 | 核查挂载参数是否 `hard`（严禁 `soft`/`nolock`）；NFSv3 池按高水位做容量规划 | 建议池底层为全闪 NVMe OSD，确认集群允许客户端执行 `mkdir .snap/...` |

---

## 6. Kubernetes 架构落地设计（非特权边界与 CSI 模型）

### 6.1 为什么不在容器内直接 `losetup`

DevPilot 要求所有用户 DevBox Pod 与 `DevBoxCommit` Build Job 保持 `privileged: false`：
1. 非特权容器的 Cgroup v2 设备控制器默认拒绝访问 `/dev/loop-control` 与 `/dev/loop*`；
2. Linux `/dev/loop*` 属于全局命名空间设备，一旦向容器放行 `b 7:*` 且容器内保留 `CAP_MKNOD`，恶意租户可通过创建他人对应的 `/dev/loopN` 节点直接读写宿主机上其他租户的 `disk.img`。

因此，所有 `loop` 绑定、加锁、格式化、扩容、卸载与 `FITRIM` 全部收敛在宿主机侧的 `devpilot-loop-csi` Node DaemonSet 中完成，**DevBox 容器与 Commit Job 容器均零暴露任何 `/dev/loop*` 块设备**。

### 6.2 `devpilot-loop-csi` 拓扑与池目录布局

```text
<pool_root>/
├── <pvc-volume-name>/
│   ├── disk.img          # 稀疏 ext4 镜像（持 F_OFD_SETLK 锁对象，内含 upper/ work/ merged/ .meta/）
│   ├── disk.img.lease    # 节点心跳租约 JSON（node, bootID, volumeID, device, acquired, heartbeat）
│   ├── volume.json       # 创建元数据（capacityBytes, copyMethod 等）
│   └── .snap/<snap>/     # （仅 cephfs-snapdir 策略）CephFS 瞬时快照视图
└── .snapshots/
    └── <snap-name>/
        ├── disk.img      # （reflink/copy 策略）快照镜像文件
        └── meta.json
```

运行形态（见 [`charts/devpilot/templates/loop-csi.yaml`](../charts/devpilot/templates/loop-csi.yaml)）：
- **CSI Controller（单副本 Deployment）**：挂载与计算节点完全一致的池路径（`--pool=<name>=<path>`），搭配 `csi-provisioner`、`csi-resizer`、`csi-snapshotter` sidecar；容器不访问 `/dev/loop*`，仅需基础文件权限创建/删除/克隆/快照 `disk.img` 与运行 `mkfs.ext4`。
- **CSI Node（DaemonSet）**：特权容器，挂载 `/dev`、`hostPath` 共享池路径及 `Bidirectional` 传播的 `<kubeletDir>/plugins/loop.devpilot.wutz.dev`（保存节点状态 `state/*.json` 与私有 ext4 挂载点 `mounts/*`），再向 Kubelet 目标路径做常规 bind mount。
- **Publish-only（不实现 `NodeStageVolume`）**：[`NodeGetCapabilities`](../internal/loopcsi/node.go) 有意不宣告 `STAGE_UNSTAGE_VOLUME`，因为 Kubelet 不会向 `NodeStageVolume` 传入 `readonly` 标志；由 `NodePublishVolume` 根据只读属性精确分流两种发布模式：
  - **读写模式（`ModeReadWrite`）**：DevBox Pod 使用。打开 `disk.img` → 申请 OFD 写锁 → 校验 `disk.img.lease` 并写租约 → 若超块带 `Errors` 则跑 `e2fsck -p` → `AttachFile`（4 KiB + DIO + Autoclear）→ 挂载 `ext4`（触发 MMP 检查）→ 按需 `resize2fs` → bind mount 给 Pod。Pod 内 `devpilot-init` 看到的仍是普通目录 `/mnt/.devpilot-rootdisk`，**代码零差异兼容块存储**。
  - **只读裸模式（`ModeRaw`）**：Commit Job（以及任意 `ReadOnlyMany` / `readOnly: true` 挂载）使用。对目标卷的 `disk.img` 申请共享读锁 `F_RDLCK`，校验无活跃外国租约后，**直接只读 bind mount 卷目录**把 `disk.img` 文件本身暴露给 Pod（不绑定 loop 设备、不挂载 ext4）。

---

## 7. `DevBoxCommit` 与纯用户态 `ext4` 解析

### 7.1 非特权用户态 `ext4` 解析器（[`internal/ext4`](../internal/ext4) + [`Ext4Source`](../internal/commit/ext4source.go)）

当 Commit Job 运行在裸盘（`ModeRaw`）之上时，`/mnt/.devpilot-rootdisk` 下没有 `upper/` 目录，而是直接存在只读的 `disk.img` 文件。[`openCommitInput`](../cmd/devpilot-commit/main.go) 自动探测该布局：

1. **纯只读零特权**：使用 Go 实现的 `ext4.OpenFile` 直接通过 `pread` 解析超级块、块组描述符（支持 32/64 字节）、Inode 表、Extent 树及传统间接块映射、线性与 HTree 目录、`inline_data`、硬链接、快/慢符号链接与扩展属性（in-inode 及外部 xattr 块 / `ea_inode`）。Commit Job 继续保持 `privileged: false`、`capabilities.drop: ["ALL"]`，可调度至任意普通 CPU 节点。
2. **绕开内核 `EROFS` 与只读 `trusted.*` 限制**：
   - 无需挂载 `ext4`，自然不会触发内核对只读设备带 `needs_recovery` 报 `EROFS` 拒挂；
   - 对于因崩溃等原因确实残留 `needs_recovery` 与未提交日志的镜像，[`internal/ext4/journal.go`](../internal/ext4/journal.go) 支持**纯内存 jbd2 日志重放**（v1/v2 superblock、32/64-bit tags、csum v2/v3、revoke、escaped blocks，绝不修改镜像文件）；
   - 用户态直接解析 `ext4` xattr 记录中的 `trusted.overlay.opaque`（无需 `CAP_SYS_ADMIN`），与 `.meta/opaque-dirs.json` 和 `user.overlay.opaque` 共同保证 opaque 目录转换准确。

### 7.2 在线热提交自动 Sync→Freeze 提升与严格切点保障

> **禁止对运行中写者的活盘直接裸读**：`ModeRaw` 会申请 `F_RDLCK`，并与运行中 DevBox 持有的 `F_WRLCK` 直接冲突；即使绕开锁，跨节点直读正在被修改的 `disk.img` 也会读到撕裂结构。

对于使用 `loop.devpilot.wutz.dev` 驱动的根盘，[`DevBoxCommitReconciler`](../internal/controller/devboxcommit_freeze.go) 的在线快照流程如下：

```text
DevBoxCommit (Running, consistency: "" | Sync | Freeze)
  │
  ▼ effectiveConsistency() 检测到 PV CSI Driver == loop.devpilot.wutz.dev
  │ 自动将 Sync 提升为 Freeze（窗口由 --commit-freeze-timeout 控制，默认 30s）
  │
  ├─► 1. ExecFreezer 通过 pods/exec 调起容器内 /proc/1/exe --freeze --freeze-timeout=<window+10s>
  │      devpilot-init 执行 syncfs + FIFREEZE，输出 "frozen" 并阻塞等待 stdin 关闭
  ├─► 2. 控制器创建 VolumeSnapshot（由 loopCSI.snapshotClass 处理：cephfs-snapdir / reflink / 稀疏 copy）
  ├─► 3. 控制器轮询 API Server 等待 snapshot Status.CreationTime 或 ReadyToUse == true
  │      - 若在 --commit-freeze-timeout 内确认切点：关闭 stdin 正常解冻（helper 必须以 exit 0 退出）
  │      - 若超时、exec 断连或 helper 提前自动解冻（exit 3）：立即删除不可信快照，commit 标为 FreezeFailed
  ├─► 4. 从已确认快照恢复临时 PVC → 以 ModeRaw 只读挂给隔离命名空间 Commit Job
  └─► 5. devpilot-commit 用户态解析快照中的 disk.img 生成增量 OCI 层并推送至仓库
```

**重要提示（关于无 Reflink 的后端）**：在 `strategy: auto` 且底层共享存储不支持 `FICLONE`（例如部分 NFSv4.1 或 GPFS 配置）时，[`CreateSnapshot`](../internal/loopcsi/controller.go) 在 `allowLiveCopy: true` 下会在**冻结期间执行稀疏拷贝（`copyImage`）**。若 `disk.img` 实际已分配数据较大（数 GB 至数十 GB），拷贝耗时可能超过默认的 `30s` 冻结窗口并导致 `FreezeFailed`。对此类后端：
- 可通过管理器启动参数 `--commit-freeze-timeout`（Chart 中经 `extraArgs` 配置）按实际写入量与存储带宽适当调大；
- 或者先将 DevBox 关机后执行**离线提交**（离线状态无需冻结，且可以直接在原卷上以 `ModeRaw` 只读发布，零拷贝开销）。

---

## 8. Helm Chart 与参数契约

在 [`charts/devpilot/values.yaml`](../charts/devpilot/values.yaml) 中，`loopCSI` 默认关闭（`enabled: false`）。开启示例：

```yaml
loopCSI:
  enabled: true
  pools:
    - name: vast
      path: /mnt/vast/devpilot-rootdisks   # 所有计算节点与 loop-csi controller 须挂载在相同路径
  storageClasses:
    - name: shared-loop-vast
      pool: vast
      directIO: required     # required | preferred | "off"（GPFS 池必须设为 "off" 或在安全内核下设为 preferred）
      trimOnUnmount: true    # 卸载前执行 FITRIM 打洞
      trimMinLength: "1048576"
      mmp: true              # 开启 ext4 Multi-Mount Protection
      reclaimPolicy: Delete
  snapshotClass:
    create: true
    name: devpilot-loop-snapclass
    strategy: auto           # auto | reflink | copy | cephfs-snapdir
    allowLiveCopy: true      # 配合 DevPilot 控制器冻结窗口使用
    deletionPolicy: Delete

# 若需将 shared-loop-vast 设为默认在线快照映射（也可在 DevBox.spec.rootDisk.volumeSnapshotClassName 中逐个指定）
defaults:
  snapshotClassByStorageClass:
    block-nvme: csi-rbdplugin-snapclass
    block-vast-cs: vast-snapshot-cs
    shared-loop-vast: devpilot-loop-snapclass
```

用户侧 `DevBox` 资源声明与使用原生块存储完全一致，仅需将 `spec.rootDisk.storageClassName` 设为上述 `shared-loop-vast`（并按需指定 `volumeSnapshotClassName`）。

### 生产 PoC 准入检查清单（开启 `loopCSI.enabled=true` 前必做）

在真实共享存储集群打开此特性前，建议按顺序完成以下实测验证：
1. **Direct I/O 回读与 GPFS 策略核验**：非 GPFS 池在目标节点共享池目录绑定 loop 后确认 `losetup -O NAME,DIO -l` 显示 **`DIO=1`**；若为 IBM Storage Scale（GPFS）池，驱动守卫会拒绝 `directIO: required`，仅可在已确认内核不受 `IJ60020` 影响时设置 `directIO: "off"`（或 `preferred`）。
2. **跨节点锁与断网接管测试**：在两台节点上用相同池路径测试 `F_OFD_SETLK` 互斥生效；模拟持锁节点宕机/断网，确认在存储协议租约期内旧锁释放且新节点可安全接管，无 `nolock` 或时间不同步问题。
3. **版本与安全基线**：确认节点 NTP 时钟同步正常；如使用 IBM Storage Scale，确认客户端/服务端与内核版本组合不存在 `IJ60020`（`6.15+` / `6.12.25+` / Ubuntu `6.14.4+` 需 Scale `>=5.2.3.10` / `6.0.1.2`）；确认 CephFS 使用内核态 `ceph.ko`、NFS 使用 `hard`（推荐 `nfsvers=4.1+`）。
4. **快照与清理验证**：实测目标池上 `CreateSnapshot`（`.snap`、`FICLONE` 或稀疏复制）的实际耗时并据此设置 `--commit-freeze-timeout`，同时验证 `FITRIM` 是否生效（若返回 `EOPNOTSUPP` 或占用不变，提前在运维指引中明确需用 `reset-rootdisk` 回收高水位空间）。

---

## 9. 与早期规划草案的落地差异修正记录（Corrections Log）

相比早期纯规划文档，编码落地阶段结合真实系统调用语义与一致性需求做出了以下关键修正：

| 主题 | 早期规划草案描述 | 实际代码落地实现与原因 |
| --- | --- | --- |
| **锁文件形态** | 使用独立的旁路锁文件 `disk.img.lock` 申请 `F_OFD_SETLK` | **直接锁在 `disk.img` 自身 fd 上**，并把该 fd 传给内核 loop 设备持有。这样锁的生命周期与内核 `loop` 绑定严格一致（CSI 插件重启不会丢锁，卸载 `AUTOCLEAR` 即自动释放）。 |
| **CSI Node Stage** | 未细化 `NodeStageVolume` 与 `NodePublishVolume` 分工 | **刻意不实现 `NodeStageVolume`（Publish-only）**：Kubelet 不会向 `NodeStageVolume` 传 `readonly`，因此直接在 `NodePublishVolume` 中按请求只读与否分流 `ModeReadWrite`（挂载 ext4）与 `ModeRaw`（只读 bind mount 暴露 `disk.img`）。 |
| **`ext4` 日志处理能力** | 假设用户态只读解析器要求镜像必须绝对干净、无法处理未重放日志 | 在 [`internal/ext4/journal.go`](../internal/ext4/journal.go) 完整实现了**内存只读 jbd2 日志重放**；同时控制器依然保留 Sync→Freeze 自动提升，保证快照复制窗口内磁盘零并发写入撕裂。 |
| **在线 Freeze 超时与可信判定** | 仅概略描述 `3s` 硬超时防死锁 | 引入可配置的 `--commit-freeze-timeout`（默认 `30s`，匹配跨节点快照或复制耗时；helper 侧设 `window+10s` 兜底）；且仅当快照切点在冻结窗口内确认且 helper 正常以 `exit 0` 退出时才保留快照，否则立即销毁快照并报错 `FreezeFailed`。 |
| **Rebase 空间回收行为** | 规划中假设 `rebaseDevBox: true` 直接替换全新空白 `disk.img` | 实际代码中 `rebaseDevBox` 复用现有 PVC 并由 `devpilot-init` 清空其中的 `upper/`/`work/`/`merged/`，因此无打洞能力后端的高水位彻底归零需通过停机 `reset-rootdisk` 注解删除重建 PVC 完成。 |
| **GPFS `O_DIRECT` 崩溃防护与 Helm YAML 1.1 修复** | 假设所有共享存储均可默认开启 `O_DIRECT + LO_FLAGS_DIRECT_IO` | 实测 IBM Storage Scale `5.2.3-5 + 5.15.0-94` 上 `kworker`（`current->mm == NULL`）异步 `O_DIRECT` 会触发 `mmfs26` `dioRdWr` 空指针 panic；在 [`loopdev`](../internal/loopdev/loopdev_linux.go) 与 [`node.go`](../internal/loopcsi/node.go) 新增 `statfs(0x47504653)` 防护（拒绝 `required`，`preferred` 回退缓冲，禁止对带 `O_DIRECT` 的 GPFS fd 绑定 loop），并修复 [`loop-csi.yaml`](../charts/devpilot/templates/loop-csi.yaml) 中 YAML 1.1 将未加引号 `directIO: off` 解析为布尔 `false` 而被 `default "required"` 覆盖的问题。 |
