# 超大 AI 基础镜像（30–100 GB）加速与预热架构规划

> **状态**：设计规划（Design Proposal，仅规划与架构文档，尚未进入编码实施）
> **关联组件**：`DevBoxReconciler`、`devpilot-init`、`devpilot-commit`、集群容器运行时（`containerd 2.0+` / `Spegel`）

---

## 1. 背景与问题剖析

在 AI 研发场景中，交互式开发机（DevBox）所使用的基础镜像通常包含完整的 CUDA Toolkit、cuDNN、NCCL、TensorRT、PyTorch、FlashAttention、vLLM/DeepSpeed 以及庞大的 Conda/Python 依赖树，**镜像逻辑体积普遍在 30–100 GB（压缩后约 15–45 GB，包含数十万个 `.so`/`.py` 小文件）**。当开发机首次创建、关机后跨节点重新调度、或执行 `DevBoxCommit` 变基（Rebase）切换新镜像时，一旦目标 GPU 节点未命中本地缓存，冷拉取往往耗时 **10–30 分钟**。

### 1.1 为什么 30–100 GB 镜像拉取需要 10–30 分钟？（耗时拆解）

超大镜像的启动慢**绝不仅仅是网络带宽问题**。以一个典型 70 GB（压缩后 28 GB，`gzip` 格式，70+ 层，45 万个文件）的 AI 镜像为例，端到端冷启动耗时分布在三个串行或半串行阶段：

| 瓶颈阶段 | 耗时占比 | 典型耗时（70 GB 镜像） | 根因剖析 |
| --- | --- | --- | --- |
| **1. 仓库下载（Pull）** | 30%–40% | 4–10 分钟 | Harbor 单实例出口带宽受限、跨机房/跨网段限速、多台 GPU 节点并发拉取打满 Ingress 带宽；大层单 HTTP 流吞吐受限 |
| **2. 解压计算（Decompress）** | **35%–45%** | **5–12 分钟** | **最容易被忽视的头号瓶颈**：传统 OCI 镜像默认采用 `gzip`，**`gzip` 无法多线程并行解压**，单核 CPU 解压极限仅约 `80–130 MB/s`，解压 28 GB 压缩流单靠 CPU 就要硬算 4–6 分钟 |
| **3. 文件系统解包（Unpack / Apply Diff）** | 20%–30% | 2–8 分钟 | `containerd` 必须**严格按层顺序（Layer 0 → Layer N）串行** apply tar 流；数十万个 Python/CUDA 小文件触发海量 `open`/`write`/`chmod`/`utimensat`/`syncfs` 元数据 IOPS |

更关键的矛盾在于：**容器开机启动（拉起 `devpilot-init`、`sshd`、`JupyterLab` 乃至首次 `import torch`）实际读取的热点文件通常只有 1.5–3 GB（不到镜像总量的 5%）**，但标准 OCI 容器运行时强制要求 **100% 下载 + 100% 解压落盘** 后才允许创建容器进程。

### 1.2 双重损失：用户体验崩塌与昂贵 GPU 空转

1. **破坏交互式“随用随关”心智**：开发机是强交互场景，用户点击“开机”期待在 10 秒内连上 VS Code Remote-SSH。若每次跨节点开机都要干等 20 分钟，用户会拒绝关机、抵触配置闲置自动关机（`idleShutdownMinutes`），导致集群 GPU 长期被闲置会话占死。
2. **平台承担隐性算力空转成本**：在当前 `DevBoxReconciler` 设计中，计费账本（`status.segments[]`）从 Pod 真正进入 `Running` 才开始计费（用户不承担拉镜像期间的费用）；但 K8s 调度器在 `Starting (PullingImage)` 阶段**已经将整张/整机 GPU 独占分配给了该 Pod**。每次 20 分钟的冷拉取，都意味着平台白白损失 20 分钟的高价值 GPU 可售卖时长。

