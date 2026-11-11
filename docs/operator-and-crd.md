# Operator 与 CRD 规范

API Group：`devpilot.wutz.dev/v1alpha1`（遵循 `*.wutz.dev` 家族统一约定，由 Kubebuilder v4 脚手架生成）。

本组件仅引入两个 Namespaced CRD：
1. **`DevBox`**（简称 `dbox`）：声明一台持久化交互式开发机的计算规格、根盘持久化存储、接入方式、生命周期期望与计费状态。
2. **`DevBoxCommit`**（简称 `dcommit`）：声明一次将指定 `DevBox` 当前根盘修改固化并推送至镜像仓库的不可变构建任务（模型对齐 `imagebuild.wutz.dev/v1alpha1` `ImageBuild`）。

---

## 1. `DevBox` CRD 定义

### 1.1 示例清单与全量字段结构

```yaml
apiVersion: devpilot.wutz.dev/v1alpha1
kind: DevBox
metadata:
  name: kcs-mclsrexo
  namespace: forge-demo
  labels:
    workspace.wutz.dev/project: demo
spec:
  displayName: "PyTorch 2.4 FlashAttn 开发机"
  desiredState: Running                  # Running | Stopped
  billingMode: Spot                      # OnDemand | Spot
  pricePerHour: "2.80"                   # 定点金额字符串 (CNY/小时)，对齐 trainpilot Money 类型

  # 基础镜像（首次启动自动锁定 digest 到 status.activeImageDigest）
  image: cr.example.com/public/pytorch:2.4.0-cuda12.4-ubuntu22.04
  command: []                            # 为空时沿用原镜像 ENTRYPOINT/CMD；无常驻命令时兜底 sleep infinity
  env:
    - name: HF_ENDPOINT
      value: https://hf-mirror.com

  # 计算与调度规格
  resources:
    gpus: 1                              # 0 表示纯 CPU 开发机
    gpuModel: RTX-4090                   # 匹配节点标签（默认 scclabs.dev/gpu）
    cpu: "8"
    memory: 32Gi
    shmRatio: "0.5"                      # /dev/shm 内存占比，默认 0.5（即 16Gi）
  zone: shandong-1                       # 可选可用区/站点标签约束

  # 根文件系统持久化盘（OverlayFS upperdir + workdir）
  rootDisk:
    size: 50Gi                           # 支持在线扩容（StorageClass allowVolumeExpansion=true）
    storageClassName: block-nvme         # 必须为 RWO 块存储（如 block-nvme / block-vast-cs）
    volumeSnapshotClassName: csi-rbdplugin-snapclass # 可选覆盖，默认按平台映射表自动匹配
    retentionDaysAfterStop: 15           # 连续关机/中断满 N 天自动释放（0 表示永不自动释放）

  # 附加共享数据盘挂载（不进入镜像 Commit）
  volumes:
    - name: shared-data
      persistentVolumeClaim:
        claimName: project-shared-nvme
      mountPath: /root/shared-nvme
      readOnly: false

  # 交互接入配置
  access:
    ssh:
      enabled: true
      username: root                     # 登录系统用户名，默认 root
      authorizedKeys:
        - "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... user@laptop"
      passwordSecretRef:                 # 可选密码登录（如兼容传统控制台掩码密码）
        name: kcs-mclsrexo-ssh-pass
        key: password
      gatewayRef:                        # 可选对接 sshgateway.wutz.dev
        name: main
        namespace: sshgateway-system
    jupyter:
      enabled: true
      port: 8888
      tokenSecretRef:                    # 为空则由 Operator 自动生成持久化随机 Token Secret
        name: kcs-mclsrexo-jupyter-token
        key: token
    ports:                               # 自定义 Web 端口暴露（TensorBoard / Gradio / Code-Server）
      - name: tensorboard
        port: 6006
        protocol: HTTP

  # 生命周期自动化策略
  lifecycle:
    idleShutdownMinutes: 0               # 0 关闭；>0 时无活跃 SSH/Jupyter 连接且 GPU 利用率低则自动关机
    maxRuntimeHours: 0                   # 0 不限时

status:
  observedGeneration: 3
  phase: Running                         # Pending | Starting | Running | Stopping | Stopped | Interrupted | Failed
  startingStep: ""                       # AllocatingResources | MountingStorage | PullingImage | StartingContainers
  message: ""

  # 镜像与根盘状态
  activeImage: cr.example.com/public/pytorch:2.4.0-cuda12.4-ubuntu22.04
  activeImageDigest: sha256:8f4e2c9a71b3...
  pendingRebaseImage: ""                 # DevBoxCommit rebaseDevBox=true 完成后写入，下次启动生效
  rootDiskPVC: kcs-mclsrexo-rootdisk
  rebaseCount: 0

  # 运行实例与接入端点
  podName: kcs-mclsrexo-pod
  nodeName: gn-10-243-145-105
  endpoints:
    ssh: "ssh://kcs-mclsrexo@ssh.bz1-dev1.example.com:2222"
    jupyter: "https://jupyter-kcs-mclsrexo-forge-demo.bz1-dev1.example.com:7443"
    ports:
      tensorboard: "https://tensorboard-kcs-mclsrexo-forge-demo.bz1-dev1.example.com:7443"

  # 停机/中断与保留期倒计时
  stoppedAt: null
  interruptedAt: null
  scheduledReleaseTime: null             # 进入 Stopped/Interrupted 后 = 时间戳 + retentionDaysAfterStop

  # 计费账本（对齐 trainpilot 计费段模型）
  totalBilledSeconds: 7200
  totalCost: "5.60"
  segments:
    - id: 1
      nodeName: gn-10-243-145-105
      pricePerHour: "2.80"
      startTime: "2026-11-11T08:00:00Z"
      endTime: "2026-11-11T10:00:00Z"
      billedSeconds: 7200
      cost: "5.60"
      endReason: UserStopped             # Running 段为空；闭合后为 UserStopped | IdleTimeout | Preempted | NodeFailure | AccountArrears | Released

  conditions:
    - type: RootDiskReady
      status: "True"
      reason: Bound
      lastTransitionTime: "2026-11-11T07:59:50Z"
    - type: Ready
      status: "True"
      reason: PodReady
      lastTransitionTime: "2026-11-11T08:00:00Z"
```

