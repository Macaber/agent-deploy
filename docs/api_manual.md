# Agent Workspace API-Server 接口手册

API-Server 作为一个轻量级网关/控制面板服务，用于与 Kubernetes API 进行交互，面向前端/前端 Launcher 提供管理用户独立开发工作空间（Workspace 自定义资源）的 RESTful API。

---

## 全局配置与运行参数

- **默认监听端口**：`3000`（可通过环境变量 `PORT` 修改）。
- **域名环境变量**：`WORKSPACE_DOMAIN`（默认为 `localhost`）。生成的 HTTP 接入 URL 会基于此域名拼接，格式为 `http://ws-<userId>.<WORKSPACE_DOMAIN>`。

---

## 接口定义

### 1. 静态主页 Web UI (Launcher)
提供可视化的用户工作空间启动、停止、休眠监控及跳转网页管理面板。
- **请求方法**：`GET`
- **请求路径**：`/`
- **请求参数**：无
- **返回响应 (HTTP 200)**：返回 `text/html` 的交互网页。

---

### 2. 获取用户工作空间状态
用于页面加载或刷新时，动态获取指定用户的工作空间是否已部署、当前生命周期状态以及对应访问入口链接。
- **请求方法**：`GET`
- **请求路径**：`/api/workspaces`
- **请求参数 (Query Params)**：
  - `userId` (string, 选填)：要查询的用户 ID，例如 `alice`。若不提供 `userId`，则查询并返回该命名空间下的所有工作空间列表。
  - `namespace` (string, 选填)：工作空间所在的 K8s 命名空间，默认为 `"default"`。
- **返回响应 (HTTP 200 OK)**：
  - **单个工作空间查询 (`userId` 存在)**:
    - 若不存在:
      ```json
      {
        "exists": false,
        "phase": "",
        "url": ""
      }
      ```
    - 若已创建:
      ```json
      {
        "exists": true,
        "phase": "Running",
        "url": "http://ws-alice.localhost",
        "oa": "alice_oa",
        "spec": { ... }
      }
      ```
  - **工作空间列表查询 (不传 `userId`)**:
    ```json
    [
      {
        "userId": "alice",
        "name": "ws-alice",
        "oa": "alice_oa",
        "phase": "Running",
        "url": "http://ws-alice.localhost",
        "image": "docker.io/library/bocomwork:v1.0.2",
        "cpu": "0.5",
        "memory": "1Gi"
      }
    ]
    ```
- **错误响应**：
  - `500 Internal Server Error`：获取 Kubernetes 集群状态异常。

---