---

## 2. 技术方案全景对比

解决 30–100 GB 镜像开机慢，不能靠单一银弹，需要从 **「减少跨节点缓存未命中（调度与预热）」**、**「加速全量拉取与解压（P2P + zstd）」**、**「打破全量下载约束（按需懒加载 / 块级快照）」** 三个层次组合治理：

| 方案维度 | 具体技术手段 | 冷节点首次开机耗时 | 缓存命中开机耗时 | 对 `devpilot-init` 切根兼容性 | 基础设施侵入度 | 适用阶段 |
| --- | --- | --- | --- | --- | --- | --- |
| **A. 缓存感知调度** | `lastNodeName` 软亲和 + Kubelet `ImageLocality` 权重调优 | 无改善（仍需冷拉） | **2–5 秒** | 100% 零改动 | **极低**（纯 Operator 调度策略） | **P0 必选** |
| **B. 声明式镜像预热** | `ImagePrewarm` CRD 预拉取官方底座 + 节点磁盘 GC 水位治理 | **2–5 秒**（提前变热节点） | **2–5 秒** | 100% 零改动 | **低**（仅受控 DaemonJob/Pod） | **P0 必选** |
| **C. 压缩与分层治理** | 全面切换 `zstd` + 统一 Golden CUDA Base 层共享 | 缩短 **40%–60%**（消除单核 gzip 瓶颈与重复底层传输） | **2–5 秒** | 100% 零改动（`devpilot-commit` 已原生支持 `zstd`） | **低**（镜像构建规范调整） | **P0 必选** |
| **D. 集群内 P2P 分发** | **Spegel**（无状态 DHT P2P）或 Dragonfly 分片并发 | **30–90 秒**（只要集群任一节点有缓存，走 25G/100G 内网拉取） | **2–5 秒** | 100% 零改动 | **中低**（`bz1-dev1` 已部署 Spegel，需深度调优） | **P1 核心** |
| **E. 按需懒加载（Lazy Pulling）** | **OCI `zstd:chunked`**（containerd soci/stargz）或 **Nydus（EROFS 内核态）** | **3–10 秒**（仅拉元数据索引 + 启动所需 3% 数据块） | **2–5 秒** | **高兼容，但需严格遵守 `s_stack_depth <= 2` 约束（见 §4.2）** | **中高**（需节点额外部署 Snapshotter 插件） | **P2 终局** |
| **F. 分布式块存储快照底座** | 将预解压 rootfs 存为 Ceph RBD / VAST `VolumeSnapshot`，开机秒级 COW 克隆挂载 | **5–12 秒**（与镜像大小完全解耦，$O(1)$ 克隆） | **5–12 秒** | 需扩展 `devpilot-init` 支持外挂块卷作为 `lowerdir`（实际上更简单，反而少套一层 overlay） | **中**（强依赖块存储 CSI 快照能力，需镜像转卷流水线） | **P2 备选/互补** |

---

## 3. 分阶段演进架构规划（P0 → P1 → P2）

结合 DevPilot 当前工程现状（M1–M5 已落地、`bz1-dev1` 具备 `containerd 2.0.5` + `Spegel` + NVMe 块存储），建议按 **三阶段渐进式落地**，在不推翻任何现有架构的前提下将开机体验从「十分钟级」收敛至「秒级」。

```text
开机请求 (desiredState: Running)
   │
   ├─► [第 1 道防线: P0 智能调度与预热] ──► 优先调度回上次节点 (lastNodeName) 或已预热节点 ──► 命中本地缓存 (2~5s Running!)
   │         │ (原节点满载 / 新建实例未命中)
   │         ▼
   ├─► [第 2 道防线: P0 Golden Base + P1 Spegel P2P + zstd] ──► 90% 基础 CUDA 层命中本机，仅拉增量层；或从邻近 GPU 节点 100G 内网并发拉取 + zstd 极速解压 (15~60s Running)
   │         │ (全新外部 100G 镜像，全集群首次出现)
   │         ▼
   └─► [第 3 道防线: P2 zstd:chunked / Nydus 按需懒加载 + 后台静默预取] ──► 挂载远程 TOC 索引按需读页启动 (5~10s Running)，后台限速补齐剩余层消除 import torch 卡顿
```