### 1.2 状态机与流转规则

```text
                    创建 (desiredState=Running)
                                │
                                ▼
                            [Pending] (创建 PVC / HostKey Secret)
                                │
                                ▼
                           [Starting] (AllocatingResources → MountingStorage → PullingImage → StartingContainers)
                           ╱    │    ╲
      (抢占式无库存/拒绝) ╱     │     ╲ (镜像拉取失败/切根校验失败)
                        ▼       ▼      ▼
               [Interrupted] [Running] [Failed]
                    ▲         │   ▲
    (Spot 节点回收) │         │   │ (desiredState=Running 启动)
                    ├─────────┘   │
                    │             │
                    │    (desiredState=Stopped 或闲置超时)
                    │             │
                    │             ▼
                    │        [Stopping] (优雅 SIGTERM + sync 刷盘 + 删 Pod detach PVC)
                    │             │
                    │             ▼
                    └──────► [Stopped]
                                  │
                     (连续停机/中断满 15 天自动 GC 或用户主动删除)
                                  ▼
                              (Deleted)
```

### 1.3 快捷操作注解（兼容 CLI 操作习惯，对齐 `trainpilot`）

除直接 `patch spec.desiredState` 外，支持单次动作注解。控制器在写回 `spec` 的同一次 `Update` 中删除该注解，因此同一个动词可以在之后任意时刻再次下发；不要依赖「已处理」标记去重：`stop` 这类注解的取值每次相同，用标记去重会把第二次请求当成重复投递而静默丢弃。
- `devpilot.wutz.dev/action: stop=<timestamp>`：关机（等价于置 `desiredState: Stopped`）
- `devpilot.wutz.dev/action: start=<timestamp>`：启动（等价于置 `desiredState: Running`）
- `devpilot.wutz.dev/action: restart=<timestamp>`：优雅重启开发机 Pod（若有 `pendingRebaseImage` 则在本次重启时完成 Rebase 并清空 `upper/`）
- `devpilot.wutz.dev/action: reset-rootdisk=<timestamp>`：仅允许在 `Stopped` / `Interrupted` / `Failed` 状态下执行，清空 `upper/` 并允许更换 `spec.image`

### 1.4 准入校验规则（CEL + ValidatingWebhook）

1. **`rootDisk.storageClassName` 不可变**：创建后禁止修改，防止孤儿数据卷。
2. **`rootDisk.size` 单调不减**：只允许在线扩容（`self.size >= oldSelf.size`），禁止缩容。
3. **`spec.image` 保护**：当 `DevBox` 已初始化根盘且未同步执行重置时，直接修改 `spec.image` 会被 Webhook 拒绝并提示：「根盘包含基于原镜像的增量修改，请先通过 `DevBoxCommit` 保存镜像，或执行重置系统盘后再切换基础镜像」。
4. **`billingMode` 不可变**：对齐 `ai-prd` §6.2，创建后抢占式（`Spot`）与按量计费（`OnDemand`）不可互转。
5. **保留环境变量名保护**：禁止 `spec.env` 覆盖 `DEVPILOT_*` 内部系统变量。

---

## 2. `DevBoxCommit` CRD 定义

### 2.1 示例清单与全量字段结构

`DevBoxCommit` 的 `spec` 为**不可变（Immutable）**——每一次镜像保存都是一个独立的构建记录对象（对齐 `ImageBuild` 的 `+kubebuilder:validation:XValidation:rule="self == oldSelf"` 设计）。

