# 演进路线图

每个里程碑均以「自动化单测/envtest 通过 + 在开发集群 `bz1-dev1`（或基于 `bz1-dev1` 创建的 `vcluster` 开发集群）端到端验证」作为关闭条件。

---

## M0 架构与 CRD 规范设计（当前）

- [x] 完成方案选型对比：确立「块存储 PVC 持久化 OverlayFS `upperdir` + 非特权流式 OCI 层追加」路线
- [x] 核实目标集群 `bz1-dev1` 基础能力：K8s `v1.32.5`、Linux `5.15.0`、`containerd 2.0.5`、`RuntimeClass: nvidia`、块存储 `block-nvme` / `block-vast-cs` 及 CSI `VolumeSnapshotClass`（`csi-rbdplugin-snapclass` / `vast-snapshot-cs`），确认 `ImageVolume` 未启用
- [x] 输出架构总览、切根与镜像固化机制深潜、`DevBox` / `DevBoxCommit` CRD 规范、安全与接入设计文档
- [x] 设计评审确认，进入 M1 编码阶段

---

## M1 脚手架、`devpilot-init` 与开关机全盘持久化

目标：跑通「创建 GPU 开发机 → `apt`/`conda` 修改系统目录与删除文件 → 关机删 Pod 释放 GPU → 跨节点重新开机 → 所有修改与 `nvidia-smi` 100% 正常」闭环。

- [x] 使用 Kubebuilder v4 初始化 Go 工程（`github.com/wutz/devpilot`，API Group `devpilot.wutz.dev/v1alpha1`），配置 Makefile、`.golangci.yml` 与 Helm Chart（`charts/devpilot`）
- [x] 实现 `cmd/devpilot-init`（`CGO_ENABLED=0` 静态二进制）：
  - [x] `.meta/base-image.json` 首次写入与跨重启镜像摘要强校验
  - [x] 内核 `overlay` 挂载（`metacopy=off,index=off`）
  - [x] `/proc/self/mountinfo` 扫描排序与 `MS_BIND|MS_REC` 子挂载迁移（重点覆盖 `/dev/nvidia*`、`libcuda.so.*`、`nvidia-smi`、`/dev/shm`、共享存储 PVC）
  - [x] `pivot_root` + 懒卸载旧根 + `PR_CAPBSET_DROP` 永久剥离业务子进程的 `CAP_SYS_ADMIN`
  - [x] PID 1 信号转发、僵尸进程回收与 `SIGTERM` 退出前 `sync()` + `.meta/opaque-dirs.json` 刷新
- [x] 实现 `DevBoxReconciler` 核心生命周期：
  - [x] rootdisk PVC 创建与在线扩容同步
  - [x] `Pending` → `Starting` → `Running` ↔ `Stopping` → `Stopped` 状态机
  - [x] 快捷注解（`stop` / `start` / `restart` / `reset-rootdisk`）与 ValidatingWebhook 校验
- [x] 验证（`bz1-dev1` 实测）：
  - [x] 在 GPU 节点 A 创建 PyTorch 开发机，执行 `apt-get install -y tmux htop`、`pip install einops`、删除底层某预置文件，校验 `torch.cuda.is_available() == True`
  - [x] 关机确认 Pod 销毁、GPU 释放，指定调度至另一台 GPU 节点 B 重新启动，验证 `tmux`、`einops`、文件删除状态以及 `torch.cuda.is_available()` 全部完好

> 实测记录（2026-11-11，`nvcr.io/nvidia/pytorch:25.08-py3`，`block-nvme` 50Gi root disk）：
> - `gn-10-243-145-105` → `gn-10-243-145-106` 跨节点重启后，`tmux`/`htop` 的 `dpkg` 记录、`einops 0.8.1`、被删除的 `/opt/nvidia/entrypoint.d/12-banner.sh`（whiteout 生效）与 `torch.cuda.is_available() == True`（RTX 5090）全部保持；
> - 关机后 Pod 立即销毁、GPU 释放、root disk PVC 保留；
> - 实测同时暴露并修复了四个环境相关问题：新 PVC 上 `upper/` 尚未创建即被打开、`RuntimeDefault` seccomp 拒绝 `pivot_root`、`CMD=None` 的入口脚本导致 PID 1 立即退出、动作注解按文本去重导致第二次同名动作被丢弃。

---

## M2 `devpilot-commit`：离线与 CSI 快照在线固化镜像