---

### 3.1 阶段一（P0，立竿见影，零底座侵入）：缓存亲和调度 + 声明式预热 + `zstd` 标准化

P0 阶段完全在 `devpilot` Operator、镜像规范与节点配置层面闭环，无需修改宿主机 containerd snapshotter，即可解决 **85% 以上的日常开机等待问题**。

#### 3.1.1 `DevBox` 历史节点软亲和（Sticky Cache Affinity）

- **现状痛点**：当前 `DevBox` 关机（`Stopped`）或被抢占（`Interrupted`）时，`status.nodeName` 会被清空；下次开机重新创建 Pod 时完全随机交由 K8s Scheduler 调度。哪怕集群有 10 台空闲 GPU 节点，Pod 也有 90% 概率落到没有缓存该用户镜像的节点上，白白重拉几十 GB。
- **规划设计**：
  1. 在 `DevBoxStatus` 中新增持久化字段 `lastNodeName`（每次成功进入 `Running` 时更新，关机/中断时**不清空**，仅在 `reset-rootdisk` 时清空）。
  2. `buildPod` 在构造开发机 Pod 时，若 `status.lastNodeName != ""` 且未发生强制节点排斥，自动注入软亲和（Soft Node Affinity）：
     ```yaml
     affinity:
       nodeAffinity:
         preferredDuringSchedulingIgnoredDuringExecution:
           - weight: 80
             preference:
               matchExpressions:
                 - key: kubernetes.io/hostname
                   operator: In
                   values: [<status.lastNodeName>]
     ```
  3. **效果**：只要上次运行的节点仍有空闲同款 GPU，100% 调度回原节点，本地 `containerd` OverlayFS 快照直接复用，**开机稳定在 2–5 秒**；若原节点已被占满，软亲和不会阻塞调度，自动漂移至其他空闲节点保底可用性。
  4. **同步开启 Kubelet / Scheduler `ImageLocality` 插件权重提升**：调高集群 `kube-scheduler` 中 `ImageLocality` 打分插件权重，使调度器天然倾向已拥有大体积基础层的节点。

#### 3.1.2 声明式镜像预热控制器（`ImagePrewarm` CRD）

针对平台提供的 **Top 10–20 官方高频基础镜像**（如 `pytorch:2.5-cuda12.4`、`vllm-openai:latest`、`cuda:12.6-devel`），不能等第一个倒霉用户开机时才去触发拉取。

- **新增轻量集群级/管理级资源 `ImagePrewarm`（`devpilot.wutz.dev/v1alpha1`）**：
  ```yaml
  apiVersion: devpilot.wutz.dev/v1alpha1
  kind: ImagePrewarm
  metadata:
    name: official-pytorch-25-cuda124
  spec:
    image: cr.example.com/public/pytorch:2.5.0-cuda12.4-ubuntu22.04
    # 仅在目标 GPU 池预热，避免污染纯 CPU 节点磁盘
    nodeSelector:
      nvidia.com/gpu.present: "true"
    tolerations:
      - key: nvidia.com/gpu
        operator: Exists
    # 滚动并发度限制，防止 100 台节点同时拉取打垮 Harbor 与业务网络
    maxConcurrency: 5
    # 是否钉住镜像防止被 Kubelet ImageGC 驱逐（可选配合 containerd pinned label）
    pinImage: true
    ttlHoursAfterComplete: 0
  status:
    desiredNodes: 32
    warmedNodes: 32
    failedNodes: 0
    resolvedDigest: sha256:8f4e2c9a...
  ```
