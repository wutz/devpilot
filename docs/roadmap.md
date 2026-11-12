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
- [x] JupyterLab 自动托管与自定义 HTTP 端口暴露（生成 Gateway API `HTTPRoute` / Ingress，适配 `bz1-dev1` `:7443` 端口）
- [x] 抢占式（`Spot`）驱逐检测与 `Interrupted` 状态流转、运行段计费账本（`status.segments[]`）
- [x] 闲置自动关机（`idleShutdownMinutes`）与连续停机/中断 15 天超期自动回收（含提前预警 Event 与 `volumetrash` 对接）

> 实测记录（2026-11-12，`devpilot-verify/m3-idle` + `m3-retention`，RTX 5090，manager `devpilot:m3-7`）：
> - 新参数 `--retention-reclaim=trash|delete`（Chart 值 `retention.reclaim`，默认 `trash`），非法值在启动时由 `Config.Validate` 拒绝；
> - `lifecycle.idleShutdownMinutes: 1` 的 DevBox 开机后，最后一次活动取 `devpilot.wutz.dev/last-active` 心跳与当前打开运行段的**较晚者**；超过阈值后控制器在同一次 reconcile 里关闭运行段为 `endReason=IdleTimeout`、写 `desiredState=Stopped`、`phase=Stopping`、`Ready=False/IdleTimeout` 并发出 `Normal IdleShutdown`，随后一次 reconcile 删除 Pod 落到 `Stopped`；
> - 实测账本：`segments[0]` `05:10:14Z → 05:11:16Z`、`billedSeconds=62`、`cost=0.048222`、`totalCost=0.048222`（`2.80 × 62/3600` 定点截断），segment 只关闭一次，Pod 删除后未再拉起；
> - 连续停机计时以 `status.nonRunningSince` 为基准，`scheduledReleaseTime = nonRunningSince + retentionDaysAfterStop × 24h`；`retentionDaysAfterStop: 1` 时开机即进入 `Ready=False/Stopped` 并立刻给出 `Warning ShutdownRetentionExpiring`（`ExpiringIn24h`）；
> - 把 `retentionDaysAfterStop` 改为 15 并回拨 `nonRunningSince` 到「距回收 60h」「距回收 10h」，条件分别变为 `RetentionExpiring=True/ExpiringIn72h`、`ExpiringIn24h` 且各只发一次 Event（同一阈值内重复 reconcile 静默）；再回拨到「距回收 14h」时条件被清除；
> - 回拨到「已超期 1 天」后，控制器关闭运行段为 `endReason=Released`、发 `Warning Released`、给 root 盘 PVC 打上 `volumetrash.wutz.dev/expire-at=20261112T050849Z`（RFC 3339 的冒号在 label 值里非法，故用紧凑布局）并**摘掉 owner reference**（否则 GC 会连 recycler 要保留的卷一起删），随后删除 DevBox；实测 PVC 仍在且 `Bound`，owner 为空；
> - `--retention-reclaim=delete` 由 envtest 覆盖：PVC 带 `deletionTimestamp` 且不打 `expire-at` 标签（envtest 无卷控制器，`pvc-protection` 会一直挂住，故以删除时间戳为证据）；
> - 断机/中断共用同一计时：`Interrupted` 期间不清 `nonRunningSince`，只有回到 `Running` 才清空 `nonRunningSince`、`scheduledReleaseTime` 与 `RetentionExpiring` 条件；
> - 每次 reconcile 的唤醒间隔取「下一个阈值」并被 `1h` 上限截断，集群里大量闲置 DevBox 不会变成轮询；
> - 实测暴露并修复一个缺陷：账本结算原本排在闲置判定之后，心跳过期（上一轮遗留）的 DevBox 在第一次看到 `Running` 的同一次 reconcile 里就被关机，**导致 `segments` 为空、`totalCost` 恒为 `0.00`**（即闲置关机一分不记）；改为先 `settleBilling` 再判定闲置，并补了 envtest 回归用例（断言 `endReason=IdleTimeout`、`billedSeconds>=600`、`totalCost` 非零）；
> - `devpilot-manager-role` 覆盖所用权限：`persistentvolumeclaims` 的 `update`、`devboxes` 的 `delete`、`events` 的 `create` 均可为（`kubectl auth can-i` 三项均为 `yes`），本次无需改 RBAC。

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

