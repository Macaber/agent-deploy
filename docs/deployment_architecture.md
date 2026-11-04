# Agent Workspace 部署架构设计与组件作用解析

本项目基于 Kubernetes 构建了一套面向企业/团队的**动态开发工作空间（Workspace On Demand）**托管平台。系统通过 API-Server 网关和 Operator 控制器，实现了开发环境的一键拉起、资源限额与会话超时休眠。

> **存储底座已定版**：生产环境统一采用 **OSS 对象存储卷 + ossfs 1.0**（CSI `fuseType: ossfs`）；NAS/NFS 与 Local Path 仅作为附录备选，阿里云 ACK 部署包暂不启用。结论与依据见 [storage_plan.md](storage_plan.md)。

以下是系统的整体部署架构设计以及各核心组件在其中扮演的角色与协同工作原理。

---

## 1. 整体架构与流量拓扑

系统各组件的协同关系与请求流量流向如下：

```mermaid
graph TD
    Client[开发人员浏览器] -->|1. 访问 IP/Path| ExtNginx[外部 Nginx 代理]
    ExtNginx -->|2. 重写 Host Header & 转发| K8sIngress[Ingress-Nginx Controller]
    
    UI[Launcher 网页 / Web Portal] -->|3. 创建/停止/唤醒请求| APIServer[API-Server 网关]
    APIServer -->|4. 调用 Client-Go 控制 CR| K8sAPI[K8s API Server]
    
    Operator[Workspace Operator] -->|5. 监听并调和 CR 状态| K8sAPI
    
    subgraph K8s 内部集群
        K8sIngress -->|6. 流量负载| K8sSvc[Workspace Service]
        K8sSvc -->|7. 穿透到主端口| WorkspacePod[Workspace Container]
        
        Operator -->|创建 / 管理| WorkspacePod
        Operator -->|创建静态 PV/PVC| WorkspacePVC[Workspace PVC]
        
        WorkspacePVC -->|8. 静态绑定子路径 /workspaces/ws-name| OSSPV[OSS CSI 静态 PV]
        OSSPV -->|9. Attach 时创建 FUSE Pod| FusePod[ossfs FUSE Pod]
        FusePod -->|10. ossfs 1.0 读写| OSSBucket[OSS Bucket]
        NodePlugin[csi-plugin-oss 节点插件] -->|bind-mount 挂载点| WorkspacePod
    end
```

---

## 2. 核心组件作用与职责解析

整个部署方案由 **CRD 与 Operator**、**Ingress 网关**、**OSS CSI 存储层（ossfs 1.0）** 以及 **API-Server** 四大板块构成，它们各司其职、紧密配合：

### 🧬 (1) CRD (自定义资源) 与 Workspace Operator

* **组件构成**：自定义资源声明 `workspaces.ai.example.com`（CRD）与后台控制器 `controller-manager`（Operator）。
* **核心作用**：**整个工作空间生命周期的“大脑”与执行引擎。**
* **具体职责**：
  * **资源状态监听**：不断轮询监听 K8s 集群中 Workspace 实例的创建、修改和删除事件。
  * **子资源编排（一键拉起）**：根据 Workspace 的声明规格，自动创建并管理对应的 `PersistentVolumeClaim`（持久化卷）、`Deployment`（计算负载）、`Service`（内网暴露）和 `Ingress`（外部路由）；计算与网络子资源建立 **Owner Reference（所有者引用）** 确保级联删除，而存储卷（PVC/PV）不绑定 Owner Reference，Workspace 删除后数据与卷对象持久保留。
  * **OSS 静态卷自动创建**：当 `spec.storage.storageClass`（或集群默认存储类）命中 OSS 时，Operator 直接创建静态 PV（名称 `<workspace>-pv`，子路径 `/workspaces/<workspace-name>`，`ReadWriteMany`，`reclaimPolicy: Retain`），再创建精确绑定该 PV 的 PVC（`<workspace>-pvc`）。SC 中的 `bucket` / `url` / `otherOpts` / `fuseType` / `sigVersion` / `region` / Secret 引用会被透传到 PV。
  * **会话超时自动休眠（`idleTimeout`）**：以 `Status.LastActiveTime` 为起点计时（创建/API 唤醒/从非 Running 进入 Running 时更新），**不是**根据 Ingress 实时流量判断“是否有人在用”。超过 `idleTimeout` 后 Operator 将 Deployment 副本缩至 `0`，状态为 `Sleeping`。唤醒需通过 API（更新 `lastActiveTime`）或将 `stopped` 置回 false 后由控制器拉起。
  * **副本管理**：Deployment 的 `replicas` 始终由 Operator 根据 `stopped` / `idleTimeout` 写入，无需额外扩缩容组件。