- **核心设计要点**：
  1. **零 GPU 占用**：预热 Pod 使用 `pause` / `/devpilot-init --noop` 极简入口，`resources` **绝不申请 `nvidia.com/gpu`**（仅挂 `tolerations` 容忍 GPU 污点），拉完并完成 `containerd` 解包后立即退出（Job 模式）或转为 `1m CPU / 4Mi Memory` 睡眠哨兵。
  2. **并发流控（Concurrency Throttling）**：通过 `maxConcurrency` 控制分批预热（例如先预热 2 台种子节点，随后其余 30 台节点自动走 Spegel P2P 从这 2 台种子节点内网拉取！）。
  3. **新增 GPU 节点自动感知**：控制器 Watch `Node` 资源，新扩容入网的 GPU 节点在开放用户调度前（或入网第一时间）自动补齐预热任务。

#### 3.1.3 `DevBoxCommit` 变基预热闭环（消除 Rebase 后首次开机慢）

当用户执行 `DevBoxCommit` 并开启 `rebaseDevBox: true` 时，虽然 `devpilot-commit` 推送到 Harbor 是流式增量追加，但下次开机 Pod 引用的是新镜像 Digest：
- **好消息**：新镜像的前 $N$ 层与原基础镜像完全相同（Digest 100% 一致），若调度回 `lastNodeName`，`containerd` 对前 $N$ 层全部跳过下载与解包，**仅需拉取最后 1 层 `upper` 增量层**！
- **进一步优化**：`DevBoxCommit.spec` 可增加可选 `prewarmTargetNode: true`（默认对 `rebaseDevBox: true` 开启），在 Commit 推送成功后立即向 `lastNodeName` 异步下发一次无 GPU 预拉取任务，等用户真正点击“开机”时连增量层也已在本地解包完毕。

#### 3.1.4 节点本地存储与 Kubelet ImageGC 防抖治理（必做运维基线）

30–100 GB 镜像场景下最常见的生产事故是 **“缓存颠簸（Cache Thrashing）”**：节点系统盘只有 200–500 GB，拉取三四个 80 GB 镜像就触及 Kubelet 默认 `imageGCHighThresholdPercent: 85%`，刚预热好的底座立刻被 Kubelet GC 删掉，下一位用户开机又重新拉取 20 分钟。

- **规划规范**：
  1. **GPU 节点独立 NVMe 镜像盘**：将 `/var/lib/containerd` 挂载至独立高性能本地 NVMe SSD（容量建议 **>= 2 TB**，可容纳 20–30 个百 GB 级基础镜像及其共享层）。
  2. **Kubelet GC 阈值调优**：设置 `imageGCHighThresholdPercent: 85`、`imageGCLowThresholdPercent: 70`、`minimumGCAge: 24h`。
  3. **全链路统一 `zstd` 压缩**：
     - 平台官方基础镜像构建（`imagebuild` / CI）统一输出 `--compression=zstd --compression-level=3`；
     - `DevBoxCommit` 保持默认 `compression: zstd`（已在 M2 实现）；
     - 相比 `gzip`，`zstd` 在 `containerd 2.0` 下解压吞吐从 `~100 MB/s` 跃升至 **`500–1200 MB/s`**，单这一项即可将 70 GB 镜像的本地解压耗时从 6 分钟压缩至 **30–50 秒**。
  4. **Golden Base Layer（黄金公共底座）规范**：收敛平台官方镜像的底层 Dockerfile 继承树（例如所有 PyTorch 2.4/2.5、vLLM、Transformers 镜像统一继承同一份 `base-ubuntu22.04-cuda12.4` 镜像层）。一旦这 25 GB 公共 CUDA 层常驻所有 GPU 节点，任何衍生 50 GB 镜像实际需要拉取的差异层骤降至 **5–15 GB**。

---

### 3.2 阶段二（P1，集群级内网加速）：Spegel P2P 深度协同与大层分片优化

