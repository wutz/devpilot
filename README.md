# DevPilot

面向机器学习平台的 Kubernetes 交互式开发机（DevBox）Operator。

DevPilot 聚焦解决 K8s 容器开发机的两大核心工程问题：

1. **关机后全系统修改持久化与跨节点自由调度**：通过块存储 PVC（Ceph RBD / VAST Block）持久化内核态 OverlayFS `upperdir`，由静态注入的 `devpilot-init` 完成切根与子挂载迁移。用户通过 `apt`、`conda`、`pip`、源码编译写入 `/usr`、`/etc`、`/opt`、`/root` 的任意变更及文件删除记录（whiteout）在关机或抢占驱逐后完整保留，开机时可调度至任意空闲 GPU 节点秒级恢复。
2. **零宿主机耦合的流式固化镜像（Commit & Push）**：无需挂载宿主机 `containerd.sock`、无需在构建节点拉取几十 GB 基础 CUDA 镜像，`devpilot-commit` 将持久化 `upperdir`（支持停机离线提交与基于 CSI `VolumeSnapshot` 的在线热提交）流式转换为标准 OCI 增量层，直接追加到远端镜像仓库（Harbor / GHCR）的基础镜像 Manifest 上。

> 状态：M1 编码中。`devpilot-init` 与 `DevBoxReconciler` 的核心开机/关机全盘持久化逻辑已实现并通过 envtest，待 `bz1-dev1` 实测验证。

## 核心架构

```text
DevBox (devpilot.wutz.dev/v1alpha1)                DevBoxCommit (devpilot.wutz.dev/v1alpha1)
   │  开关机 / 抢占中断 / 闲置停机 / 计费段 / GC         │  停机直读 (Stopped) / CSI 快照克隆 (Running)
   ▼                                                  ▼
┌──────────────────── GPU 节点 ────────────────────┐ ┌────────────── 隔离构建任务 (Commit Job) ──────────────┐
│ DevBox Pod (RuntimeClass: nvidia)                │ │ 非特权 Pod (privileged: false, 无 containerd.sock)    │
│  ├─ initContainer: 注入静态二进制 devpilot-init    │ │  ├─ 只读挂载 rootdisk PVC 或快照恢复的临时 PVC          │
│  └─ devbox 容器:                                  │ │  ├─ 扫描 upper/ 转换 0:0 whiteout 与 opaque xattr     │
│       1. 校验 .meta/base-image.json 锁定镜像摘要   │ │  ├─ 剔除 /tmp、缓存与平台注入密钥，流式压缩 zstd/gzip  │
│       2. mount overlay (base / + PVC upper/)     │ │  └─ go-containerregistry 远端追加层推送至 Harbor       │
│       3. rbind 迁移 nvidia-smi / libcuda / 共享盘 │ └──────────────────────────────────────────────────────┘
│       4. pivot_root → 拉起 sshd / Jupyter / CMD  │
└────────────────────────┬─────────────────────────┘
                         │ ReadWriteOnce 块存储 (block-nvme / block-vast-cs)
                         ▼
             rootdisk PVC (upper/ + work/ + .meta/)
```

## 核心特性与平台对齐

| 能力维度 | 设计机制 | 关联生态契约 |
| --- | --- | --- |
| 全盘持久化 | 块存储 PVC 承载 `upper/` + `work/`，内核原生 `overlay` 零 FUSE 元数据损耗 | `block-nvme` (Ceph RBD)、`block-vast-cs` (VAST Block) |
| GPU 驱动兼容 | `pivot_root` 前自动枚举 `/proc/self/mountinfo` 迁移 `nvidia-container-toolkit` 注入的驱动与设备挂载 | `RuntimeClass: nvidia`、HAMi vGPU、RDMA |
| 开关机与抢占 | 关机删 Pod 释放 GPU/CPU/内存；抢占驱逐转入 `Interrupted` 保留系统盘与共享盘；连续关机/中断超期（默认 15 天）自动回收 | 对齐 `ai-prd` 容器实例抢占语义与 `trainpilot` 计费段模型 |
| 在线/离线保存镜像 | 离线直读 PVC；在线通过 CSI `VolumeSnapshot` 秒级 COW 快照克隆临时只读卷，不中断 GPU 训练进程；支持可选 Rebase 清空 `upper/` | `csi-rbdplugin-snapclass`、`vast-snapshot-cs`、Harbor `project-registry` |
| 开发环境接入 | 容器内同命名空间 `sshd`（VS Code / Cursor Remote-SSH 直通完整 rootfs 与 GPU）+ JupyterLab / WebIDE 路由 | `sshgateway` (`SSHRoute`)、Gateway API / Ingress (`:7443`) |

## 文档目录

- [架构总览与方案选型](docs/architecture.md) — 方案对比、整体拓扑、关键设计决策（D1–D12）与核心工作流
- [持久化与镜像提交机制深潜](docs/persistence-and-commit.md) — `devpilot-init` 切根细节、NVIDIA 子挂载迁移、Whiteout 转 OCI 层、CSI 热快照与 Rebase
- [Operator 与 CRD 规范](docs/operator-and-crd.md) — `DevBox` 与 `DevBoxCommit` 完整字段定义、状态机、计费段与生命周期 GC
- [接入体系与安全边界](docs/access-and-security.md) — SSH 直连、Jupyter/Web 路由（端口 `7443`）、最小权限 `SecurityContext` 与凭证隔离
- [演进路线图](docs/roadmap.md) — M0–M4 阶段划分与集群端到端验收标准

## 示例清单

- [PyTorch 开发机示例](deploy/examples/devbox-pytorch.yaml)
- [在线固化镜像示例](deploy/examples/devbox-commit.yaml)

## 许可

Apache License 2.0
