# 接入体系与安全边界

机器学习开发机同时面向**终端重度用户**（VS Code / Cursor Remote-SSH、PyCharm 远程解释器、`scp`/`rsync`）与**浏览器交互用户**（JupyterLab、TensorBoard、Gradio/Streamlit 端口预览），且用户默认在容器内拥有 `root` 身份以执行 `apt install` 等系统级包管理。因此，DevPilot 必须同时提供顺滑的接入体验与清晰的宿主机隔离防线。

---

## 1. SSH 直连架构（VS Code / Cursor Remote-SSH 完备支持）

### 1.1 为什么采用主容器内 SSH 而非 Sidecar

在 `forgeai` 早期针对 Agent 的 `Sandbox` 模型中，SSH 运行在独立的 `sshd` Sidecar 容器里，只与主容器共享挂载的数据卷。对于机器学习开发机而言，Sidecar 模式有两个致命缺陷：
1. SSH 会话进入的是 Sidecar 镜像，**看不到主容器内安装的 Conda/Python 环境、CUDA 工具链以及持久化 `upper/` 系统盘**；
2. 无法直接查看、调试或 `gdb`/`py-spy` attach 主容器正在跑的训练进程。

在 DevPilot 中，`sshd` 直接由 `devpilot-init` 在 `pivot_root` **之后的同一容器根文件系统与进程/GPU 命名空间**中拉起：
- 若用户基础镜像已安装 OpenSSH Server（`/usr/sbin/sshd`），`devpilot-init` 生成独立配置 `/run/devpilot/sshd_config` 启动它，不污染 `/etc/ssh/sshd_config`；
- 若用户基础镜像是未装 `openssh-server` 的极简镜像，`devpilot-init` 内置基于 Go `golang.org/x/crypto/ssh` + SFTP + 本地端口转发的内嵌 SSH 服务兜底（确保任意镜像开箱即连，且支持 VS Code Remote Server 下载与连接）。

### 1.2 持久化 HostKey 与登录凭据

1. **HostKey 跨重启不变**：Operator 在创建 `DevBox` 时生成一对 `ed25519` 主机密钥存入 `<devbox>-hostkey` Secret（键 `ssh_host_ed25519_key` 与 `ssh_host_ed25519_key.pub`，命名契约与 `sshgateway` 100% 一致）。开发机无论开关机多少次、跨多少台 GPU 节点重新调度，客户端 `known_hosts` 永不报警。
2. **双认证模式**：
   - 公钥登录：将用户 `authorizedKeys` 及（当启用 `sshgateway` 时）网关 `sshgateway-identity` 公钥写入受管公钥源；
   - 可选密码登录：当配置 `passwordSecretRef` 时同步设置目标用户密码哈希，兼容传统控制台展示掩码密码与一键复制 SSH 命令的交互习惯。

### 1.3 网关暴露与 `sshgateway` 协同

支持两种对外暴露拓扑：

1. **统一网关路由模式（推荐，对接 `sshgateway.wutz.dev`）**：
   - Operator 自动在开发机命名空间创建 ClusterIP Service `<devbox>`（端口 22）及 `SSHRoute`，将用户名 `<namespace>-<devbox>`（或自定义路由用户名）映射至后端 Service。
   - **VS Code Remote-SSH 转发要求说明**：当前 `sshgateway` 默认开启了 `--disable-local-forwarding`，适合纯终端交互；而 VS Code / Cursor Remote-SSH 依赖 SSH `direct-tcpip` 本地端口转发与远端 `.vscode-server` 通信。因此对接开发机流量时，建议在 `SSHGateway` 侧为开发机网关实例开启本地转发（允许转发回环地址 `127.0.0.1`），或使用独立 Gateway Listener 承载开发机路由。
2. **LoadBalancer / NodePort 直通模式**：
   - 在未安装 `sshgateway` 的集群中，支持按需分配端口或通过 Envoy Gateway / Cilium L4 路由暴露 TCP 22。

---

## 2. Web 服务接入（JupyterLab / TensorBoard / 自定义端口）

结合开发环境集群约定（如 `bz1-dev1` 集群对外访问 Ingress 统一使用端口 **`7443`**）：

```text
浏览器 HTTPS https://jupyter-<devbox>-<ns>.<domain>:7443
   │
   ▼
Gateway API (HTTPRoute) / Ingress Controller (TLS 终止, 监听 7443)
   │
   ▼
ClusterIP Service <devbox> (8888 / 6006 / <custom-port>)
   │
   ▼
DevBox Pod 内 JupyterLab / TensorBoard / Web 服务
```

1. **JupyterLab 零配置启动**：当 `spec.access.jupyter.enabled: true` 且容器内存在 `jupyter-lab` / `jupyter` 时，`devpilot-init` 自动以 `--ServerApp.token=<secret-token> --ServerApp.ip=0.0.0.0 --ServerApp.allow_origin=*` 在后台托管运行，并将完整带 Token 或免密跳转地址写入 `status.endpoints.jupyter`。
2. **多端口按需预览**：`spec.access.ports[]` 声明的每个 HTTP 端口都会自动生成对应的子域名路由 `<port-name>-<devbox>-<ns>.<domain>:7443`。当开发机处于 `Stopped` 或 `Interrupted` 时，路由保留或返回友好的停机状态提示页，避免悬挂连接。
3. **WebSSH 终端**：平台控制台或内置看板可通过 K8s `pods/exec` WebSocket（或连接容器内 SSH）提供浏览器一键 WebSSH，同时定期刷新活跃心跳注解 `devpilot.wutz.dev/last-active` 防止触发闲置关机。

