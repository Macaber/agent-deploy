# Workspace 自定义资源 (CRD) 使用配置手册

本文档是 `Workspace` 自定义资源的完整参数配置和状态监控使用手册。它详细列出了 Spec (规格配置) 和 Status (状态反馈) 的全部参数作用、格式规范及配置示例。

---

## 1. 资源声明基础模版

在 Kubernetes 中，工作空间的定义采用如下标准声明：

```yaml
apiVersion: ai.example.com/v1alpha1
kind: Workspace
metadata:
  name: ws-alice               # 1. 工作空间资源的唯一标识（建议用 ws-<username>）
  namespace: default           # 2. 部署的 Namespace 命名空间
spec:
  owner: alice                 # 3. 拥有者用户 ID
  idleTimeout: 30m             # 4. 空闲超时自动睡眠时间
  exposeSSH: true              # 5. 是否暴露 SSH 端口
  stopped: false               # 6. 手动启停状态位
  runtime:                     # 7. 容器运行规格定义
    image: smanx/opencode:latest
    port: 4096
    cpu: "1"
    memory: "2Gi"
    env:
      - name: OPENCODE_SERVER_PASSWORD
        value: "mysecret123"
  storage:                     # 8. 存储配额与策略
    size: 10Gi
    storageClass: standard
```

---

## 2. 配置参数详解 (Spec)

`spec` 字段用于定义您期望该工作空间达到的运行规格，包含容器、存储、休眠、网络等属性。

### A. 基础管理参数

| 参数名 | 数据类型 | 是否必填 | 作用描述 | 示例值 |
| :--- | :--- | :--- | :--- | :--- |
| **`owner`** | `string` | **是** | 声明该独立工作空间归属的用户 ID。 | `"alice"`, `"user_1001"` |
| **`stopped`** | `boolean` | 否 | 手动启停控制开关。<br>设为 `true`：触发手动挂起，副本数降为 0（容器销毁以释放 CPU/内存算力，数据保存在 PVC/NAS 磁盘中）。<br>设为 `false` 或不填：处于正常激活运行状态。 | `true`, `false` |
| **`idleTimeout`** | `string` | 否 | 自动休眠（缩容至 0 副本）的空闲超时时间配额。采用 Go 持续时间格式表示。若省略该字段，则该空间永不因为空闲而超时关闭。 | `"15m"` (15分钟)<br>`"2h"` (2小时) |
| **`exposeSSH`** | `boolean` | 否 | 是否在容器中额外开启并暴露 SSH 端口（22 端口）。如果设为 `true`，Service 和 Ingress 将自动代理 SSH 流量。默认为 `false`。 | `true`, `false` |

---

### B. 运行环境配置 (Spec.Runtime)

`spec.runtime` 定义工作空间使用的基础容器镜像、端口和系统算力资源限额：

| 子参数名 | 数据类型 | 是否必填 | 作用描述 | 示例值 |
| :--- | :--- | :--- | :--- | :--- |
| **`image`** | `string` | **是** | 启动开发空间所用的基础容器镜像。 | `"smanx/opencode:latest"`<br>`"codercom/code-server:v2"` |
| **`port`** | `int32` | 否 | 容器内开发服务监听的端口号。<br>若缺省：对于 `nginx` 镜像自动使用 `80`，对于其他镜像自动使用 `8080`（API-Server 创建 `opencode` 时会设为 `4096`）。 | `4096`, `8080` |
| **`cpu`** | `string` | 否 | 分配给该容器的 CPU 计算配额。限制和请求配额设为一致（保证 QOS 等级）。<br>若缺省：默认为 `"500m"` (0.5核)。 | `"500m"` (0.5核)<br>`"2"` (2核) |
| **`memory`** | `string` | 否 | 分配给该容器的内存配额。限制和请求设为一致。<br>若缺省：默认为 `"1Gi"` (1G)。 | `"512Mi"`, `"4Gi"` |
| **`env`** | `array` | 否 | 注入该开发容器中的环境变量键值对列表。常用于传递 API Key、初始化密码或系统配置。 | 见下文示例 |

#### Env 变量配置语法示例：
```yaml
env:
  - name: OPENCODE_SERVER_USERNAME
    value: "admin"
  - name: OPENCODE_SERVER_PASSWORD
    value: "password123"
```

---

### C. 存储持久化配置 (Spec.Storage)

`spec.storage` 用于控制用户的持久化卷（PVC）的分配大小及类型：

| 子参数名 | 数据类型 | 是否必填 | 作用描述 | 示例值 |
| :--- | :--- | :--- | :--- | :--- |
| **`size`** | `string` | **是** | 持久化磁盘的容量大小。 | `"1Gi"`, `"20Gi"` |
| **`storageClass`** | `string` | 否 | 绑定的 Kubernetes 存储类名称。如果为空，则使用集群中默认的 StorageClass（例如生产环境部署的 `nfs-client`）。 | `"standard"`, `"nfs-storage"` |

---

## 3. 运行状态反馈详解 (Status)

`status` 字段由 Operator 控制器实时观测和更新，您可以通过查询 status 来得知当前空间处于什么阶段、如何访问。

| 状态参数名 | 数据类型 | 作用描述 | 示例值 |
| :--- | :--- | :--- | :--- |
| **`phase`** | `string` | 当前生命周期的状态阶段（详细定义见下表）。 | `"Running"`, `"Sleeping"` |
| **`podName`** | `string` | 当前集群中实际运行的容器 Pod 的名称。当处于休眠或停止时，此值为空。 | `"ws-alice-deploy-66bfdfb459-vrwsn"` |
| **`pvcName`** | `string` | 绑定到该工作空间的存储卷（PersistentVolumeClaim）名称。 | `"ws-alice-pvc"` |
| **`endpoint`** | `string` | 工作空间对外的 HTTP 访问链接域名入口。 | `"ws-alice.localhost"`, `"ws-alice.domain.com"` |
| **`lastActiveTime`**| `datetime` | 最近一次检测到用户活跃（或手动唤醒）的时间戳。用于计算空闲过期时间。 | `"2026-06-25T10:14:01Z"` |

### 状态阶段 (Status.Phase) 转换定义

* **`Pending`**: 准备中。Operator 正在处理基础资源（例如在等待 NAS/NFS 磁盘卷 PVC 成功绑定）。
* **`Starting`**: 启动中。正在调度 Pod、拉取镜像或初始化存储子路径。
* **`Running`**: 运行中。容器内部的服务已准备就绪，可以通过 `endpoint` 域名正常打开网页访问。
* **`Sleeping`**: 空闲休眠中。触发了空闲检测（持续 `idleTimeout` 时间无任何活跃），容器已被销毁释放算力，数据完好保留在 NAS 存储上，点击“唤醒”或新请求流量接入时会瞬间恢复。
* **`Stopped`**: 已停止。用户手动点击了“关闭工作空间”，副本数被安全缩容到 0，数据完整保留。
* **`Failed`**: 运行失败。可能由于镜像拉取失败、CPU/内存配额不足或持久化挂载出错等原因导致。