当原节点满载、开发机被迫跨节点调度到一台尚未缓存该用户自定义镜像的节点时，P1 确保**只要集群内任意一台节点曾经拉取过该镜像，新节点就无需回源 Harbor，直接走机房高速内网（25G/100G）秒级同步**。

#### 3.2.1 保留压缩 Blob（Spegel 生效前提核查）

`containerd` 默认配置 `discard_unpacked_layers = true` 会在镜像层解包到 OverlayFS snapshot 后删除 `/var/lib/containerd/io.containerd.content.v1.content/blobs` 下的原始压缩包以节省磁盘；**一旦开启该选项，Spegel 将无法向其他节点提供 P2P 分发**！
- **节点配置硬性约束**：GPU 节点 `containerd` 必须确保 `discard_unpacked_layers = false`（以独立本地 NVMe 空间换取全集群 P2P 缓存池能力）。

#### 3.2.2 针对百 GB 镜像的传输并发调优

在 `containerd` 配置（`/etc/containerd/config.toml`）中针对超大层调优：
- 提高并发下载层数：`max_concurrent_downloads = 10`（默认 3，70+ 层的镜像默认串行排队严重）；
- 提高单层 HTTP 分块并行度与连接复用，避免单个 10 GB CUDA 层阻塞整个拉取流水线；
- Spegel 配置仅针对私有 Harbor 与公共镜像源开启本地回退（`resolveLatestTag = false`，因 DevPilot 已在 `resolveImage` 阶段将所有镜像精确锁定为不可变 `@sha256:...` Digest，天然与 P2P 内容寻址 100% 契合！）。

---

### 3.3 阶段三（P2，终极冷启动方案）：按需懒加载（Lazy Pulling）与 `devpilot-init` 深度融合

对于 **100 GB 规模、低频长尾、或用户刚从外部导入的全新镜像**（全集群无任何节点缓存，P2P 也未命中），若仍要求把开机时间严格控制在 **10 秒以内**，核心路径是引入 **按需懒加载（Lazy Pulling）**。

#### 3.3.1 懒加载技术路线选型：`OCI zstd:chunked` vs `Nydus (EROFS)`

| 对比项 | 方案 E1：OCI 原生 `zstd:chunked`（eStargz / soci 演进标准） | 方案 E2：Dragonfly **Nydus**（RAFS v6 + Linux 内核 `EROFS`） |
| --- | --- | --- |
| **镜像制品兼容性** | **100% 兼容标准 OCI**！同一个镜像 Tag，安装了懒加载 Snapshotter 的节点秒级按需挂载；未安装的普通节点自动当成普通 `zstd` 镜像正常拉取，**无需维护双份镜像仓库** | 需将 OCI 镜像转换（或构建期双推）为 Nydus RAFS 格式镜像（Harbor 支持 Nydus 转换插件，但存在异步转换延迟） |
| **内核与挂载路径** | 用户态 FUSE daemon（`stargz-snapshotter`）按文件 chunk 发起 HTTP Range 请求 | **内核原生 `EROFS` + `fscache`（Linux 5.19+）** 或 FUSE 模式；EROFS 走内核页缓存，零用户态上下文切换，无 FUSE 守护进程 OOM 崩溃风险 |
| **与 `devpilot-commit` 集成成本** | **极低**：`devpilot-commit` 流式写 `zstd` 层时可直接生成 `zstd:chunked` Seeking Footer + TOC 索引，Commit 产物立即可懒加载 | 中：需在 `devpilot-commit` 集成 `nydus-image` 构建逻辑或依赖 Harbor 侧异步转换 |
| **冷启动耗时（100G 镜像）** | **4–8 秒** | **3–6 秒** |

