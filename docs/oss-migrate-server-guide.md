# oss-migrate-server：OSS → OSS 数据迁移服务指南

`oss-migrate-server` 是一个独立的数据迁移服务，专门用于在阿里云 OSS 之间进行数据迁移与同步（包括跨 Bucket、跨前缀 Prefix、工作空间重命名或克隆同步）。

- **典型迁移场景**：
  从 `oss://euvduatc-oss/workspaces/ws-9743616` 迁移到 `oss://euvduatc-oss/workspaces/ws-974361-guwnb`
- **安全同步语义**：
  - **存在就跳过（默认）**：迁移前先探测目标 OSS 前缀，若目标已有真实对象，则跳过上传，绝不覆盖；
  - **强制覆盖（force）**：开启 force 模式后忽略目标已有数据，强制执行覆盖更新；
  - **演练模式（dry-run）**：仅模拟探测与生成操作预览，不发生实际数据传输；
  - **全量保真与目录过滤**：默认不排除任何内容（完整保留 `server/skills/builtin` 等所有文件）；支持按需勾选仅迁移 `workspace`、`bocomwork`、`bocomcode` 等特定子目录。
- **三种使用方式**：
  1. **Web 控制台（嵌入式 UI）**：通过浏览器可视化填写源与目标路径、批量映射或直接浏览源端工作空间列表；
  2. **HTTP REST API**：便于脚本、CI/CD 自动化流水线调用；
  3. **CLI 命令行直连模式**：支持直接通过命令行执行单次迁移任务。

---

## 1. 架构与与 migrate-server 的区别

| 特性 | migrate-server | oss-migrate-server |
|---|---|---|
| **数据源** | 节点本地磁盘 `local-path-storage` | 阿里云 OSS 对象存储 |
| **目标端** | 阿里云 OSS | 阿里云 OSS |
| **部署形态** | DaemonSet（每个节点挂载宿主机本地卷） | Deployment（单副本或多副本，纯网络/OSS交互） |
| **宿主机依赖** | 强依赖所在节点的本地目录 | 无宿主机卷依赖，集群内任意节点均可运行 |
| **核心机制** | 本地 `ossutil cp` 上传到 OSS | OSS 服务端/并发快速同步（`ossutil cp -r oss://... oss://...`） |

---

## 2. 构建镜像

```bash
# 构建镜像（默认镜像名 oss-migrate-server:latest, linux/amd64）
make docker-build-oss-migrate-server

# 指定镜像名称与标签
make docker-build-oss-migrate-server OSS_MIGRATE_IMG=myharbor.local/library/oss-migrate-server:v1.0.0
docker push myharbor.local/library/oss-migrate-server:v1.0.0

# 或打包为本地 tar 离线镜像包：
make oss-migrate-server VERSION=v1.0.0
# 生成：deploy/oss-migrate-server.tar
```

---

## 3. Kubernetes 部署

部署文件已提供在 `deploy/oss-migrate-server-deploy.yaml`。

```bash
# 确认 default 命名空间中已存在 oss-secret（包含 akId 与 akSecret）
kubectl get secret oss-secret -n default

# 部署服务
kubectl apply -f deploy/oss-migrate-server-deploy.yaml

# 查看 Pod 与 Service
kubectl -n default get deployment,pod -l app=oss-migrate-server
kubectl -n default get svc oss-migrate-server-svc
```

### 关键环境变量

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `akId` / `akSecret` | secretKeyRef | 阿里云 OSS AccessKey 凭据（从 `oss-secret` 读取） |
| `OSS_ENDPOINT` | `oss-cn-hangzhou-internal.aliyuncs.com` | 建议填写内网 Endpoint，避免公网流量费用与带宽瓶颈 |
| `OSS_SOURCE_BUCKET` | `oss://euvduatc-oss` | 默认源 Bucket |
| `OSS_SOURCE_PREFIX` | `workspaces` | 默认源前缀 |
| `OSS_TARGET_BUCKET` | `oss://euvduatc-oss` | 默认目标 Bucket |
| `OSS_TARGET_PREFIX` | `workspaces` | 默认目标前缀 |
| `OSS_MIGRATE_DIRS` | `workspace,bocomwork,bocomcode` | 迁移子目录列表（留空或 `*` 表示整前缀全量迁移） |
| `OSS_MIGRATE_EXCLUDES` | 空 | 排除路径模式（默认留空，完整保留内置 skills 等所有文件） |
| `OSS_MIGRATE_LOG` | `/var/log/oss-migrate-server/oss2oss-migrate.log` | TSV 格式迁移审计日志文件路径 |
| `OSS_MIGRATE_AUTH_USER` | `admin` | 控制台登录账号（兼容 `MIGRATE_AUTH_USER`） |
| `OSS_MIGRATE_AUTH_PASSWORD` | `admin` | 控制台密码（默认 `admin`；留空表示关闭；兼容 `MIGRATE_AUTH_PASSWORD`） |
| `OSS_MIGRATE_URL_PREFIX` | `/bocomwork/oss-migrate` | 反向代理前缀（配合 Nginx，留空默认 `/`） |