### 🌐 (2) Ingress (网关路由层)

* **组件构成**：`ingress-nginx-controller`（基于 Nginx 的 Kubernetes 官方网关控制器）。
* **核心作用**：**南北向流量入口与二级域名路由转发。**
* **具体职责**：
  * **端口绑定与网络透传**：在物理节点上通过 `hostNetwork: true` 模式运行，直接监听集群物理机的 `80` 和 `443` 端口，实现高性能的流量接入。
  * **动态域名解析**：当 Operator 决定启动一个工作空间时，会为其自动创建一个 Ingress 路由规则。Ingress 监听请求的 `Host` 头（如 `ws-dev-user01.bocomwork`），根据域名将流量精确分发到对应用户的 `Service` 上。
  * **长连接（WebSocket）支持**：为开发环境的网页终端（Terminal）、代码热编译等提供持久的、低延迟的 WebSockets 双向通道代理。

### 💾 (3) OSS 存储层：CSI 驱动 + ossfs 1.0（当前唯一生产方案）

* **组件构成**：
  * `deploy/oss/csi-controller.yaml`：CSI **控制面** Deployment `csi-controller-oss`（1 副本），负责 Attach/Detach，并在目标节点创建/删除 FUSE Pod；
  * `deploy/oss/csi-plugin.yaml`：CSI **节点面** DaemonSet `csi-plugin-oss`（每节点 1 个），向 kubelet 注册驱动、执行 NodePublish/Unpublish、把 FUSE 挂载点 bind-mount 进业务 Pod；
  * **FUSE Pod**：每个卷一个，运行在 `ack-csi-fuse` 命名空间，内部运行 **ossfs 1.0** 客户端，真正完成 OSS 挂载与读写；
  * `deploy/oss/storageclass.yaml`：StorageClass `alicloud-oss`，声明 Bucket、内网 Endpoint 与 ossfs 1.0 挂载参数；
  * `deploy/oss-public/`：公共共享 OSS PV/PVC（`global-oss-share-pv` / `global-oss-share-pvc`）。
* **核心作用**：**数据持久化、跨节点漂移与多用户物理隔离。**
* **具体职责**：
  * **个人空间隔离**：Operator 为每个 Workspace 固定使用 `/workspaces/<workspace-name>` 子路径，天然实现按用户/工作空间的目录隔离；容器内表现为 `/workspace`（以及 opencode 的 `config` / `share` / `state` / `cache` 子路径）。
  * **工作现场持久化**：开发人员在 `/workspace` 中写入的代码、会话日志、插件配置或 LLM 聊天记录，经内核 FUSE 由 ossfs 异步上传至 OSS Bucket。
  * **无损休眠与重启**：工作空间休眠（副本缩 0）或漂移到其他节点重启时，计算资源被销毁重建，但 OSS 上数据不受影响；新 Pod 重新挂载同一子路径即可恢复现场。
  * **共享资产**：公共工具包等读多写少资源通过独立共享 Bucket 前缀与 RWX PVC 下发，挂载到 `/data/oss-shared` 等路径，与个人数据隔离。
  * **删除保护**：删除 Workspace 时 Operator **不删除** PV/PVC，也不清空 OSS 前缀；数据由管理员显式清理。
* **为什么固定 ossfs 1.0**：Agent 的 edit、编译、`npm/pip` 安装均为随机写密集场景，ossfs 2.0 官方不建议该场景使用；1.0 的 `-o max_stat_cache_size=100000 -o allow_other` 参数成熟稳定。

### 🔌 (4) API-Server (轻量级网关)