### 3. 创建 / 启动工作空间
为指定用户创建一个全新工作空间（如果不存在）。如果该工作空间已经存在，且用户在请求体中传入了规格参数，API-Server 将动态更新该工作空间的资源规格（例如调整 CPU、内存、环境变量或休眠时间）。
*注：该接口是一个**阻塞式接口**，调用后会启动轮询检测，直到 K8s Pod 就绪且状态（Phase）变为 `Running` 才会返回成功响应。轮询超时时间为 90 秒。*
- **请求方法**：`POST`
- **请求路径**：`/api/workspaces`
- **请求体格式**：`application/json`
- **请求体参数说明**：
  | 参数名 | 数据类型 | 是否必填 | 默认值 | 作用描述 |
  | :--- | :--- | :--- | :--- | :--- |
  | **`userId`** | `string` | **是** | - | 用户唯一标识（决定 workspace 资源名称 `ws-<userId>`）。 |
  | **`namespace`** | `string` | 否 | `"default"` | 工作空间所在的 K8s 命名空间。 |
  | **`image`** | `string` | **是** | - | 启动工作空间的容器镜像。 |
  | **`port`** | `int` | 否 | `4096` | 容器监听端口。 |
  | **`cpu`** | `string` | 否 | `"500m"` (0.5核) | CPU 资源配额限制与请求，若为空则缺省为 0.5 核。 |
  | **`memory`** | `string` | 否 | `"1Gi"` | 内存资源配额限制与请求，若为空则缺省为 1G。 |
  | **`storageSize`** | `string` | 否 | `"1Gi"` | 持久化 PVC 大小规格，例如 `"10Gi"`。 |
  | **`storageClass`** | `string`| 否 | - | 指定使用的 StorageClass 名称。 |
  | **`idleTimeout`** | `string` | 否 | `"5m"` (API 创建默认) | 自 `lastActiveTime` 起的最长运行窗口，超时后 Operator 将工作空间缩容为 `Sleeping`。**不是**实时“无操作闲置”检测；唤醒/再次创建会刷新 `lastActiveTime`。Go 持续时间格式，如 `"30m"`。 |
  | **`env`** | `array` | 否 | - | 环境变量列表，格式为 `[{"name": "KEY", "value": "VAL"}]`。当命名空间为 `bocomwork` 且包含 `OA` 环境变量时，会自动为 Workspace 资源打上 `oa=<value>` 标签（支持 `-l oa=xxx` 检索）。 |
  | **`command`** 或 **`cmd`** | `array` | 否 | - | 自定义容器的启动入口命令 (对应 `ENTRYPOINT`)，如 `["bocomwork-entrypoint"]` 或 `["/bin/bash"]`。 |
  | **`args`** | `array` | 否 | - | 自定义容器启动入口命令参数 (对应 `CMD`)，如 `["-g", "daemon off;"]`。 |
  | **`volumeMounts`** | `array` | 否 | - | 自定义持久卷在容器内的挂载路径及卷内子目录映射。如 `[{"mountPath": "/app", "subPath": "app-dir"}]`。 |
  | **`sharedVolumeMounts`** | `array` | 否 | - | 预先存在的共享存储卷 (PVC) 挂载配置列表。支持 `readOnly` 属性，格式为 `[{"pvcName": "shared-pvc", "mountPath": "/shared", "subPath": "subdir", "readOnly": true}]`。 |
  | **`initContainers`** | `array` | 否 | - | **初始化容器配置列表**。在主容器启动前依次运行的初始化容器。支持 `name`, `image`, `command`, `args`, `env`, `volumeMounts`, `sharedVolumeMounts`, `configMapVolumeMounts` 字段。 |
  | **`runtimeClassName`** | `string` | 否 | - | **容器运行时沙箱名称**（例如 `"kata"`、`"kata-qemu"` 等）。指定后将由 Kata Containers 独立 MicroVM 内核沙箱拉起 Pod，从物理底层彻底防止宿主机内核逃逸。 |
  | **`networkPolicy`** | `object` | 否 | - | **工作空间专属网络安全策略配置**。包含 `disabled` (是否禁用策略，默认 `false`)、`blockedCIDRs` (自定义禁止出站的网段列表，仅拦截用户显式声明的网段，无任何隐式默认拦截)、`allowedCIDRs` (精准白名单放行的 IP/网段列表，如私有 LLM 网关)。 |
  | **`postStartScript`** | `string` | 否 | - | **K8s 原生生命周期钩子**。容器启动后立即在后台异步运行的多行 Shell 脚本。 |
  | **`healthPath`** | `string` | 否 | - | **自定义就绪探针 HTTP 路径**。若指定（例如 `"/health"`），K8s 将使用 HTTP GET 探测此路径；若不指定或为空，默认回退使用 TCP 协议对暴露端口（`port`）进行存活健康探测。 |

- **完整请求体示例**：
  ```json
  {
    "userId": "alice",
    "namespace": "bocomwork",
    "image": "docker.io/library/bocom-opencode-work:v1.0.5",
    "port": 3000,
    "cpu": "1",
    "memory": "1Gi",
    "storageSize": "10Gi",
    "storageClass": "alicloud-oss",
    "idleTimeout": "30m",
    "runtimeClassName": "kata",
    "networkPolicy": {
      "disabled": false,
      "blockedCIDRs": [
        "10.0.0.0/8",
        "172.16.0.0/12",
        "192.168.0.0/16",
        "169.254.169.254/32"
      ],
      "allowedCIDRs": [
        "10.10.20.5/32",
        "192.168.1.100/32"
      ]
    },
    "env": [
      {
        "name": "OPENCODE_RESOURCE_ATTRIBUTES",
        "value": "service.name=opencode,workspace.owner=$(WORKSPACE_OWNER)"
      }
    ],
    "command": ["bocomwork-entrypoint"],
    "volumeMounts": [
      {
        "mountPath": "/workspace",
        "subPath": "workspace"
      },
      {
        "mountPath": "/data",
        "subPath": "data"
      }
    ],
    "sharedVolumeMounts": [
      {
        "pvcName": "bocomwork-local-share",
        "mountPath": "/opt/bocom-defaults/skill/",
        "subPath": "skill",
        "readOnly": true
      }
    ],
    "configMapVolumeMounts": [
      {
        "configMapName": "bocomwork-config",
        "mountPath": "/etc/bocomwork",
        "readOnly": true
      }
    ],
    "initContainers": [
      {
        "name": "sync-shared-tools",
        "image": "rancher/library-busybox:1.31.1",
        "command": ["sh", "-c"],
        "args": [
          "cp -rn /mnt/shared-pvc/skill/* /workspace/opt/bocom-defaults/skill/"
        ],
        "sharedVolumeMounts": [
          {
            "pvcName": "bocomwork-local-share",
            "mountPath": "/mnt/shared-pvc",
            "readOnly": true
          }
        ],
        "volumeMounts": [
          {
            "mountPath": "/workspace",
            "subPath": "workspace"
          }
        ]
      }
    ],
    "postStartScript": "echo 'Container initialized' && touch /workspace/boot-success",
    "healthPath": "/health"
  }
  ```