---

## 4. Nginx 反向代理配置（代理路径：/bocomwork/oss-migrate/）

如果通过统一网关或反向代理对外暴露服务，请将以下规则置入 Nginx 的 `server { ... }` 块中。

> [!IMPORTANT]
> 1. 请使用 `location ^~ /bocomwork/oss-migrate/`，`^~` 确保该前缀拥有最高匹配优先级，**避免被下方的通用 `location /bocomwork/` 规则拦截**（通用规则会校验 `$ws_user_id`，而迁移控制台无需该 Cookie）；
> 2. 必须配置 `proxy_set_header X-Forwarded-Prefix /bocomwork/oss-migrate;`，以确保后端登录重定向、Cookie 以及退出流程正常工作；
> 3. 配置 `location = /bocomwork/oss-migrate` 自动重定向补齐末尾斜杠，保证浏览器相对路径资源解析一致。

### 4.1 独立/外部网关 Nginx 配置

```nginx
# 1. 定义后端 Upstream
upstream oss_migrate_backend {
    # 方式 A：集群外部网关，通过 K8s 节点 NodePort（30081）访问
    server 12.244.xxx.251:30081 max_fails=3 fail_timeout=10s;
    server 12.244.xxx.252:30081 max_fails=3 fail_timeout=10s;

    # 方式 B：集群内部 Nginx，直接通过 CoreDNS 域名访问
    # server oss-migrate-server-svc.default.svc.cluster.local:3000;
    keepalive 32;
}

# 2. 缺少末尾斜杠时自动 301 重定向，避免浏览器相对路径混乱
location = /bocomwork/oss-migrate {
    return 301 $scheme://$http_host/bocomwork/oss-migrate/;
}

# 3. 核心代理规则（必须在 location /bocomwork/ 之前声明）
location ^~ /bocomwork/oss-migrate/ {
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    # 传递反代前缀给后端服务（登录跳转与鉴权 Cookie 会自动适配该前缀）
    proxy_set_header X-Forwarded-Prefix /bocomwork/oss-migrate;

    # HTTP 1.1 与长连接
    proxy_http_version 1.1;
    proxy_set_header Connection "";

    # 迁移属于长时间操作，放宽超时并关闭缓冲
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;
    proxy_buffering off;
    proxy_cache off;
    chunked_transfer_encoding on;

    # 剥除 /bocomwork/oss-migrate 前缀后转发给后端
    rewrite ^/bocomwork/oss-migrate/(.*)$ /$1 break;
    proxy_pass http://oss_migrate_backend;
}
```

### 4.2 K8s Ingress (ingress-nginx) 资源配置（可选）

若采用集群内置的 `ingress-nginx` 控制器，可直接部署以下 Ingress：

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: oss-migrate-ingress
  namespace: default
  annotations:
    kubernetes.io/ingress.class: "nginx"
    nginx.ingress.kubernetes.io/ssl-redirect: "false"
    nginx.ingress.kubernetes.io/proxy-body-size: "50m"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "600"
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
    nginx.ingress.kubernetes.io/rewrite-target: /$2
    nginx.ingress.kubernetes.io/configuration-snippet: |
      proxy_set_header X-Forwarded-Prefix /bocomwork/oss-migrate;
spec:
  rules:
    - http:
        paths:
          - path: /bocomwork/oss-migrate(/|$)(.*)
            pathType: ImplementationSpecific
            backend:
              service:
                name: oss-migrate-server-svc
                port:
                  number: 3000
```

---

## 5. Web 可视化控制台交互说明

服务默认通过 NodePort `30081` 暴露。浏览器访问 `http://<任意节点IP>:30081/`：

### 4.1 可视化工作台（点选 + 手动输入 / 选中）
页面采用左右分栏联动设计，符合运维直觉：