> 实测记录（2026-11-12，`devpilot-verify/m3-spot`，RTX 5090，manager `devpilot:m3-5`）：
> - `billingMode: Spot` + `pricePerHour: "2.80"` 的 DevBox 进入 `Running` 时 `status.segments[0]` 立即打开（`id=1`、`nodeName` 记录所在节点、`endTime` 为空、`cost` 为空），重复 reconcile 不改写 `startTime`；
> - 把 Pod 状态改为 `Failed`/`Evicted`（模拟节点回收）后，控制器进入 `Interrupted`：`interruptedAt` 与 `nonRunningSince` 写入、`nodeName`/`podName` 清空、`Ready=False/Preempted`、新增条件 `Preempted=True`、Pod 被删除，且此后不再原地拉起（持续观察 1 分钟无新 Pod）；
> - 账本在同一个 status patch 里关闭运行段：`endReason=Preempted`、`billedSeconds=24`、`cost=0.018666`（`2.80 × 24/3600` 定点截断），`totalCost` 由账本求和派生；
> - `devbox/m3-spot` 上出现 `Warning Interrupted` Event（`The spot node was reclaimed; the root disk is retained`）；`status.segments` 上限 256，超出时只丢弃最旧的已关闭段；
> - 用户手动 `action: start` 后 `interruptedAt` 清空、条件 `Preempted` 移除，在同一块 root disk 上重新调度并打开 `segments[1]`，历史段与 `totalCost` 保留；
> - 按需（`OnDemand`）DevBox 同样有账本，但被驱逐时走 `Failed` + `endReason=NodeFailure`，不会被标记为抢占；
> - 实测暴露并修复两个缺陷：① 中断后由 Pod 删除事件触发的 reconcile 从 informer 缓存读到过期的 `Running` 状态，又把 Pod 拉了起来（改为 DevBox 直读 API server），并补了对应 envtest 回归用例；② 新 events API 需要 `events.k8s.io/events` 的 `create;patch` 权限，原 RBAC 只有核心组 `events`，导致中断 Event 被 API server 拒绝。

> 实测记录（2026-11-12，`devpilot-verify/m3-route`，RTX 5090，manager `devpilot:m3-3` + `devpilot-init:m3-3`）：
> - 暴露方式选型：集群 Ingress 控制器为 `ingress-nginx`（`LoadBalancer 10.243.147.247`，`8080:30935,7443:30199/TCP`，`--default-ssl-certificate=cert-manager/default-tls`），现有路由统一为 `ingressClassName: nginx` + 仅写 `tls.hosts` 不写 `secretName`；MetalLB 池已耗尽且 Gateway API 被 agentgateway 占用，故选择 `networking.k8s.io/v1 Ingress` 而非 `HTTPRoute`，URL 统一带 `:7443`；
> - 新增管理参数 `--ingress-class/--ingress-domain/--ingress-port`（本次 `nginx` / `dev1.bz1.paratera.com` / `7443`），三者任一为空即不发布 HTTP；
> - `access.jupyter.enabled=true` 后控制器生成 `<devbox>-jupyter` Secret（32 字节随机 hex，key `token`，owner reference 指向 DevBox），以 `JUPYTER_TOKEN` 环境变量交给 `devpilot-init`（不进 argv，`/proc/<pid>/cmdline` 无泄漏），挂在 `/run/devpilot-jupyter/token`（该路径在 `pivot_root` 后仍可见）并由 `jupyter lab --no-browser --ip=0.0.0.0 --allow-root` 托管；
> - 共享接入 `Service/m3-route` 同时承载 `ssh/22`、`jupyter/8888`、`api/3000`，`Ingress/m3-route` 把 `jupyter-m3-route-devpilot-verify` 与 `api-m3-route-devpilot-verify` 两个主机转发到按名引用（named port）的同一 Service，注解 `proxy-body-size=0`、`proxy-read/send-timeout=3600`、`proxy-buffering=off` 以支持长内核会话与 WebSocket；
> - 实测 `https://jupyter-…:7443/` 返回 302 → `/lab?`，带 `?token=` 登录 200，`/api/status` 返回 JupyterLab 状态 JSON；`https://api-…:7443/` 在容器内 `python3 -m http.server 3000` 起服务后返回 200 与预期正文（未起服务时正确 502）；
> - `spec.access.ports[]` 走 `+listType=map` + CEL（保留名 `ssh`/`jupyter`、端口号唯一、`maxItems=16`），Webhook 额外拦截 Jupyter 端口与 22 或自定义端口冲突；
> - `desiredState=Stopped` 后 `Ingress`、`Service`、`<devbox>-jupyter` Secret 均保留，重新开机后主机名与 token 不变，同一 URL 再次返回 200；`specHash` 只纳入 `jupyter` 开关与端口（端口变更替换 Pod，token 轮换不替换）；
> - 集群上 `devpilot-manager-role` 需随清单补齐 `networking.k8s.io/ingresses` 权限（已 patch），CRD 亦需 `kubectl apply` 更新以带上 `ports[]` 与 Jupyter 校验规则。

