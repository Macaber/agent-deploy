# migrate-server：local-path → OSS 迁移服务（DaemonSet）

把一个老集群里仍使用 `local-path` 的 Workspace 数据迁移到 OSS，并在迁移成功后可选地
删除 Workspace CR / PVC / PV（local-path）；本地目录作为兜底保留，不自动删除。

- **部署形态**：DaemonSet，每节点一个 Pod。每个 Pod 挂载**本节点**的
  `/data/local-path-storage`，只处理本节点上的 Workspace（天然隔离、互不干扰）。
- **与 api-server 同构**：`cmd/migrate-server` + 内嵌 `portal.html` + `Dockerfile.migrate-server`
  + `deploy/migrate-server-deploy.yaml` + Makefile target。
- **迁移语义与 `hack/migrate-localpath-to-oss.sh` 一致**：远端已有数据则整 ws 跳过；
  远端为空才上传；无法确认远端状态则判失败、绝不盲传；`force` 才覆盖。
  排除项（默认 `bocomwork/server/skills/builtin`）以“裁树”方式跳过，兄弟项照传。
- **多节点单页面（方式 2）**：页面列出全部节点，可选任一节点上的任意 workspace；
  提交时前端按节点把请求打到该节点 Pod（URL 前缀 `/node/<name>/`，nginx 每节点一个
  upstream + `externalTrafficPolicy: Local`）。每个 Pod 只接受 `NODE_NAME == <name>`
  的请求，否则返回 409，杜绝误操作别节点本地盘。

路径映射：

```
本地: /data/local-path-storage/pvc-<uid>_<ns>_ws-<ID>-pvc/{workspace,bocomwork,bocomcode}
OSS : oss://<bucket>/<prefix>/ws-<ID>/{workspace,bocomwork,bocomcode}
```

---

## 1. 构建镜像

```bash
make docker-build-migrate-server          # 默认 migrate-server:latest, linux/amd64
# 或
make docker-build-migrate-server MIGRATE_IMG=myharbor.local/library/migrate-server:v1.0.0
docker push myharbor.local/library/migrate-server:v1.0.0
```

镜像内含 `ossutil`；启动时由 `hack/migrate-entrypoint.sh` 从 `akId/akSecret` 生成
`~/.ossutilconfig`（复用集群里 OSS StorageClass 使用的 `oss-secret`）。

## 2. 部署 DaemonSet

```bash
# 确认 oss-secret 已存在（详见 deploy/oss/secret.yaml）
kubectl get secret oss-secret -n default

# 改好 deploy/migrate-server-deploy.yaml 里的镜像地址后:
kubectl apply -f deploy/migrate-server-deploy.yaml

kubectl -n default get ds,pod -l app=migrate-server -o wide
kubectl -n default get svc migrate-server-svc
```

关键环境变量（已在 manifest 中给出默认值）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `NODE_NAME` | downward API | 本 Pod 所在节点名（自校验用） |
| `LP_ROOT` | `/data/local-path-storage` | 本节点 local-path 根目录 |
| `OSS_BUCKET` | `oss://euvduatc-oss` | 目标 bucket |
| `OSS_PREFIX` | `workspaces` | bucket 内前缀 |
| `OSS_MIGRATE_DIRS` | `workspace,bocomwork,bocomcode` | 迁移子目录 |
| `OSS_MIGRATE_EXCLUDES` | `bocomwork/server/skills/builtin` | 排除目录（`none` 表示不排除） |
| `OSS_MIGRATE_LOG` | `/var/log/migrate-server/oss-migrate.log` | TSV 日志路径 |
| `OSS_ENDPOINT` | 空 | ossutil 使用的 OSS Endpoint（内网建议填写） |

## 3. 对外暴露：nginx（方式 2，单页面 + 每节点 upstream）

DaemonSet 的 Service 是 `NodePort` + `externalTrafficPolicy: Local`（默认 nodePort `30080`），
即访问“某节点IP:30080”只会落到该节点自己的 Pod。nginx 为每个节点配一个 upstream，
并按 `/node/<name>/` 前缀精确转发。

```nginx
# 每个节点一个 upstream；把 <node-ip> 换成该节点的内网 IP，端口用 NodePort
upstream migrate_node_1 { server 12.244.107.214:30080; }
upstream migrate_node_2 { server 12.244.107.215:30080; }
# ... 有多少节点就配多少

# 单页面 + 按节点前缀转发
location ~ ^/bocomwork/workspace-migrate/(node/(?<node>[^/]+)/(.*))$ {
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_http_version 1.1;

    # 迁移是长任务，进度靠轮询；放宽超时、关闭缓冲
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;
    proxy_buffering off;

    # 只剥离 /bocomwork/workspace-migrate 前缀，保留 /node/<name>/... 传给 Pod 进行节点自校验
    rewrite ^/bocomwork/workspace-migrate/(.*)$ /$1 break;

    # nginx 里不能动态拼 upstream 名，实际写法用 map 或列举 location：
    # 下面 3 行是示意；推荐用 map 二选一（见下方“map 写法”）
    proxy_pass http://migrate_node_1;
}

# 控制台首页与全局接口（任意节点 Pod 都能打开；默认打到节点1）
location /bocomwork/workspace-migrate/ {
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_http_version 1.1;
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;
    proxy_buffering off;
    rewrite ^/bocomwork/workspace-migrate/(.*)$ /$1 break;
    proxy_pass http://migrate_node_1;
}
```

`map` 写法（用变量选择 upstream，避免为每节点手写 location）：