1. **左侧：源工作空间列表（通过 `oss ls` 自动列出）**
   - 页面加载时自动在源 Bucket / Prefix 下执行 `oss ls`，实时列出所有存在的工作空间（如 `ws-9743616` 等）；
   - 支持实时搜索/关键词过滤；
   - 点击列表中任意一个工作空间，即将其设为当前源端，并高亮显示。

2. **右侧：配置目的路径（支持手动输入，也支持从已有列表中选中）**
   - **方式一（手动输入）**：可在输入框中直接输入任意目标工作空间名（如 `ws-974361-guwnb`）或完整 OSS 路径；
   - **方式二（下拉直接选中）**：提供“从目标端现有目录中选中”下拉框（自动通过 `oss ls` 列出目标端已有的工作空间目录），可一键选取；
   - **快捷按钮推荐**：提供 `ws-974361-guwnb`、`+ 后缀 -guwnb`、`+ 后缀 -bak`、`与源同名` 等快捷生成按钮；
   - **实时目标存在性探测（自动安全提醒）**：
     - 当输入或选中目标路径后，后台自动防抖执行探测：
     - **若目标已存在**（发现对象数 > 0）：页面弹出黄色警示框，提示目标已包含数据，并提示【存在就跳过】当前生效；如需覆盖请勾选【强制覆盖】；
     - **若目标为空或不存在**：页面显示绿色对勾卡片，提示可安全同步。

3. **策略勾选与执行**
   - **存在就跳过（默认勾选）**：目标非空时自动跳过，不覆盖任何远端数据；
   - **强制覆盖（Force）**：与“存在就跳过”互斥，强行覆盖；
   - **演练模式（Dry-Run）**：仅输出操作预览，不发生实际数据拷贝；
   - 点击大按钮 **“🚀 开始迁移”**：即时启动异步任务，下方展示进度条、传输子目录、已完成/跳过项及实时执行明细。

### 4.2 批量文本映射迁移（Batch Mapping）
支持多对路径批量迁移，文本框中每行填写一条映射：
```
oss://euvduatc-oss/workspaces/ws-9743616 -> oss://euvduatc-oss/workspaces/ws-974361-guwnb
ws-9743617 -> ws-9743617-backup
```
支持简写为 `ws-A -> ws-B`，服务将自动根据配置拼接默认源/目标 Bucket 和 Prefix。

### 4.3 审计日志查看
“审计日志”标签页展示 `/var/log/oss-migrate-server/oss2oss-migrate.log` 中的所有操作历史，包括时间戳、状态（OK / SKIP / FAIL）、源/目标路径、同步对象数与耗时。

---

## 5. REST API 说明

### 1. 提交迁移任务
`POST /api/migrate/run`

请求体：
```json
{
  "tasks": [
    {
      "source": "oss://euvduatc-oss/workspaces/ws-9743616",
      "destination": "oss://euvduatc-oss/workspaces/ws-974361-guwnb",
      "force": false,
      "dryRun": false
    }
  ],
  "force": false,
  "dryRun": false
}
```

响应体：
```json
{
  "taskId": "task-1732349123456789"
}
```

### 2. 目标存在性探测
`POST /api/migrate/remote-check`

请求体：
```json
{
  "uris": [
    "oss://euvduatc-oss/workspaces/ws-974361-guwnb"
  ]
}
```

响应体：
```json
{
  "results": [
    {
      "uri": "oss://euvduatc-oss/workspaces/ws-974361-guwnb",
      "exists": true,
      "objectCount": 42
    }
  ]
}
```

### 3. 获取任务状态与明细
`GET /api/tasks/{taskId}`

### 4. 查询审计日志
`GET /api/logs`

---

## 6. CLI 命令行模式

`oss-migrate-server` 二进制支持直接在终端中运行：

```bash
# 存在就跳过（默认）
oss-migrate-server \
  --src "oss://euvduatc-oss/workspaces/ws-9743616" \
  --dst "oss://euvduatc-oss/workspaces/ws-974361-guwnb"

# 强制覆盖
oss-migrate-server \
  --src "oss://euvduatc-oss/workspaces/ws-9743616" \
  --dst "oss://euvduatc-oss/workspaces/ws-974361-guwnb" \
  --force

# 演练预览
oss-migrate-server \
  --src "oss://euvduatc-oss/workspaces/ws-9743616" \
  --dst "oss://euvduatc-oss/workspaces/ws-974361-guwnb" \
  --dry-run
```