* **组件构成**：一个独立编译部署 of Go Web 服务，挂载了专用的 ServiceAccount `api-server-sa`。
* **核心作用**：**前端 Portal 与 Kubernetes 底层集群之间的通信桥梁。**
* **具体职责**：
  * **屏蔽集群复杂度**：对外（面向网页 Launcher 或三方平台）提供简洁 of HTTP RESTful 接口（创建、唤醒、停止、查询状态），避免外部系统直接使用复杂的 Kubernetes 证书和 SDK。
  * **用户身份与参数映射**：接收 HTTP 请求中的自定义参数（如自定义镜像、环境变量、挂载路径 `volumeMounts`、容器入口 `command`、容器后置脚本 `postStartScript` 以及调度命名空间 `namespace`），并在校验后组装成标准的 K8s CR 结构体提交给集群。
  * **状态轮询与阻塞等待**：在拉起或唤醒时，提供阻塞机制（90 秒超时），API-Server 会在后台监听 Pod 是否 Ready，并在容器完全可以提供服务时才向请求端返回成功状态，确保极佳的用户体验。

---

## 3. 组件间的生命周期协同工作流

我们以一个典型的**创建工作空间**过程，展示各组件的协同配合：

```text
 1. 外部请求 -> API-Server (带参数 {"userId": "user1", "namespace": "dev-ns", "port": 1234})
 2. API-Server -> 在 K8s 对应命名空间创建名为 "ws-user1" 的 Workspace 自定义对象
 3. Operator (监听到事件) -> 开始调和：
    a. 创建静态 PV ws-user1-pv（path=/workspaces/ws-user1，RWX，Retain）并创建 PVC ws-user1-pvc 精确绑定
    b. 创建 Deployment (注入用户指定的镜像、端口 1234、挂载参数及 postStart 脚本，/workspace 使用子路径 workspace)
    c. 创建 Service (暴露 Deployment 的 http 端口)
    d. 创建 Ingress (绑定域名 ws-user1.bocomwork Service 80 端口)
 4. Pod 调度到节点 -> kubelet 发起 Attach：
    a. csi-controller-oss 在目标节点 ack-csi-fuse 命名空间创建 FUSE Pod
    b. FUSE Pod 内 ossfs 1.0 挂载 bucket:/workspaces/ws-user1 到宿主机挂载点
    c. csi-plugin-oss 执行 NodePublish，把挂载点 bind-mount 到 Pod 卷目录
 5. Pod 启动成功 -> 运行后置脚本 (postStartScript) -> 容器服务就绪 (处于 Running)
 6. Ingress Controller 刷新配置 -> 将 http://ws-user1.bocomwork 接入后端 Pod
 7. API-Server 轮询检测到 Workspace 状态变为 Running -> 向外部返回 "http://ws-user1.bocomwork" 访问入口
 8. 开发人员通过配置了 Cookie 转发的外部 Nginx 访问 IP/bocomwork/ws/user1/ -> 开始愉快编码
```

> 停止/休眠链路是上述流程的逆过程：副本缩至 0 → NodeUnpublish 触发 ossfs flush → Controller 删除该卷的 FUSE Pod → PVC/PV 保留（Retain），OSS 数据不动。写入/关闭的时序风险与缓解建议见 [oss_storage_architecture.md](oss_storage_architecture.md)。

---

## 4. 存储方案边界（定版）

| 方案 | 当前定位 | 文档位置 |
| :--- | :--- | :--- |
| **OSS + ossfs 1.0** | **生产唯一方案** | [offline_deployment_guide.md](offline_deployment_guide.md) 主线、[oss_storage_architecture.md](oss_storage_architecture.md) |
| Local Path 本地物理盘 | 附录备选（演练/性能对照/临时环境） | [offline_deployment_guide.md](offline_deployment_guide.md) 附录 A |
| NAS / NFS 网络共享 | 附录备选（存量迁移过渡/应急回退） | [offline_deployment_guide.md](offline_deployment_guide.md) 附录 B |
| 阿里云 ACK 部署包 | **暂不启用**（后续上云参考） | [offline_deployment_guide.md](offline_deployment_guide.md) 附录 C、[../deploy-ack/README.md](../deploy-ack/README.md) |