- **返回响应 (HTTP 200 OK - 成功运行后返回)**：
  ```json
  {
    "url": "http://ws-alice.localhost",
    "phase": "Running"
  }
  ```
- **错误响应**：
  - `400 Bad Request`：请求体 JSON 格式错误或缺失 `userId`。
  - `503 Service Unavailable`：工作空间启动失败（Phase 变更为 `Failed`），快速失败而无需等待超时。返回 JSON 示例：`{"error": "Workspace failed to start", "phase": "Failed"}`。
  - `504 Gateway Timeout`：空间在 90 秒内未能成功进入 `Running` 状态。
  - `500 Internal Server Error`：与 Kubernetes API 交互异常。

---

### 4. 唤醒工作空间
当工作空间已存在且处于已休眠（`Sleeping`）或手动停止（`Stopped`）状态时，用于快速将其唤醒恢复运行。该接口仅支持传入用户 ID，不会对原有的规格配置做任何修改。
*注：该接口是一个**阻塞式接口**，轮询超时时间为 90 秒。*
- **请求方法**：`POST`
- **请求路径**：`/api/workspaces/wakeup`
- **请求体格式**：`application/json`
- **请求体示例**：
  ```json
  {
    "userId": "alice",
    "namespace": "default"
  }
  ```
- **返回响应 (HTTP 200 OK - 唤醒成功且 Running 后返回)**：
  ```json
  {
    "url": "http://ws-alice.localhost",
    "phase": "Running"
  }
  ```
- **错误响应**：
  - `400 Bad Request`：请求体 JSON 格式错误或缺失 `userId`。
  - `404 Not Found`：指定用户的工作空间不存在（需要先通过启动接口进行创建）。
  - `503 Service Unavailable`：工作空间唤醒失败（Phase 变更为 `Failed`），快速失败而无需等待超时。返回 JSON 示例：`{"error": "Workspace failed to start", "phase": "Failed"}`。
  - `504 Gateway Timeout`：空间在 90 秒内未能成功唤醒进入 `Running` 状态。
  - `500 Internal Server Error`：与 Kubernetes API 交互异常。

---

### 5. 停止 / 挂起工作空间
手动关闭指定用户的工作空间。触发后，API-Server 将 Workspace 资源的 `spec.stopped` 设为 `true`，触发 Operator 控制器安全销毁运行的 Pod 容器并释放 CPU/内存等集群算力，持久化的配置文件与聊天日志保存在磁盘卷中不受影响。
- **请求方法**：`POST`
- **请求路径**：`/api/workspaces/stop`
- **请求体格式**：`application/json`
- **请求体示例**：
  ```json
  {
    "userId": "alice",
    "namespace": "default"
  }
  ```
- **返回响应 (HTTP 200 OK)**：
  ```json
  {
    "status": "stopped"
  }
  ```
- **错误响应**：
  - `400 Bad Request`：请求体 JSON 格式错误或缺失 `userId`。
  - `500 Internal Server Error`：修改 Kubernetes 中资源规格失败。

---

## 附录：Agent Sandbox 三维安全隔离机制说明

针对运行具有代码执行和 Shell 命令运行能力的 AI Agent（如 OpenCode、Pi 等），平台在 API-Server 与底层 Operator 中内置了三层隔离防护体系：

```
                Agent Sandbox
                     │
        ┌────────────┼────────────┐
        │            │            │
      Kata          RBAC       NetworkPolicy
        │            │            │
        │            │            │
   Kernel隔离     API隔离       网络隔离
        │            │            │
        └────────────┼────────────┘
                     │
                 Workspace
```

1. **Kernel 隔离 (Kata Containers / MicroVM)**：
   * 在创建请求中指定 `"runtimeClassName": "kata"`，Pod 将运行在轻量级独立虚拟机（MicroVM）内核中，与宿主机内核物理隔离，从底层阻断任何容器逃逸提权。
2. **API & 环境变量隔离 (RBAC & 阻断服务发现)**：
   * **阻断环境变量互现 (`EnableServiceLinks: false`)**：默认关闭同 Namespace 下其他 Service 的环境变量注入，彻底解决 `ws-a` 在容器环境变量中嗅探到 `ws-b` 内网路由地址的问题。
   * **阻断 K8s API 访问 (`AutomountServiceAccountToken: false`)**：默认不挂载 ServiceAccount Token，防止 Agent 容器内调用 K8s API 进行集群侦察。
3. **网络隔离 (NetworkPolicy 自定义管控)**：
   * **入站（Ingress）全通**：所有外部用户、浏览器访问、Ingress 网关、Kubelet 就绪探针完全畅通；
   * **出站（Egress）管控**：放行 DNS（UDP/TCP 53）与 Ingress 网关回包；仅拦截用户在 `blockedCIDRs` 中自定义声明的网段（无任何隐式默认拦截）；支持通过 `allowedCIDRs` 精准放行特定白名单。