目标：跑通「开发机不停机或停机状态下，一键将容器修改秒级打包追加为新 OCI 镜像并推送至 Harbor，新镜像可直接用于新开发机或 `TrainPilot` 训练任务」。

- [x] 实现 `cmd/devpilot-commit` 流式构建引擎：
  - [x] OverlayFS `upper/` 扫描器：字符设备 `0:0` → `.wh.<name>` 转换、`.meta/opaque-dirs.json` / `opaque` xattr → `.wh..wh..opq` 转换、硬链接去重保留
  - [x] 默认排除规则（`/tmp`、APT/Pip 缓存、SSH HostKey、受管 `authorized_keys`）与自定义 `excludePaths`
  - [x] 基于 `go-containerregistry` 远端读取基础镜像 Manifest/Config，流式压缩（`zstd` / `gzip`）上传增量层并推送新 Manifest（零基础层下载）
  - [x] 支持 `spec.changes`（`ENV` / `WORKDIR` / `CMD` / `Author` / `Comment`）元数据合成
- [x] 实现 `DevBoxCommitReconciler`：
  - [x] 离线路径（`DevBox` 为 `Stopped`/`Interrupted`）：直接只读挂载 rootdisk PVC 启动隔离构建 Job
  - [x] 在线热路径（`DevBox` 为 `Running`）：触发容器 `sync`/`fsfreeze` → 创建 CSI `VolumeSnapshot` → 克隆临时只读 PVC → 启动构建 Job → Finalizer 自动清理临时 PVC 与快照
  - [x] 实现 `spec.rebaseDevBox: true`：提交成功后写入 `pendingRebaseImage`，下次重启自动切换底座并清空 `upper/`
- [x] 验证（`bz1-dev1` 实测）：
  - [x] 运行中开发机安装新依赖并触发 `DevBoxCommit`（`csi-rbdplugin-snapclass`），确认 GPU 进程无中断、新镜像成功推送到 Harbor、临时快照自动清理
  - [x] 用推送的新镜像直接创建 `TrainTask` 或新 `DevBox`，验证环境开箱即用且先前被 whiteout 删除的文件在新镜像中确实不存在

  实测记录（2026-11-12，`devpilot-verify/m1-verify`，RTX 5090）：
  提交 `m1-verify-online-m2g`（`gzip`，`block-nvme=csi-rbdplugin-snapclass`）成功推送
  `cr.dev1.bz1.paratera.com:7443/library/m1-verify:m2-verify@sha256:f15c1bc9…`。
  构建期间 GPU 训练进程（pid 86）持续运行 1h35m 未中断；Job、克隆 PVC、`VolumeSnapshot` 在终态后全部自动清理。
  用该镜像启动新 `DevBox m2-verify` 后，`/root/persistence-marker` 保留、`humanize 4.16.0` 可直接导入，
  `/opt/nvidia/entrypoint.d/` 与源开发机一致（被 whiteout 删除的条目在新镜像中不存在）。
  过程中定位并修复 6 个仅生产环境暴露的缺陷：#15 flush 路径、#16 构建 Job 用户、#17 失败 Job 泄漏、
  #18 私有目录读取权限、#19 终态后幽灵 Job、#20 增量层 diffID 计算顺序。

---

## M3 开发接入体系、抢占中断与计费回收

目标：补齐生产级用户体验与智算云商业化运营闭环。

- [x] 容器内 SSH 服务托管（兼容自带 `sshd` 与内置 Go SSH Server 兜底），持久化 `<devbox>-hostkey` Secret
- [x] 对接 `sshgateway.wutz.dev` 自动生成 `SSHRoute`（验证 VS Code / Cursor Remote-SSH 端到端连接与端口转发）
- [ ] JupyterLab 自动托管与自定义 HTTP 端口暴露（生成 Gateway API `HTTPRoute` / Ingress，适配 `bz1-dev1` `:7443` 端口）
- [ ] 抢占式（`Spot`）驱逐检测与 `Interrupted` 状态流转、运行段计费账本（`status.segments[]`）
- [ ] 闲置自动关机（`idleShutdownMinutes`）与连续停机/中断 15 天超期自动回收（含提前预警 Event 与 `volumetrash` 对接）