**选型建议**：
- **若节点内核为 Linux 5.15（如当前 `bz1-dev1`）且追求零镜像转换负担**：优先采用 **`zstd:chunked`（搭配 `stargz-snapshotter` 开启后台预取）**，因为制品与标准 OCI 完全合一，`devpilot-commit` 产出的镜像天然支持，且对未启用懒加载的训练集群 100% 向后兼容。
- **若后续节点内核升级至 Linux 5.19+ / 6.x 且追求极致大并发 I/O 稳定性**：推荐演进至 **Nydus RAFS v6 + 内核态 `EROFS`（fscache 模式）**，彻底消除用户态 FUSE 进程开销。

#### 3.3.2 关键架构校验：懒加载 Snapshotter 与 `devpilot-init` 的兼容性分析

DevPilot 与普通无状态 Pod 不同，`devpilot-init` 会在容器启动瞬间执行二次 `overlay` 挂载、子挂载迁移与 `pivot_root`（详见 [持久化与镜像提交机制深潜](persistence-and-commit.md)）。引入懒加载前必须严格验证三大内核边界：

1. **Linux 内核 `FILESYSTEM_MAX_STACK_DEPTH = 2` 叠层深度校验（通过）**：
   - 在懒加载 Snapshotter（无论是 FUSE 挂载层还是 EROFS 挂载层）下，每个远程只读镜像层是一个独立的 `fuse` 或 `erofs` 文件系统挂载点，其内核 `sb->s_stack_depth = 0`。
   - `containerd` 将这些只读层拼装成容器初始根 `/` 时，使用的是第 1 层内核 `overlay`（`s_stack_depth = 0 + 1 = 1`）。
   - 随后 `devpilot-init` 将容器初始 `/` 只读绑定到 `/run/devpilot/base-lower`，并挂载 PVC `upper/` 组成 `merged` 时，这是第 2 层内核 `overlay`（`s_stack_depth = 1 + 1 = 2 <= FILESYSTEM_MAX_STACK_DEPTH(2)`）。
   - **结论**：叠层深度严格保持为 `2`，**不会触发内核 `EINVAL` 报错**！
2. **`metacopy=off,index=off` 与 Copy-Up 行为（通过）**：
   - 当用户在开发机内执行 `apt` 或修改某个尚未下载到本地的底层大文件时，内核 `overlay` 发起 Copy-Up 读取 `base-lower`，底层懒加载 Snapshotter 透明拦截该 `read()` 系统调用，通过 HTTP Range 从 Harbor/Spegel 拉取对应 chunk 后返回给内核完成 `upper/` 落盘，随后 `devpilot-commit` 看到的依然是完整的 `upper/` 文件。
3. **`RuntimeClass: nvidia` 驱动子挂载迁移（通过）**：
   - `nvidia-container-toolkit` 在 `devpilot-init` 运行前将宿主机 `libcuda.so.*`、`nvidia-smi` bind-mount 到初始 `/`，懒加载目录树中已有对应目录结构（元数据 TOC 在挂载瞬间已全量就绪），子挂载扫描与 `MS_BIND|MS_REC` 迁移行为 100% 一致。

#### 3.3.3 解决懒加载的“二次卡顿”陷阱：智能后台预取（Background Prefetch）

懒加载在 AI 交互式场景有一个著名痛点：**Pod 虽然 5 秒就 `Running` 连上了 SSH，但用户刚敲下 `python -c "import torch"`（需要顺序/随机读取几千个 `.py` 和几百 MB `.so` 动态库），终端瞬间卡住几十秒等待远程按需拉取小文件！**

为此，P2 阶段必须配套以下两项机制消除运行时抖动：
1. **开机即全量限速后台预取（Background Prefetch）**：
   - 容器在 5 秒内通过按需读取启动 `sshd` 与 `JupyterLab` 进入 `Running`；
   - 与此同时，节点侧 Snapshotter 立即在后台以受控带宽（如 `300–500 MB/s`，经由 Spegel P2P 或 Harbor）异步并发拉取该镜像剩余数据块填充本地缓存；
   - 用户从看到“运行中”、打开本地 VS Code、建立 Remote-SSH 连接、打开终端敲命令，通常有 **15–45 秒的人机交互间隔**——这段时间恰好被后台预取用来把核心 Python/CUDA 库静默拉到本地 NVMe！
