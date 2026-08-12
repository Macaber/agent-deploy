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
  - `userId` (string, 必填)：要查询的用户 ID，例如 `alice`。
  - `namespace` (string, 选填)：工作空间所在的 K8s 命名空间，默认为 `"default"`。
- **返回响应 (HTTP 200 OK)**：
  - **如果工作空间不存在 (尚未创建过)**:
    ```json
    {
      "exists": false,
      "phase": "",
      "url": ""
    }
    ```
  - **如果工作空间已创建**:
    ```json
    {
      "exists": true,
      "phase": "Running",  // 可能是 Running, Sleeping, Stopped, Pending 等
      "url": "http://ws-alice.localhost"
    }
    ```
- **错误响应**：
  - `400 Bad Request`：缺失 `userId` 参数（`Missing userId parameter`）。
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
  | **`userId`** | `string` | **是** | - | 用户唯一标识（决定 workspace 资源名称 `ws-<userId>`；当 `namespace` 为 `"bocomwork"` 且环境变量中包含非空 `USER_CODE` 时，使用 `USER_CODE` 的值替换 `userId`）。 |
  | **`namespace`** | `string` | 否 | `"default"` | 工作空间所在的 K8s 命名空间。 |
  | **`image`** | `string` | **是** | - | 启动工作空间的容器镜像。 |
  | **`port`** | `int` | 否 | `4096` | 容器监听端口。 |
  | **`cpu`** | `string` | 否 | `"500m"` (0.5核) | CPU 资源配额限制与请求，若为空则缺省为 0.5 核。 |
  | **`memory`** | `string` | 否 | `"1Gi"` | 内存资源配额限制与请求，若为空则缺省为 1G。 |
  | **`storageSize`** | `string` | 否 | `"1Gi"` | 持久化 PVC 大小规格，例如 `"10Gi"`。 |
  | **`storageClass`** | `string`| 否 | - | 指定使用的 StorageClass 名称。 |
  | **`idleTimeout`** | `string` | 否 | `"5m"` (API 创建默认) | 自 `lastActiveTime` 起的最长运行窗口，超时后 Operator 将工作空间缩容为 `Sleeping`。**不是**实时“无操作闲置”检测；唤醒/再次创建会刷新 `lastActiveTime`。Go 持续时间格式，如 `"30m"`。 |
  | **`exposeSSH`** | `boolean` | 否 | `false` | 是否开启并暴露 SSH 默认 22 端口。 |
  | **`env`** | `array` | 否 | - | 环境变量列表，格式为 `[{"name": "KEY", "value": "VAL"}]`。 |
  | **`command`** 或 **`cmd`** | `array` | 否 | - | 自定义容器的启动入口命令 (对应 `ENTRYPOINT`)，如 `["bocomwork-entrypoint"]` 或 `["/bin/bash"]`。 |
  | **`args`** | `array` | 否 | - | 自定义容器启动入口命令参数 (对应 `CMD`)，如 `["-g", "daemon off;"]`。 |
  | **`volumeMounts`** | `array` | 否 | - | 自定义持久卷在容器内的挂载路径及卷内子目录映射。如 `[{"mountPath": "/app", "subPath": "app-dir"}]`。 |
  | **`sharedVolumeMounts`** | `array` | 否 | - | 预先存在的共享存储卷 (PVC) 挂载配置列表。支持 `readOnly` 属性，格式为 `[{"pvcName": "shared-pvc", "mountPath": "/shared", "subPath": "subdir", "readOnly": true}]`。 |
  | **`configMapVolumeMounts`** | `array` | 否 | - | Kubernetes ConfigMap 卷挂载配置列表。支持 `readOnly` 属性，格式为 `[{"configMapName": "bocomwork-config", "mountPath": "/etc/bocomwork", "subPath": "config.yaml", "readOnly": true}]`。 |
  | **`initContainers`** | `array` | 否 | - | **初始化容器配置列表**。在主容器启动前依次运行的初始化容器。支持 `name`, `image`, `command`, `args`, `env`, `volumeMounts`, `sharedVolumeMounts`, `configMapVolumeMounts` 字段。 |
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
    "exposeSSH": true,
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