> 实测记录（2026-11-12，`devpilot-verify/m3-ssh-verify`，RTX 5090，`devpilot-init:m3-2`）：
> - `<devbox>-hostkey` Secret 在 Pod 之前创建，含 420 字节 OpenSSH 私钥、对应公钥与 `authorized_keys`，owner reference 指向 `DevBox`；
> - 公钥登录成功（`/root`，`TERM`/PTY 正常），未授权密钥被拒（`Permission denied (publickey)`），未开放密码登录；
> - 删除 Pod 由控制器重建后主机密钥与 Secret `resourceVersion` 均未变化，`known_hosts` 严格校验通过，`/opt/m3/probe.txt` 跨重启保留；
> - `ssh -tt host 'cmd'` 从无终端脚本发起（`pty-req` 尺寸 0x0）可正常拿到 PTY 并执行完整命令，非 PTY 的 `ssh host cmd` 正确回传退出码（`exit 9` → 9）；
> - 实测暴露并修复两个缺陷：#23 的 0x0 `pty-req` 被当成「不要 PTY」，以及 stdin 立刻 EOF 时关闭 PTY 主设备导致 shell 被 `SIGHUP` 杀死（Linux 上必现，macOS 上偶发）；
> - 同期发现并修正：集群上 `devpilot-manager-role` 未随 RBAC 清单更新，缺少 `secrets` 权限导致 Secret informer 无法建立、reconcile 卡在等待 PVC。

> 实测记录（2026-11-12，`devpilot-verify/m3-route` + `forgeai-system/main` 网关，RTX 5090，manager `devpilot:m3-2`）：
> - 前置：集群上已有 `sshgateway` Chart 0.4.0 控制器（`--gateway-namespace=forgeai-system`，sshpiper `full-v1.6.1`），新建 `SSHGatewayClass/sshgateway` 与 `SSHGateway/forgeai-system/main`（`allowedRoutes.namespaces.from: All`，2222 端口），三个条件 `Accepted/Programmed/Ready` 全部为 True；
> - DevBox 置 `access.ssh.gatewayRef.name=main, namespace=forgeai-system` 后，控制器在 PVC 绑定阶段即生成同名的 `Service/m3-route`（22/TCP）与 `SSHRoute/m3-route`，用户名 `devpilot-verify-m3-route`（命名空间限定，网关内唯一），`backendRefs` 指向 `m3-route:22 / root`；网关侧 `status.listeners[0].attachedRoutes` 由 0 变 1，`Accepted`、`ResolvedRefs` 均为 True，DevBox 条件 `SSHRouteReady=True/RouteAccepted`；
> - 网关平台身份公钥（`sshgateway-identity/ssh-publickey`）被自动并入 `<devbox>-hostkey` 的 `authorized_keys`，与用户客户端密钥并列，路由的 `authorizedKeys` 只含客户端密钥；
> - `ssh -p 30322 devpilot-verify-m3-route@10.243.145.105` 经网关登录成功（`m3-route-pod`，`root`），网关呈现 sshpiper 自身主机密钥（`SSH-2.0-SSHPiper`），对内层 `m3-route.devpilot-verify.svc` 按 `known_hosts_data` 校验后端主机密钥，客户端 `known_hosts` 无需感知 Pod 变化；
> - `-L 127.0.0.1:18022:127.0.0.1:22` 免密转发成功，端口转发（Remote-SSH 的 devcontainer / 调试端口链路）可用；
> - 关机再开机后 `Service` 与 `SSHRoute` 原样保留（用户名不被释放、不改变），重新连接在 `StrictHostKeyChecking=yes` 下通过，`hostname` 与登录用户不变；
> - 环境限制：MetalLB 地址池已耗尽（`no available IPs`），`SSHGateway` 的 `status.addresses` 为空，故 `status.endpoints.ssh` 回落到 Pod 级地址；NodePort `30322` 在网关 Pod 所在节点 `gn-10-243-145-105` 上可达，共享 VIP `10.243.147.247` 上不可达，实测即经该 NodePort 完成；
> - 集群上 `devpilot-manager-role` 再次需随清单补齐 `services` 与 `sshgateway.wutz.dev/sshgateways,sshroutes` 权限（已 patch，未重新 apply 整份清单）。

---

## M4 平台生态集成与可选控制台看板

- [ ] `workspace.wutz.dev` 聚合 RBAC（`devpilot-member-edit` / `devpilot-member-view`）与 `<project>-build` 命名空间凭证同步
- [ ] 内置轻量 Web 看板（遵循 `DESIGN.md` 视觉规范：实例卡片、状态流转、一键开关机、一键保存镜像弹窗、计费明细展示）
- [ ] GitHub Actions 发布流水线：构建多架构镜像 `ghcr.io/wutz/devpilot` 与 OCI Helm Chart `oci://ghcr.io/wutz/charts/devpilot`