2. **AI 框架优先预取清单（Prefetch Landmark / Priority TOC）**：
   - 在平台构建官方镜像或 `devpilot-commit` 打包时，将以下路径标记为高优先级预取段（eStargz `estargz.toc` landmark）：
     - `/etc/**`、`/usr/bin/**`、`/usr/sbin/sshd`
     - `/opt/conda/bin/**`、`/opt/conda/lib/python*/site-packages/torch/**`
     - `/usr/local/cuda/lib64/libcudart*`、`libcublas*`
   - 确保容器拉起后的前 10 秒内优先把 `import torch` 链路文件预热进页缓存。

---

### 3.4 架构备选与补充：模型/数据解耦与块存储底座快照

除了容器镜像层面的加速，在 AI 平台架构设计上还应从源头控制镜像膨胀，并评估块存储原生加速路径：

1. **严禁将大模型权重打入开发机基础镜像（源头减重）**：
   - 很多 100 GB 镜像之所以达到 100 GB，是因为用户把 30–70 GB 的 HuggingFace LLM 权重（如 Llama/Qwen）放进了 `/root/.cache/huggingface` 并 Commit 进了镜像。
   - **产品规范引导**：
     - 平台公共模型库统一预置在集群高性能共享并行文件系统（如 `shared-vast-cs` / `shared-juice`）的只读公共目录（如 `/public/models`），所有开发机默认挂载且**零拷贝秒级可见**；
     - `devpilot-commit` 默认排除提示或控制台扫描提醒：当检测到 `upper/` 下 `root/.cache/huggingface/hub/**` 超过阈值（如 `> 5 GiB`）时，在看板保存镜像弹窗中给出明确提示，引导用户将模型缓存软链或配置 `HF_HOME` 到持久化共享数据盘 PVC，使环境镜像回归 **15–30 GB** 的合理区间。
2. **基于 Ceph RBD / VAST `VolumeSnapshot` 的预解压块设备底座（备选路线 F）**：
   - 若未来某集群因安全合规禁止在宿主机部署第三方 FUSE/Nydus Snapshotter，可利用 `bz1-dev1` 现有的块存储秒级 COW 快照能力：
   - 平台将高频 100 GB 镜像提前解包到一块模板黄金 PVC 并生成 `VolumeSnapshot`；开发机创建时，不以容器镜像承载庞大的 AI 环境，而是用一个 `< 50 MB` 的极简引导镜像启动 Pod，同时从黄金快照秒级（`< 3s`）COW 克隆出只读/读写块存储卷挂载给 `devpilot-init` 直接作为根文件系统（甚至省去一层 OverlayFS！）。
   - **权衡**：该路线对 K8s 节点完全零插件侵入且冷启动恒定在 5–10 秒，但需要维护“OCI 镜像 ↔ 块存储黄金快照”的异步转换流水线，适合作为不便改造节点 Runtime 时的强力兜底方案。

---

## 4. 产品交互、计费与可观测性协同改进

在将拉取耗时压缩至秒级的同时，针对不可避免的极端冷拉取场景（如用户指定了一个全新的外部 `nvcr.io` 80 GB 镜像），必须在产品体验上消除“黑盒盲等”：

### 4.1 细粒度拉取状态与缓存命中可视化

扩展 `DevBox.status` 与内置看板（`internal/dashboard`）在 `Starting` 阶段的信息透明度：
- 在 `StartingStep == PullingImage` 时，结合 Pod Event 与节点镜像检查区分三种子状态展示给用户：
  - `CacheHit`：「已命中节点本地镜像缓存，正在挂载持久化根盘（预计 < 5 秒）」
  - `P2PPulling`：「正在通过集群高速内网同步镜像（P2P 加速中）」
  - `ColdPulling`：「首次拉取外部超大镜像（建议等待完成，后续开关机将秒级启动）」