---

## M4 平台生态集成与可选控制台看板

- [x] `workspace.wutz.dev` 聚合 RBAC（`devpilot-member-edit` / `devpilot-member-view`）与 `<project>-build` 命名空间凭证同步

> 实测记录（2026-11-12，`devpilot-verify` / `devpilot-verify-build`，RTX 5090，manager `devpilot:m4-2`）：
> - 聚合契约先按集群现状确认：聚合器为 `workspace-edit` / `workspace-view`，选择器是 `matchLabels: workspace.wutz.dev/aggregate-to-member: edit|view`（与已有 `project-member-edit`、`imagebuild-member-edit` 同形），因此新增的 `config/rbac/devpilot_member_edit_role.yaml`、`devpilot_member_view_role.yaml` 只带该标签，不改聚合器；`kubectl apply` 后 `workspace-edit` 的 `devpilot.wutz.dev` 规则已自动出现（edit = devboxes/devboxcommits 全量 + 两个 status 的 get；view = get/list/watch + status get）；
> - Chart 侧同规则在 `templates/member-roles.yaml`，由 `rbac.memberRoles`（默认 `true`）控制；没有项目运营方的集群可置 false；
> - 凭证隔离动因：`project-member-edit` 给项目命名空间授予 `secrets: get/list/watch`，项目内的 push token 对每个成员可读，能覆盖不属于自己的镜像。构造约束是 build Job 必须留在项目命名空间（CSI 克隆与根盘 PVC 同命名空间，Pod 也只能挂本命名空间的 Secret），所以拆成两份：耐久副本放 `<project>-build`，项目命名空间里只有一份随 Job 生灭的 `<commit>-registry` 过渡副本；
> - 新增参数 `--build-namespace-suffix`（默认 `-build`，Chart `build.namespaceSuffix`），置空即关闭隔离、凭证留在项目命名空间（仅当成员读不到 secrets 时才安全）；后缀会拒绝 `/ : 空格`，否则会折叠回项目命名空间或产生非法名；
> - 实跑：`m4-cred` commit 开建后自动创建 `devpilot-verify-build`（labels `devpilot.wutz.dev/project-namespace: devpilot-verify`），`harbor-push` 副本落地（payload 与源一致，labels 带 `project-namespace` / `credential`，不继承源的 helm 注解），项目命名空间里出现 `m4-cred-registry`（注解 `credential-source: devpilot-verify-build/harbor-push`，owner reference 指向 commit），build Job 的 `registry` volume 指向它；Job 终态后过渡副本随 `releaseRegistryCredential` 删除，耐久副本保留供下次构建；
> - 凭证轮换用 envtest 覆盖：源 Secret payload 变更后下一次 `syncRegistryCredential` 覆盖耐久副本（`Immutable` 一并清空）；项目命名空间缺失时回落读 `<project>-build`；两处都没有则以 `RegistryCredentialFailed` 失败；
> - RBAC：管理器需新增 `namespaces` 的 create/get/list/watch（`make manifests` 与 Chart `clusterrole.yaml` 同步）才能按需建 build 命名空间；集群上 `devpilot-manager-role` 已 patch，`auth can-i create namespaces` 由 no 变 yes；
> - 顺带修掉的线上缺陷：commit 的 finalizer 原先用整对象 `Update` 写入，`spec` 上 `self == oldSelf` 的 CEL 规则会拒绝——typed client 总会序列化可选的 `changes` 结构，而由 YAML 创建的 commit 存的 spec 没有 `changes` 键，于是报 `spec is immutable` 并无限重试（envtest 里所有 commit 都由同一 typed client 创建，`changes` 一开始就存在，所以测不出来）。现改为 metadata merge patch（`patchCommitFinalizer`），`m4-cred` 随即进入 `Building`。
- [ ] 内置轻量 Web 看板（遵循 `DESIGN.md` 视觉规范：实例卡片、状态流转、一键开关机、一键保存镜像弹窗、计费明细展示）
- [ ] GitHub Actions 发布流水线：构建多架构镜像 `ghcr.io/wutz/devpilot` 与 OCI Helm Chart `oci://ghcr.io/wutz/charts/devpilot`