```nginx
map $node $migrate_upstream {
    default       http://migrate_node_1;
    "node-1"      http://migrate_node_1;
    "node-2"      http://migrate_node_2;
    # ...
}

location ~ ^/bocomwork/workspace-migrate/(node/(?<node>[^/]+)/(.*))$ {
    proxy_set_header Host $host;
    proxy_http_version 1.1;
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;
    proxy_buffering off;
    # 只剥离 /bocomwork/workspace-migrate 前缀，保留 /node/<name>/... 传给 Pod 进行节点自校验
    rewrite ^/bocomwork/workspace-migrate/(.*)$ /$1 break;
    proxy_pass $migrate_upstream;   # 解析到对应节点 Pod
}
```

> 说明：页面本身（`/bocomwork/workspace-migrate/`）可走任一节点 Pod；页面内切换/请求
> 各节点时，前端自动把 base 改成 `/bocomwork/workspace-migrate/node/<name>/`，
> 由上面带 `$node` 的 location 转发到该节点。**每个 Pod 会拒绝不属于自己节点的请求(409)**，
> 即使 nginx 配错也不会误操作其他节点的本地盘。

如果你只有一个统一入口、不想做每节点 upstream，也可只保留一段标准 canary 反向代理
（前端直接按节点 IP:NodePort 访问，nginx 只兜底首页）：

```nginx
location /bocomwork/workspace-migrate/ {
    proxy_set_header Host "launcher.workspace.bocomcode";
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_read_timeout 600s;
    proxy_buffering off;
    rewrite ^/bocomwork/workspace-migrate/(.*)$ /$1 break;
    proxy_pass http://migrate_server_svc;   # 集群内 Service 名，或某节点 NodePort
}
```

## 4. 使用（页面）

1. 打开 `http://<域名>/bocomwork/workspace-migrate/`。
2. 左侧选择节点 → 页面显示该节点 local-path 下的所有 workspace（含子目录/大小/文件数）。
3. 勾选要迁移的 workspace（可“全选”）。
4. 选项：
   - **演练(dry-run)**：只打印将执行的 ossutil 命令，不上传、不删除。
   - **强制覆盖(force)**：忽略远端已有数据，覆盖上传（默认关闭）。
   - **迁移后清理**：迁移成功后删除该 ws 的 CR/PVC/PV(local-path)（本地目录保留；默认关闭，二次确认，删除前强制校验迁移证据）。
5. 点“迁移选中”，在“任务进度”里查看逐 ws 状态；下方“迁移日志”显示 TSV 日志。

## 5. API（便于脚本/自动化）

所有 `/api/...` 均可加 `/node/<name>/` 前缀指定节点（必须等于该 Pod 的 `NODE_NAME`）。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/nodes` | 全集群节点列表（含 hostIP、是否本机） |
| GET | `/api/workspaces` | 本节点 local-path 下的 workspace 列表 |
| POST | `/api/migrate/remote-check` | `{ids:[...]}` 探测远端是否已有数据 |
| POST | `/api/migrate/run` | `{ids,dirs,force,dryRun,cleanup}` 启动迁移任务，返回 taskId |
| POST | `/api/migrate/cleanup` | `{ids,dryRun}` 仅清理（删 CR/PVC/PV；**须已迁移成功**，本地目录保留） |
| GET | `/api/tasks` | 本节点最近任务列表 |
| GET | `/api/tasks/<id>` | 单任务详情 |
| GET | `/api/logs` | 本节点迁移日志行 |

## 6. 删除能力的安全护栏

**第一道、也是最关键的一道：必须先证明“已迁移到 OSS”才允许删除。**
`Clean()` 在删除任何 K8s 对象前会强制校验迁移证据，满足以下**任一**才继续，
否则直接 `FAIL` 并且**一个对象都不删、本地目录也不动**：

- 迁移日志（`OSS_MIGRATE_LOG`）中存在该 ws 的 `status=OK` 记录（`LogHasOK`）；或
- 远端 OSS `oss://<bucket>/<prefix>/ws-<ID>/` 下确实存在真实对象（远端探测）。

若两者都无法确认（含 ossutil 访问异常），一律视为“未迁移成功”，拒绝清理。

其余护栏：

- 仅处理 `storageClass=local-path` 的 PVC；若 PVC 的 SC 不是 local-path，直接拒绝（FAIL）。
- 仅当对应 PV 由 `rancher.io/local-path` 提供（CSI driver / `spec.local` / provisioned-by 注解）时才删 PV。
- 删除顺序：**Workspace CR → PVC → PV**，任一环节失败即停止并报错。
- **本地目录永不删除**：本工具不会 `RemoveAll` 节点本地目录，作为最后一份兜底保留。
  （注意：删除 PV 时 local-path provisioner 仍可能回收卷目录，这正是第一步强制校验迁移证据的原因。）
- 支持 `dryRun`：只列出将要删除的内容（含迁移校验结论），不实际删除。
- 迁移后自动清理仅在 `迁移结果=OK` 且非 dry-run 时触发（此时日志已写入 OK，天然满足证据校验）。

## 7. 与旧脚本的关系

`hack/migrate-localpath-to-oss.sh` 保留作为参考与回退手段。`cmd/migrate-server/migrate.go`
是其 Go 移植，`migrate_test.go` 用假 ossutil 覆盖了与脚本等价的关键行为
（远端有数据跳过、force 覆盖、远端状态无法确认判失败、排除裁树且兄弟项照传）。