- 若节点 Kubelet 开启 `CRI Image Pull Progress` 事件上报，同步提取已拉取字节百分比展示在控制台卡片上，避免用户误判死机而反复点击重启。

### 4.2 先预拉取、后占卡（可选高级调度优化，消除平台 GPU 空转）

针对极端外部冷镜像（预计拉取 > 5 分钟）且集群 GPU 紧张的场景，可在 P1/P2 引入 **两阶段启动（Two-Phase Pre-Pull）**：
1. Operator 检测到目标候选 GPU 节点均无该镜像缓存且镜像体积 > 阈值（如 `> 20 GiB`，可通过读取 Manifest 各层 `size` 求和秒级获知！）时：
2. 先在选定的目标节点上创建一个 **零 GPU 请求（`gpus: 0`）** 的轻量 `prepull` Pod 将镜像拉取解包落盘；在此期间该节点的 GPU 仍可被其他短时任务使用，不锁死稀缺算力；
3. 待 `prepull` 完成后，立即在同一节点（`nodeName` 精确绑定）拉起真正的 GPU `DevBox` Pod，此时 GPU 锁定到进入 `Running` 仅需 **2–3 秒**！

---

## 5. 路线图与预期量化收益总结

| 阶段 | 核心交付项 | 涉及改造范围 | 预期开机启动耗时（30–100 GB 镜像） | 建议优先级 |
| --- | --- | --- | --- | --- |
| **P0：调度亲和 + 预热 + zstd 规范** | 1. `DevBoxStatus.lastNodeName` 软亲和调度<br>2. `ImagePrewarm` CRD 预热官方热门底座<br>3. `DevBoxCommit` 变基后置预拉取<br>4. 节点 `/var/lib/containerd` 独立 NVMe + GC 水位调优 + 全面默认 `zstd` + Golden Base 层收敛<br>5. 看板镜像体积预警（引导 `HF_HOME` 剥离权重） | `devpilot` Operator、`charts/devpilot`、节点运维配置（**零容器运行时插件改造**） | • **关机再开 / 热门官方底座**：`10–30m` ➔ **2–5 秒**<br>• **Commit 变基重启**：`15m` ➔ **3–10 秒**<br>• **无缓存纯冷拉**：`20–30m` ➔ **4–8 分钟** | **最高（推荐立即纳入 M6）** |
| **P1：Spegel P2P 深度加速 + 两阶段占卡优化** | 1. 节点 `discard_unpacked_layers=false` 固化与 Spegel 大镜像并发调优<br>2. Manifest 体积探测（`> 20 GiB` 冷镜像自动两阶段无卡预拉取，消除 GPU 空转）<br>3. 控制台缓存命中态与拉取进度提示 | `devpilot` Operator、集群 `Spegel` / `containerd` 参数 | • **同集群任一节点已有的跨节点冷调度**：`10–30m` ➔ **20–60 秒**<br>• **GPU 算力空转损耗**：降低 **95%+** | **高（紧随 P0 落地）** |
| **P2：按需懒加载终局架构** | 1. GPU 节点部署 `zstd:chunked` Snapshotter（或 Linux 6.x 下 Nydus EROFS）+ 开机后台限速全量预取<br>2. `devpilot-commit` 流式直出 `zstd:chunked` 带 TOC 索引增量层 + PyTorch 热点文件优先预读列表 | `devpilot-commit`、节点 Containerd Snapshotter 插件 | • **全集群从未见过的 100 GB 全新冷镜像**：`10–30m` ➔ **5–10 秒**（且无 `import torch` 二次卡顿） | **中长期演进（M7+）** |
