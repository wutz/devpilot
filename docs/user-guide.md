# 用户使用指南与场景示例

本文档从使用者（机器学习研发人员 / 平台用户与运维管理员）视角，汇总 **DevPilot** 的公开使用场景、功能操作方法、注意事项与可直接套用的 YAML 示例。

> 若需要深入了解底层切根原理、CRD 字段全表或架构设计，请参阅 [架构总览](architecture.md)、[Operator 与 CRD 规范](operator-and-crd.md)、[持久化与镜像提交机制深潜](persistence-and-commit.md) 与 [接入体系与安全边界](access-and-security.md)。

---

## 目录

- [1. 场景速查与示例清单](#1-场景速查与示例清单)
- [2. 快速开始](#2-快速开始)
- [3. 常见 DevBox 规格与挂载场景](#3-常见-devbox-规格与挂载场景)
- [4. 交互式接入（SSH / JupyterLab / HTTP 与 gRPC 端口）](#4-交互式接入ssh--jupyterlab--http-与-grpc-端口)
- [5. 开关机、重启、重置系统盘与成本自动化](#5-开关机重启重置系统盘与成本自动化)
- [6. 将开发环境固化为容器镜像（DevBoxCommit 与 Rebase）](#6-将开发环境固化为容器镜像devboxcommit-与-rebase)
- [7. 大镜像启动加速与节点预热（Prepull & ImagePrewarm）](#7-大镜像启动加速与节点预热prepull--imageprewarm)
- [8. 权限体系（RBAC）与安全提示](#8-权限体系rbac与安全提示)
- [9. 单机多卡模型微调实践与场景边界](#9-单机多卡模型微调实践与场景边界)
- [10. 常见问题排查（FAQ）](#10-常见问题排查faq)

---

## 1. 场景速查与示例清单

| 使用场景 | 核心功能点 | 示例文件 |
| --- | --- | --- |
| **GPU 算法开发机（主力场景）** | 单/多卡 GPU、Guaranteed QoS、共享存储 PVC、SSH 网关、JupyterLab、TensorBoard、Spot 计费 | [devbox-pytorch.yaml](../deploy/examples/devbox-pytorch.yaml) |
| **单机多卡模型微调（LoRA / SFT）** | 多卡 GPU（如 4×/8×）、大比例 `/dev/shm`、只读底座模型 + 读写 Checkpoint 卷、`HF_HOME` 重定向、TensorBoard、训练不断线固化 | [devbox-finetune.yaml](../deploy/examples/devbox-finetune.yaml) |
| **纯 CPU 调试机 / 按需冷创建** | `gpus: 0`、无 NVIDIA 运行时依赖、初始 `desiredState: Stopped` 预分配系统盘、闲置自动关机 | [devbox-cpu-minimal.yaml](../deploy/examples/devbox-cpu-minimal.yaml) |
| **模型推理与多端口调试（HTTP + gRPC）** | 只读权重共享盘（`readOnly: true`）、同时暴露 HTTP 与 gRPC 服务、可用区（`zone`）与污点容忍 | [devbox-inference-grpc.yaml](../deploy/examples/devbox-inference-grpc.yaml) |
| **运行中在线保存镜像（不中断进程）** | `consistency: Sync`、CSI `VolumeSnapshot`、`zstd` 压缩、包缓存清理、路径排除 | [devbox-commit.yaml](../deploy/examples/devbox-commit.yaml) |
| **停机离线固化并 Rebase 瘦身** | `consistency: OfflineOnly`、`zstd:chunked`、`rebaseDevBox: true`、节点自动预热 `prewarmNode: true` | [devbox-commit-offline-rebase.yaml](../deploy/examples/devbox-commit-offline-rebase.yaml) |
| **集群级热门基础镜像预热（管理员）** | 0 GPU 并发拉取、`pinImage: true` 防止 kubelet GC 清理、新节点自动补齐 | [image-prewarm.yaml](../deploy/examples/image-prewarm.yaml) |
| **Kustomize 基础 Sample** | 开箱可用的 `DevBox` / `DevBoxCommit` / `ImagePrewarm` 最小配置 | [config/samples/](../config/samples/) |

---

## 2. 快速开始

### 2.1 前置条件

1. 集群已安装 DevPilot Operator（含 CRD 与 Webhook）。
2. 目标命名空间可用以下基础设施：
   - **块存储 StorageClass**（推荐 `ReadWriteOnce`，如 `block-nvme`），若需运行中在线 Commit 需配套 `VolumeSnapshotClass`（如 `csi-rbdplugin-snapclass`）。
   - 如需保存镜像到仓库，需配置好镜像推送凭证 Secret（或由平台自动注入）。
3. 准备本地 SSH 公钥（例如 `~/.ssh/id_ed25519.pub`）。

### 2.2 创建并连接第一台 DevBox

以 [devbox-pytorch.yaml](../deploy/examples/devbox-pytorch.yaml) 为例：

1. 将文件中的 `spec.access.ssh.authorizedKeys` 替换为你自己的 SSH 公钥，按需调整 `storageClassName`、`gpuModel` 与 `volumes`（若暂无共享 PVC 可先删除 `volumes` 段）。
2. 提交创建：

```bash
kubectl apply -f deploy/examples/devbox-pytorch.yaml
```

3. 观察状态推进（`Pending` → `Starting` → `Running`）：

```bash
kubectl get devbox kcs-pytorch-dev -w
```

当 `PHASE` 变为 `Running` 后，查看访问端点与详细信息：

```bash
kubectl get devbox kcs-pytorch-dev -o yaml
```

在 `status.endpoints` 中可以看到：
- `ssh`：若配置了 `gatewayRef`，为网关稳定连接地址；未配置网关时为集群内 Pod 直连地址 `<user>@<name>-pod.<ns>.pod:22`。
- `jupyter`：JupyterLab 访问 URL（当管理员配置了 `--ingress-domain` 时生成）。
- `ports`：自定义暴露的 HTTP/gRPC URL 映射。

---

## 3. 常见 DevBox 规格与挂载场景

### 3.1 GPU 开发机 vs 纯 CPU 开发机

- **GPU 开发机（`spec.resources.gpus > 0`）**：
  - Operator 自动为 Pod 注入 `runtimeClassName: nvidia`，请求 `nvidia.com/gpu`，并在 `gpuModel` 非空时添加节点标签选择器（默认键为 `scclabs.dev/gpu`）。
  - CPU 与内存固定设置 `requests == limits`（Guaranteed QoS），避免训练过程中因节点内存压力被驱逐。
  - 容器内的 `/dev/shm` 会按 `memory * shmRatio`（默认 `0.5`，即内存的一半）分配为内存盘，防止 PyTorch `DataLoader` 多进程报错。
- **纯 CPU 开发机（`spec.resources.gpus: 0`）**：
  - 不要求 GPU 节点，不设置 `nvidia` RuntimeClass，适合代码阅读、数据预处理、CPU 编译测试。
  - 同样拥有完整的 rootfs 持久化、`/dev/shm` 配比、SSH 与 JupyterLab。参见 [devbox-cpu-minimal.yaml](../deploy/examples/devbox-cpu-minimal.yaml)。

### 3.2 创建时先准备磁盘暂不开机（`desiredState: Stopped`）

创建 `DevBox` 时若将 `spec.desiredState` 设为 `Stopped`：
- 控制器会提前创建好根盘 PVC（`<name>-rootdisk`）、SSH HostKey Secret、Service 与 Ingress/SSHRoute 占位，确保访问地址和密钥提前固定；
- **不会创建 Pod，不会占用 GPU/CPU 算力，也不会开启计费段**；
- 需要使用时再开机（见第 5 节）。

### 3.3 挂载共享存储（数据集 / 模型权重）

通过 `spec.volumes` 可挂载同命名空间下已有的 `ReadWriteMany`（或节点可挂）PVC：

```yaml
spec:
  volumes:
    - name: shared-datasets
      persistentVolumeClaim:
        claimName: team-datasets
      mountPath: /workspace/datasets
      readOnly: false
    - name: model-weights
      persistentVolumeClaim:
        claimName: shared-models
      mountPath: /models
      readOnly: true
```

> **重要特性**：`spec.volumes` 挂载的共享目录是独立子挂载，**不会被 `DevBoxCommit` 打包进容器镜像**。请把代码/数据放在共享卷或独立目录中，只把操作系统库、Conda/Python 依赖等放进根盘。

### 3.4 自定义启动命令、环境变量与调度约束

- **`spec.command` / `spec.args`**：留空或镜像默认 CMD 为裸 Shell（如 `/bin/bash`、`sh`、`zsh`）时，DevPilot 会自动回退为 `sleep infinity` 以保持容器常驻可连接；若镜像本身需要启动守护进程或自定义训练前初始化脚本，可显式指定 `command`/`args`。
- **`spec.env`**：可注入 `HF_ENDPOINT`、`NCCL_DEBUG` 等环境变量（最多 128 项）。注意：**以 `DEVPILOT_` 开头的变量名由系统保留**，CRD 与 Webhook 会拒绝用户覆盖。
- **调度控制**：
  - `spec.zone`：自动映射为 `topology.kubernetes.io/zone`。
  - `spec.nodeSelector`：与 `gpuModel`、`zone` 合并，冲突时以 `spec.nodeSelector` 优先。
  - `spec.tolerations`：透传至 Pod，便于调度到带专用 GPU 污点的节点。

### 3.5 创建后哪些字段可改、哪些不可变？

| 字段 | 创建后能否修改 | 说明 |
| --- | --- | --- |
| `resources`（GPU/CPU/内存/`shmRatio`） | 可改 | 修改后会重建 Pod 生效，rootdisk 内容完整保留 |
| `volumes` / `env` / `command` / `nodeSelector` / `tolerations` | 可改 | 修改后重建 Pod 生效 |
| `access.ssh.authorizedKeys` | 可改 | **热更新**：直接刷新 Secret，不重启运行中的 Pod |
| `access.jupyter.port` / `access.ports` | 可改 | 修改端口会同步更新 Service/Ingress 并按需重建 Pod |
| `rootDisk.size` | **仅允许扩容** | 不可缩小，支持在线/离线向底层 CSI 扩容 |
| `rootDisk.storageClassName` | **不可变** | 创建后禁止修改（需重置系统盘场景除外） |
| `billingMode` | **不可变** | `OnDemand` 与 `Spot` 创建后不可互转 |
| `spec.image` | **根盘创建后锁定** | 一旦绑定 `status.activeImageDigest`，直接修改会被拒绝，必须通过「Commit + Rebase」或「`reset-rootdisk`」切换 |

---

## 4. 交互式接入（SSH / JupyterLab / HTTP 与 gRPC 端口）

### 4.1 SSH 接入（VS Code / Cursor Remote-SSH）

1. 在 `spec.access.ssh` 中启用并填入公钥：

```yaml
access:
  ssh:
    enabled: true
    username: root
    authorizedKeys:
      - "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... user@laptop"
    # 可选：若集群部署了 sshgateway，填写网关引用即可获取稳定公网/跳板路由
    gatewayRef:
      name: main
      namespace: sshgateway-system
```

2. **HostKey 持久化**：DevPilot 会在同命名空间自动生成 `<name>-hostkey` Secret 并跨关机/迁移节点永久复用，因此再次开机不会出现客户端 `REMOTE HOST IDENTIFICATION HAS CHANGED` 告警。如需主动轮换，直接删除该 Secret 再重调谐即可。
3. **动态增删公钥不中断连接**：向 `authorizedKeys` 追加或删减公钥不会重建 Pod，下一次 SSH 登录即刻生效。
4. **注意**：`devpilot-init` 内置轻量 SSH 服务**仅支持公钥认证（`authorizedKeys`）**；`passwordSecretRef` 字段仅用于自行运行传统 `sshd` 的自定义镜像。

**在本地 `~/.ssh/config` 中配置（结合 SSHGateway 时）**：

```sshconfig
Host my-pytorch-dev
  HostName <gateway-host>
  Port <gateway-port>
  User <status.endpoints.ssh 中的网关路由用户名>
  IdentityFile ~/.ssh/id_ed25519
```

### 4.2 JupyterLab 接入

开启 `spec.access.jupyter`：

```yaml
access:
  jupyter:
    enabled: true
    port: 8888
```

- 未指定 `tokenSecretRef` 时，Operator 自动创建 `<name>-jupyter` Secret 生成随机 64 位十六进制 Token。查看 Token：

```bash
kubectl get secret <devbox-name>-jupyter -o jsonpath='{.data.token}' | base64 -d && echo
```

- 若希望多实例共用固定密码/Token，可提前创建含 `token` 键的 Secret 并通过 `tokenSecretRef` 引用。
- `jupyter.port` 不能与 SSH `22` 端口或 `access.ports` 端口重复。

### 4.3 自定义 HTTP 与 gRPC 调试端口

通过 `spec.access.ports` 最多可暴露 16 个端口（`ssh` 与 `jupyter` 为保留端口名）：

```yaml
access:
  ports:
    - name: tensorboard
      port: 6006
      protocol: HTTP
    - name: triton-grpc
      port: 8001
      protocol: GRPC
```

- `HTTP` 与 `GRPC` 规则会自动拆分到独立的 Ingress（gRPC Ingress 自动注入 `nginx.ingress.kubernetes.io/backend-protocol: GRPC`），并在 `status.endpoints.ports` 中分别生成 `https://...` 与 `grpcs://...` 地址。
- **重要细节**：即使 DevBox 处于关机（`Stopped`）状态，Operator 仍会保留 Service、Ingress 与 `SSHRoute`，确保你的域名、SSH 路由用户名与书签长期固定，开机即通。

---

## 5. 开关机、重启、重置系统盘与成本自动化

### 5.1 两种开关机方式

**方式 A：直接修改 `spec.desiredState`**（适合 Declarative GitOps 或上层平台调用）：

```bash
# 关机（立即释放 Pod/GPU，保留 rootdisk PVC、SSH HostKey、Jupyter Token 和路由）
kubectl patch devbox kcs-pytorch-dev --type=merge -p '{"spec":{"desiredState":"Stopped"}}'

# 开机
kubectl patch devbox kcs-pytorch-dev --type=merge -p '{"spec":{"desiredState":"Running"}}'
```

**方式 B：使用单次动作注解 `devpilot.wutz.dev/action`**（推荐脚本或自动化系统重复触发）：

控制器会在执行后自动清除该注解，因此相同的动作（例如多次 `stop` 或多次 `restart`）可以带上时间戳反复下发：

```bash
# 关机
kubectl annotate devbox kcs-pytorch-dev devpilot.wutz.dev/action="stop=$(date +%s)" --overwrite

# 开机（也可用于从 Spot 中断态 Interrupted 恢复）
kubectl annotate devbox kcs-pytorch-dev devpilot.wutz.dev/action="start=$(date +%s)" --overwrite

# 重启容器（删除并重建 Pod，保留全部根盘数据）
kubectl annotate devbox kcs-pytorch-dev devpilot.wutz.dev/action="restart=$(date +%s)" --overwrite

# 重置系统盘（仅在 Stopped / Interrupted / Failed 状态下允许！）
kubectl annotate devbox kcs-pytorch-dev devpilot.wutz.dev/action="reset-rootdisk=$(date +%s)" --overwrite
```

> **提示**：执行 `reset-rootdisk` 会删除 `<name>-rootdisk` PVC 并清空 `status.activeImageDigest` 与 `status.lastNodeName`——这是**不通过 Commit 直接切换 `spec.image` 基础镜像**的唯一途径；在 `Running` 状态下调用会被拒绝。

### 5.2 抢占式实例（`billingMode: Spot`）中断行为

当 `Spot` 类型的 DevBox 所在节点发生抢占、驱逐或被标记 `node.kubernetes.io/out-of-service:NoExecute` 时：
1. DevBox 会立即停机并进入 `Interrupted` 状态，设置 `Preempted=True` Condition 与 Kubernetes Event。
2. 当前计费段立刻闭合（`endReason: Preempted`），系统盘与共享盘完整保留。
3. **控制器不会在后台盲目自动拉起抢占实例**（避免反复争抢资源或产生非预期扣费）；当你需要继续工作时，执行 `action: start` 或将 `spec.desiredState` 置为 `Running` 即可重新调度。

### 5.3 闲置自动关机与超期数据保留（GC）

- **闲置自动关机（`spec.lifecycle.idleShutdownMinutes`）**：
  - 设置大于 0 的分钟数后，若自开机或最近一次 `devpilot.wutz.dev/last-active` 活跃时间起超过该阈值未活跃，DevPilot 会自动将实例关机并闭合当前计费段（`endReason: IdleTimeout`）。
- **停机后保留期（`spec.rootDisk.retentionDaysAfterStop`）**：
  - 默认 `15` 天（`0` 表示永久保留不自动清理）。
  - 自实例进入 `Stopped` 或 `Interrupted` 起累计连续未运行时间（可在 `status.scheduledReleaseTime` 查看预计清理时间）；在到期前 72 小时和 24 小时发送预警（`ShutdownRetentionExpiring` Condition 与 Event）。
  - 重新开机后会清零停机计时并清除预警。

### 5.4 查看运行段计费明细

DevBox 每次进入 `Running` 都会在 `status.segments[]` 中开启一条计费段，停机/中断时以定点字符串（最多 6 位小数）结算费用并累加至 `status.totalCost` 与 `status.totalBilledSeconds`（最多保留最近 256 条已闭合计费段）。

```bash
kubectl get devbox kcs-pytorch-dev -o jsonpath='{range .status.segments[*]}{.id}{"\t"}{.startTime}{"\t"}{.endTime}{"\t"}{.billedSeconds}s{"\t"}{.cost}{"\t"}{.endReason}{"\n"}{end}'
```

---

## 6. 将开发环境固化为容器镜像（DevBoxCommit 与 Rebase）

当你通过 `apt`、`pip`、`conda` 或源码编译安装好了环境，只需提交一个 `DevBoxCommit` CR，即可把根盘增量层（`upper/`）流式打包追加到远端容器镜像，无需在宿主机挂载 Docker/containerd socket，也不必拉取数十 GB 基础镜像。

### 6.1 运行中在线热提交（不中断工作）

参见 [devbox-commit.yaml](../deploy/examples/devbox-commit.yaml)：

```bash
kubectl apply -f deploy/examples/devbox-commit.yaml
kubectl get devboxcommit commit-pytorch-flashattn-v1 -w
```

`consistency` 策略说明：
- **`Sync`（默认推荐）**：在运行中的 Pod 内执行文件系统落盘同步，然后通过 CSI `VolumeSnapshot` 创建临时只读克隆卷交由构建 Job 读取，无需停机。
- **`Freeze`**：通过 `fsfreeze` 短暂冻结根盘文件系统完成极高一致性快照后立即解冻（使用 `loop.devpilot.wutz.dev` 共享存储根盘时会自动升级为 `Freeze`）。
- **`OfflineOnly`**：仅在 DevBox 关机后构建；若创建时 DevBox 仍在运行，`DevBoxCommit` 会停留在 `Pending` 等待关机后自动开始。

### 6.2 停机离线固化并 Rebase 根盘

如果你在开发机中进行了多次升级安装，或希望把旧增量层压平为新的基础镜像（减少叠加层数并释放空间），建议先关机再使用 `rebaseDevBox: true`（参见 [devbox-commit-offline-rebase.yaml](../deploy/examples/devbox-commit-offline-rebase.yaml)）：

- **`rebaseDevBox: true`**：仅允许在离线 Commit 时使用。推送成功后会在源 DevBox 上标记 `status.pendingRebaseImage`，下次开机时自动将底层镜像切换为新镜像并清空旧 `upper/`，`status.rebaseCount` 加 1。
- **`prewarmNode`**：当 `rebaseDevBox: true` 时默认开启（`true`），在镜像推送成功后立即创建一次 `ImagePrewarm`，提前把新镜像拉回源 DevBox 上次所在节点，减少下次开机的冷启动等待。
- **压缩与裁剪**：
  - `compression`：支持 `zstd`（默认）、`gzip`、`zstd:chunked`。
  - `cleanPackageCache: true`（默认）：自动跳过 apt/pip 下载缓存，减小镜像体积。
  - `excludePaths`：按相对于根盘的 glob 排除不需要打入镜像的临时文件或大模型权重目录（当检测到大容量模型缓存 ≥ 5Gi 时，会发出 `LargeModelCache` 事件提醒）。
  - `changes`：叠加目标镜像的 `author`、`comment`、`env`、`workingDir`、`cmd`。

> **注意**：`DevBoxCommit.spec` 创建后完全不可变；同一台 DevBox 同时发起多个 `DevBoxCommit` 时，控制器会自动排队串行执行。

---

## 7. 大镜像启动加速与节点预热（Prepull & ImagePrewarm）

PyTorch/CUDA 基础镜像通常达到 15–30 GB。DevPilot 提供两层机制缩短冷启动等待：

1. **DevBox 级缓存亲和与双阶段无卡预拉取（`spec.scheduling`）**：
   - `stickyNodeAffinity: true`（默认）：关机再开机时优先调度回上次运行节点（`status.lastNodeName`），复用本地镜像缓存。
   - `prepull: Auto`（默认，`Always` 强制 / `Never` 禁用）：当 GPU 开发机需要调度且满足预拉取条件时，先用 **0 GPU** 轻量 Pod 在节点完成基础镜像拉取，确认缓存就绪后再创建正式 GPU Pod，避免拉取镜像的数分钟空占昂贵 GPU 算力。可通过 `status.imageCacheState` 观察当前状态。

2. **管理员级集群预热（`ImagePrewarm`）**：
   参见 [image-prewarm.yaml](../deploy/examples/image-prewarm.yaml)。支持对满足 `nodeSelector` / `tolerations` 的节点分批并发（`maxConcurrency`）预拉取指定镜像，且当集群新扩容节点匹配标签时自动补齐；开启 `pinImage: true` 后可运行极轻量（5m CPU / 8Mi 内存 / 0 GPU）哨兵 Pod 防止 kubelet 镜像垃圾回收（GC）误删热门基础镜像。

---

## 8. 权限体系（RBAC）与安全提示

DevPilot 提供了两个可被多租户项目体系聚合的标准 `ClusterRole`：

- **`devpilot-member-edit`**（开发成员）：允许创建/删除/修改/查看 `devboxes` 与 `devboxcommits`（及读取 `/status`）。
- **`devpilot-member-view`**（只读访客）：仅允许查看 `devboxes`、`devboxcommits`、`imageprewarms`（及 `/status`）。

在提交 `DevBoxCommit` 时，若平台配置了隔离构建命名空间（默认 `--build-namespace-suffix=-build`），推送镜像所需的 Registry Secret 会被限制在 `<ns>-build` 命名空间内运行构建任务，普通用户无法在开发机容器或项目命名空间内窃取仓库推送凭证。

---

## 9. 单机多卡模型微调实践与场景边界

交互式开发与**单机多卡实验性微调**（如 LoRA / QLoRA / SFT、小规模 DPO、消融实验与梯度/显存调优）是 DevBox 的典型高频使用场景。对应可直接套用的示例清单见 [devbox-finetune.yaml](../deploy/examples/devbox-finetune.yaml)。

### 9.1 推荐实践要点

1. **多卡规格与 `/dev/shm` 配比**：
   - `spec.resources.gpus` 支持单机 `0..64` 卡；GPU 型 DevBox 自动以 `requests == limits`（Guaranteed QoS）向集群申请 CPU、内存与 `nvidia.com/gpu`，防止微调进程因节点内存压力被驱逐。
   - PyTorch `DDP` / `FSDP` / `DeepSpeed` 多进程 `DataLoader` 与 NCCL 进程间通信强依赖共享内存。请按显存与并发 `num_workers` 合理配置主机内存与 `shmRatio`（默认 `"0.5"`，例如 `memory: 128Gi` 对应 `/dev/shm` `64Gi`）。
2. **存储分层：避免权重与 Checkpoint 撑爆根盘**：
   - **环境放根盘（rootdisk）**：CUDA 工具链、PyTorch、`deepspeed`、`peft`、`flash-attn` 等系统库与 Python 依赖留在根盘，便于跨机保留与 `DevBoxCommit` 固化。
   - **权重与样本放共享 PVC（`spec.volumes`）**：通过只读卷挂载底座权重（如 `/models`，`readOnly: true`），通过读写卷挂载数据集与 Checkpoint 输出目录（如 `/workspace`，`readOnly: false`）。
   - **重定向下载缓存环境变量**：在 `spec.env` 中配置 `HF_HOME=/workspace/cache/huggingface` 与 `TORCH_HOME=/workspace/cache/torch`，避免 Hugging Face / TorchHub 权重默认落入 `/root/.cache`——不仅消耗 `rootdisk`，还会在提交镜像时触发 ≥ 5 GiB 的 `LargeModelCache` 体积告警并拖慢推送。
3. **训练不断线导出环境镜像，无缝衔接大规模训练**：
   - 调试跑通单机 4 卡/8 卡微调脚本与超参后，可直接在训练运行中提交 `consistency: Sync` 的 `DevBoxCommit`，无需停掉当前微调实验即可把最终环境固化为 OCI 镜像。
   - 通过 `spec.access.ports` 暴露 TensorBoard（`6006/HTTP`）实时观察 Loss/梯度曲线；若使用 `billingMode: Spot` 跑长时微调，务必将 Checkpoint 定期保存到共享 PVC（`/workspace`），以便被抢占进入 `Interrupted` 后重新开机并从最新 Checkpoint 续训（`resume_from_checkpoint`）。

### 9.2 与多机分布式训练（TrainPilot）的明确边界

| 能力诉求 | 推荐产品 / 方式 | 原因说明 |
| --- | --- | --- |
| 环境调试、单机 1–8 卡 LoRA / SFT 交互式微调、实时改代码看指标 | **DevPilot (`DevBox`)** | 全根盘持久化、SSH/Jupyter 直连、TensorBoard 实时观测、支持随时暂停与固化镜像 |
| **多机多卡分布式训练**（跨节点 Gang Scheduling、RDMA 自动注入、多机 AllReduce 容错恢复） | **DevBoxCommit 固化镜像 → 投递 TrainPilot (`TrainTask`)** | DevPilot 非目标明确不包含跨节点拓扑编排；应在单机 DevBox 验证好依赖与单卡/单机脚本后，通过 `DevBoxCommit` 出包交由 `TrainPilot` 无人值守扩机跑全量训练 |
| **长期无人值守周期性微调流水线** | **CI / 工作流编排 + `TrainTask`** | DevBox 面向人机交互式工作区，受 `idleShutdownMinutes` 等交互保护约束 |

---

## 10. 常见问题排查（FAQ）

1. **为什么修改 `spec.image` 时被 Webhook 拒绝？**
   - 当根盘已创建并绑定了 `status.activeImageDigest` 后，强行更换基础镜像会让已有 `upper/` 中的动态库路径与新底层系统错位。
   - **解决**：如果希望保留当前系统环境并切换基础镜像，请先关机后创建 `rebaseDevBox: true` 的 `DevBoxCommit`；如果不需要旧根盘数据，先关机再执行 `kubectl annotate devbox <name> devpilot.wutz.dev/action="reset-rootdisk=$(date +%s)" --overwrite`，然后再修改 `spec.image` 开机。

2. **执行 `reset-rootdisk` 报错 `reset-rootdisk requires a stopped devbox`？**
   - 仅支持在 `Stopped`、`Interrupted` 或 `Failed` 阶段重置根盘。请先关机并等待 `status.phase` 变为 `Stopped` 后再重试。

3. **DevBox 启动后很快变为 `Failed`（`the devbox command exited successfully`）？**
   - 镜像自定义的 `spec.command` 执行完即退出（DevBox 需要一个前台常驻进程作为 PID 1 托管目标）。如果只需交互式开发，请移除短命令或改为长驻进程（例如 `sleep infinity`）。

4. **`DevBoxCommit` 一直处于 `Pending`？**
   - 可能原因包括：(a) 同一台 DevBox 上已有更早的 `DevBoxCommit` 正在执行（自动排队互斥）；(b) 设置了 `consistency: OfflineOnly` 但目标 DevBox 尚未关机；(c) 当前节点暂不支持在线 CSI 快照且处于等待中。通过 `kubectl describe devboxcommit <name>` 查看 `status.message` 与 Events 即可确认。

5. **集群只有 GPFS / NFS 等共享文件系统、没有块存储 CSI 怎么办？**
   - 平台管理员可在部署时评估启用实验性的 `loopCSI` 稀疏 `disk.img` 根盘后端（默认关闭），详见 [共享存储可选 `disk.img` 根盘架构设计与实现说明](shared-storage-disk-img.md)。