```yaml
apiVersion: devpilot.wutz.dev/v1alpha1
kind: DevBoxCommit
metadata:
  name: commit-pytorch-flashattn-v1
  namespace: forge-demo
spec:
  # 来源开发机（位于同一 namespace）
  devBoxName: kcs-mclsrexo

  # 目标仓库与标签（最终推送为 <registry>/<namespace>/<repository>:<tag>）
  destination:
    repository: devbox/pytorch-flashattn
    tags:
      - v1
      - cuda12.4-latest

  # 在线提交一致性与压缩选项
  consistency: Sync                      # Sync (默认, sync刷盘后打CSI快照) | Freeze (短暂fsfreeze+CSI快照) | OfflineOnly (要求必须Stopped)
  compression: zstd                      # zstd (默认, 解压与构建快) | gzip (最大兼容老旧客户端)
  cleanPackageCache: true                # 默认清理 /var/cache/apt/archives/*.deb 与 pip http 缓存
  excludePaths:                          # 在默认排除规则外追加排除的相对根路径 glob
    - "root/scratch/**"

  # 可选镜像配置覆盖（追加到基础镜像 Config 上）
  changes:
    author: "alice@example.com"
    comment: "Installed flash-attn 2.7.0 and transformers 4.48"
    env:
      - name: CONDA_DEFAULT_ENV
        value: llm
    workingDir: /root

  # 提交成功后是否在下次启动时自动将该镜像设为 DevBox 新底座并清空 upper/（仅离线停机提交允许设为 true，防止在线快照后的写入丢失）
  rebaseDevBox: false

status:
  phase: Succeeded                       # Pending | Snapshotting | Building | Pushing | Succeeded | Failed
  mode: OnlineSnapshot                   # OfflinePVC | OnlineSnapshot | OnlineSameNode
  message: ""
  baseImageDigest: sha256:8f4e2c9a71b3...
  layerDiffID: sha256:4a1b90e2c3d4...
  layerSize: 1482910208                  # 压缩后增量层字节数（约 1.38 GiB）
  digest: sha256:e9c1778a02f4...         # 新镜像 Manifest Digest
  images:
    - cr.example.com/forge-demo/devbox/pytorch-flashattn:v1
    - cr.example.com/forge-demo/devbox/pytorch-flashattn:cuda12.4-latest
  snapshotName: ""                       # 构建期间创建的临时 VolumeSnapshot，结束后已清理
  buildJobName: commit-pytorch-flashattn-v1-8x9k
  startTime: "2026-11-11T09:30:00Z"
  completionTime: "2026-11-11T09:31:12Z"
  conditions:
    - type: Succeeded
      status: "True"
      reason: ImagePushed
      lastTransitionTime: "2026-11-11T09:31:12Z"
```

### 2.2 并发互斥与垃圾回收（GC）

1. **同一 `DevBox` 提交串行化**：若同一 `devBoxName` 已有处于 `Snapshotting` / `Building` / `Pushing` 阶段的 `DevBoxCommit`，新提交保持在 `Pending`（`reason: WaitingForActiveCommit`），避免并发生成冗余快照。
2. **快照期间防误删锁**：`DevBoxCommit` 添加 Finalizer `devpilot.wutz.dev/commit-cleanup`。无论构建成功、失败还是中途被用户删除 CR，Operator 都确保构建命名空间中的临时 PVC、跨命名空间或同命名空间的 `VolumeSnapshot` 以及临时凭据被彻底清理，不留存储泄漏。
3. **停机期间提交锁**：当离线提交正在读取 `Stopped` 开发机的 rootdisk PVC 时，若用户此刻点击开机（`desiredState: Running`），`DevBoxReconciler` 检测到存在活跃离线 Commit Job，会在 `Starting`（`MountingStorage`）短暂等待 Commit 读取完毕释放 RWO 卷后再挂载启动，避免 RWO Multi-Attach 错误。

---

## 3. 连续停机超期回收（15 天自动释放）

对齐 `ai-prd` §3.1 与 §6.2 规则：
1. 每当 `DevBox` 进入 `Stopped` 或 `Interrupted` 状态时，若之前未处于非运行态，记录首次进入时间 `nonRunningSince = min(stoppedAt, interruptedAt)`（即从 `Interrupted` 和 `Stopped` 之间切换不重置累计时长；只有真正恢复到 `Running` 才清零该计时器）。
2. 计算 `status.scheduledReleaseTime = nonRunningSince + retentionDaysAfterStop * 24h`（默认 15 天）。
3. 释放前预警 Event：在距离 `scheduledReleaseTime` 剩余 72h、24h 时发出标准 K8s Warning Event（`ShutdownRetentionExpiring`），供上层平台消费发送站内信或邮件。
4. 到期回收：到达 `scheduledReleaseTime` 后，Operator 闭合生命周期记录，可按平台配置执行直接删除 `DevBox`（级联回收 rootdisk PVC）或为 rootdisk PVC 打上 `volumetrash.wutz.dev/expire-at` 标签移入 `volumetrash` 回收站缓冲期。
