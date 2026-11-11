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
- [ ] 验证（`bz1-dev1` 实测）：
  - [ ] 运行中开发机安装新依赖并触发 `DevBoxCommit`（`csi-rbdplugin-snapclass`），确认 GPU 进程无中断、新镜像成功推送到 Harbor、临时快照自动清理
  - [ ] 用推送的新镜像直接创建 `TrainTask` 或新 `DevBox`，验证环境开箱即用且先前被 whiteout 删除的文件在新镜像中确实不存在

---

## M3 开发接入体系、抢占中断与计费回收

目标：补齐生产级用户体验与智算云商业化运营闭环。

- [ ] 容器内 SSH 服务托管（兼容自带 `sshd` 与内置 Go SSH Server 兜底），持久化 `<devbox>-hostkey` Secret
- [ ] 对接 `sshgateway.wutz.dev` 自动生成 `SSHRoute`（验证 VS Code / Cursor Remote-SSH 端到端连接与端口转发）
- [ ] JupyterLab 自动托管与自定义 HTTP 端口暴露（生成 Gateway API `HTTPRoute` / Ingress，适配 `bz1-dev1` `:7443` 端口）
- [ ] 抢占式（`Spot`）驱逐检测与 `Interrupted` 状态流转、运行段计费账本（`status.segments[]`）
- [ ] 闲置自动关机（`idleShutdownMinutes`）与连续停机/中断 15 天超期自动回收（含提前预警 Event 与 `volumetrash` 对接）

---

## M4 平台生态集成与可选控制台看板

- [ ] `workspace.wutz.dev` 聚合 RBAC（`devpilot-member-edit` / `devpilot-member-view`）与 `<project>-build` 命名空间凭证同步
- [ ] 内置轻量 Web 看板（遵循 `DESIGN.md` 视觉规范：实例卡片、状态流转、一键开关机、一键保存镜像弹窗、计费明细展示）
- [ ] GitHub Actions 发布流水线：构建多架构镜像 `ghcr.io/wutz/devpilot` 与 OCI Helm Chart `oci://ghcr.io/wutz/charts/devpilot`