---

## 3. 安全边界与最小权限模型

因为用户需要在容器内以 `root` 执行 `apt`、编译代码，并且 `devpilot-init` 需要在启动瞬间挂载内核态 `overlay` 并执行 `pivot_root`，安全边界必须在设计上层层收口：

### 3.1 严禁 `privileged: true`，最小化 Linux Capabilities

| 安全配置项 | DevBox 主容器取值 | 防护作用 |
| --- | --- | --- |
| `privileged` | **`false`（硬性禁止）** | 防止宿主机全部 `/dev/nvme*`、宿主机根盘设备与其他租户的 `/dev/nvidia*` 设备节点暴露进容器（最核心的容器逃逸防线） |
| `hostNetwork` / `hostPID` / `hostIPC` | **`false`** | 隔离宿主机网络栈、进程树与共享内存命名空间 |
| `hostPath` 卷 | **零使用** | 容器只挂载 K8s 受管 PVC、`emptyDir`、Secret/ConfigMap，不接触宿主机任何目录或 `containerd.sock` |
| `capabilities.drop` | `["ALL"]` | 先清空默认能力集 |
| `capabilities.add` | 常规标准集（`CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `KILL`, `SETGID`, `SETUID`, `NET_BIND_SERVICE`）+ 启动切根所需的 `SYS_ADMIN`, `SYS_CHROOT`, `MKNOD` | 支持 `overlay` 挂载、`pivot_root` 与 `apt`/`dpkg` 包解包管理 |
| **切根后能力裁剪（Bounding Set Drop）** | **`devpilot-init` 在完成 `pivot_root` 后，拉起 `sshd` 和用户进程前主动通过 `prctl(PR_CAPBSET_DROP)` 移除 `CAP_SYS_ADMIN`！** | **核心安全加固**：用户交互会话及所有子进程（即使 UID=0）的 Capability Bounding Set 中已无 `CAP_SYS_ADMIN`，用户在容器内既不能卸载根盘、也不能手工挂载新文件系统或触发 `CAP_SYS_ADMIN` 相关内核攻击面，而 `apt install`、`conda`、CUDA 训练完全不受影响！ |
| `seccompProfile` | `RuntimeDefault`（或平台增强 Profile） | 拦截 `kexec_load`、`open_by_handle_at`、`bpf`、`perf_event_open`、`userfaultfd` 等高危内核系统调用 |
| `automountServiceAccountToken` | **`false`** | 开发机容器内部无需调用 K8s API Server，彻底阻断容器内窃取 ServiceAccount Token 横向渗透命名空间的途径 |

> **演进备选（K8s User Namespaces + `userxattr`）**：在 K8s 全面启用 `UserNamespacesSupport`（Linux 内核 >= 5.11 支持 OverlayFS `userxattr` 挂载选项）的环境中，容器内 `root (UID 0)` 将映射为宿主机非特权高段 UID，届时甚至连容器初始启动瞬间的宿主级 `CAP_SYS_ADMIN` 都无需授予。当前「切根后立即从 Bounding Set 永久剥离 `CAP_SYS_ADMIN`」方案在现有 K8s 1.32 + NVIDIA RuntimeClass 生产环境下兼容性最好（当前 `nvidia-container-toolkit` 对 User Namespace 映射仍在演进完善中）且安全性极高。

### 3.2 镜像仓库凭证隔离（对齐 `imagebuild` 契约）

- **拉取凭证（Pull）**：开发机 Pod 通过 `imagePullSecrets` 引用只读拉取密钥，kubelet 在节点侧完成镜像拉取，容器内部文件系统中不存在任何 Docker `config.json`。
- **推送凭证（Push）**：运行 `DevBoxCommit` 时，Commit Job 运行在受控的 `<project>-build`（或 `devpilot-system`）命名空间中，普通项目成员对该命名空间的 Secret **零读取权限**，只能通过 `DevBoxCommit.status` 和只读 Job 日志查看构建进度与镜像摘要，确保租户无法提取高权限 Registry Robot Push Token 覆盖他人镜像。

### 3.3 多租户网络与 RBAC 集成

当集群安装了 `workspace.wutz.dev`（`project` Operator）时：
- DevPilot 提供带标签 `workspace.wutz.dev/aggregate-to-member: "true"` 的聚合 `ClusterRole`（`devpilot-member-edit` 与 `devpilot-member-view`），使项目成员天然具备自身命名空间下 `DevBox` 与 `DevBoxCommit` 的管理权限；
- 遵循项目命名空间默认 `NetworkPolicy`：不同项目的开发机之间东西向网络默认隔离，仅放行来自 `sshgateway-system`（端口 22）与 Ingress Gateway（HTTP 暴露端口）的入站流量。
