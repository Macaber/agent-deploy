# Agent 工作空间平台 - 阿里云 ACK 部署包（OSS 存储方案）

本目录是面向**阿里云 ACK 集群**的开箱部署包，存储底座采用 **OSS 对象存储卷（默认 ossfs 1.0 客户端，随机写安全；ossfs 2.0 作为读多写少场景的可选项）**。Agent（opencode）的 edit、编译、依赖安装均为随机写操作，ossfs 2.0 官方明示该场景不建议使用，故默认采用 ossfs 1.0。平台整体架构（Workspace Operator + API-Server + Ingress + CRD）与自建集群方案完全一致，本手册仅覆盖 ACK 环境的差异部分；组件原理见 [docs/deployment_architecture.md](../docs/deployment_architecture.md)。

> **核心结论：ACK 已内置 OSS CSI 驱动（托管 csi-plugin）与 Nginx Ingress 组件，OSS 挂载能力开箱即用。无需部署 `deploy/oss/csi-*.yaml` 与 `deploy/ingress-deploy.yaml`，只需三个 YAML（Secret → StorageClass → 共享 PV/PVC）即可完成存储接入。**

---

## 目录结构

```text
deploy-ack/
├── README.md                  # 本部署手册
├── secret.yaml                # ① OSS 凭证 Secret（default 命名空间）
├── storageclass.yaml          # ② alicloud-oss StorageClass（默认 ossfs 1.0 + V4 签名）
├── storageclass-ossfs2.yaml   #    ossfs 2.0 可选 SC（只读/顺序读场景，与②二选一）
├── shared-pv.yaml             # ③ 公共共享 OSS PV（ossfs 1.0）
├── shared-pvc.yaml            # ③ 公共共享 PVC（Workspace 通过 sharedVolumeMounts 引用）
├── api-server-deploy.yaml     # ④ API-Server（ACR 镜像 + ACK 域名/SLB 配置）
└── workspace-sample.yaml      # ⑤ 部署验证示例 Workspace
```

Operator / CRD 一键安装包不在本目录，构建方式见步骤 5：`GOWORK=off make build-installer IMG=<ACR镜像>` 生成 `dist/install.yaml`。

---

## 1. 镜像清单：复用与新增

| 镜像 | 版本 | 来源 | 说明 |
| :--- | :--- | :--- | :--- |
| **workspace-operator** | `v1.1.0`（新构建） | 本仓库 Dockerfile | **必须重新构建**：本次新增 OSS V4 签名透传（`sigVersion`/`region` 进 PV、清理侧 SDK 走 V4），构建命令见步骤 5 |
| **api-server** | `v1.0.0` | 复用 `deploy/api-server.tar` | 无代码改动，`docker load` 后重新打 ACR tag 即可 |
| **opencode（Agent 运行镜像）** | `smanx/opencode:latest` | 复用镜像名，需重新拉取 | `deploy/` 目录无现成 tar，按步骤 4 `docker pull` 后推 ACR |
| csi-plugin / csi-ossfs 等 FUSE 镜像 | 由 ACK 托管 | ACK 内置 | **无需准备**，托管 csi-plugin 自动维护（仅 ossfs 2.0 可选方案要求 csi-plugin ≥ v1.33.1） |
| nginx-ingress-controller / kube-webhook-certgen | 由 ACK 托管 | ACK 内置 | **无需准备**（安装 Nginx Ingress 组件即可） |
| ~~oss-csi-plugin.tar~~ / ~~ingress-controller.tar~~ / ~~ingress-certgen.tar~~ / ~~local-path-provisioner.tar~~ / ~~nfs-provisioner.tar~~ | - | 不再需要 | ACK 托管组件替代，离线包中的这些 tar **不要**导入 ACK |

**无需新增任何第三方镜像**；唯一需要新构建的是 workspace-operator（`v1.1.0`，含 V4 签名适配）。

> 若暂不重建 Operator（继续用 `v1.0.0`）：仅当 Bucket 为**存量 V1 签名 Bucket**（如专有云）时可正常工作；**2025-09-01 之后新建的公共云 Bucket 强制 V4 签名，v1.0.0 会挂载失败**，必须升级。

---

## 2. 前置条件与集群规划

| 配置项 | 要求 | 说明 |
| :--- | :--- | :--- |
| 集群类型 / 版本 | ACK Pro，推荐 K8s 1.28+ | 默认 ossfs 1.0 方案对版本无硬性要求；仅 ossfs 2.0 可选方案要求 K8s ≥ 1.26 且 csi-plugin ≥ v1.33.1 |
| 存储组件 | **CSI 组件（ack-csi-plugin）** | 创建集群时默认安装，无需手工部署任何 CSI 清单 |
| CNI | **Terway + 开启 NetworkPolicy** | Workspace 网络隔离依赖；Flannel 集群策略不生效 |
| Ingress 组件 | **Nginx Ingress（ack-ingress-nginx）**，IngressClass 名为 `nginx` | Operator 创建 Workspace Ingress 时硬编码 `ingressClassName: nginx` |
| 网络 | 集群与 OSS Bucket、ACR **同 Region** | 走内网 Endpoint，免流费低延迟 |
| DNS | 泛域名 `*.yourdomain.com` → Ingress SLB IP | 见步骤 6 |

**OSS Bucket 约定**（均需与集群同 Region、私有读写、标准存储）：

- 个人工作空间 Bucket（如 `your-user-workspaces-bucket`）：Operator 按 `workspaces/<workspace-name>` 前缀自动管理每个 Workspace 的数据；
- 公共共享 Bucket（如 `your-shared-bucket-name`）：`/shared-assets` 路径存放公共工具包。

> **签名版本注意**：阿里云 OSS 自 2025-03-01 起 V1 签名不再对新账号开放，**2025-09-01 起新建 Bucket 强制 V4 签名**。本部署包所有 YAML 已默认 `sigVersion: v4` + `region`；如挂接**存量专有云 Bucket**（仅支持 V1），将 `sigVersion`/`region` 两行删除即可。

---

## 3. 步骤 1：准备 RAM 凭证

1. RAM 控制台创建用户（如 `workspace-oss-sa`，勾选 OpenAPI 调用），生成 AccessKey；
2. 授予最小权限（两个 Bucket 的读写删列）：

```json
{
  "Version": "1",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["oss:GetObject", "oss:PutObject", "oss:DeleteObject", "oss:ListObjects", "oss:GetBucketInfo"],
    "Resource": [
      "acs:oss:*:*:your-user-workspaces-bucket", "acs:oss:*:*:your-user-workspaces-bucket/*",
      "acs:oss:*:*:your-shared-bucket-name", "acs:oss:*:*:your-shared-bucket-name/*"
    ]
  }]
}
```

> **为什么必须用 AK Secret（不能用纯节点 RAM 角色）**：Operator 删除 Workspace 时直接读取 Secret 中的 `akId`/`akSecret` 调用 OSS SDK 清空该 Workspace 的 `workspaces/<name>` 前缀数据（`internal/controller/oss_cleanup.go`）。省略 Secret 会导致挂载可用、但**删除后数据残留**（仅记录日志，不阻塞删除）。

## 4. 步骤 2：确认 ACK 托管 CSI 组件

> `deploy/oss/csi-plugin.yaml`、`deploy/oss/csi-controller.yaml` **不要**在 ACK 上 apply，托管组件已包含等价能力。

```bash
# CSIDriver 已注册（应含 ossplugin.csi.alibabacloud.com）
kubectl get csidriver | grep alibabacloud.com

# 节点插件就绪
kubectl -n kube-system get ds | grep csi
kubectl -n kube-system get ds -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.template.spec.containers[0].image}{"\n"}{end}' | grep csi
```

默认 ossfs 1.0 方案对 csi-plugin 版本无硬性下限；仅当启用 `storageclass-ossfs2.yaml`（可选）时要求 csi-plugin ≥ v1.33.1，不足时在 ACK 控制台「组件管理」升级。

## 5. 步骤 3：部署存储（本目录 YAML）

```bash
cd deploy-ack

# ① OSS 凭证
#    先编辑 secret.yaml 填入 AccessKey
kubectl apply -f secret.yaml

# ② StorageClass（默认 ossfs 1.0，随机写安全；只读/顺序读场景可选 storageclass-ossfs2.yaml，二选一）
#    先编辑 storageclass.yaml：bucket / region / url 三处
kubectl apply -f storageclass.yaml

# ③ 公共共享 PV/PVC
#    先编辑 shared-pv.yaml：bucket / region / url 三处
kubectl apply -f shared-pv.yaml
kubectl apply -f shared-pvc.yaml

# 验证
kubectl get sc alicloud-oss
kubectl get pvc -n default global-oss-share-pvc   # 应为 Bound
```

**ossfs 1.0 vs ossfs 2.0 选型**：

| | ossfs 1.0（本包默认） | ossfs 2.0（可选） |
| :--- | :--- | :--- |
| 随机写（Agent edit / 编译 / npm 安装） | 🏆 成熟稳定，工作空间默认选择 | ⚠️ **不建议**（官方明示随机写场景用 1.0） |
| 顺序读写吞吐 | 一般 | 🏆 显著更优（官方测试内网可达 20 Gbps 级） |
| `allow_other` 非 root 访问 | 需显式 `-o allow_other`（本包已带） | 默认开启（v2.0.1+） |
| csi-plugin 版本要求 | 无特殊要求 | ≥ v1.33.1（K8s ≥ 1.26） |
| 适用 | Agent 个人工作空间（随机写密集） | 模型资产、数据集、只读工具包分发 |

> 本包默认全部采用 ossfs 1.0（个人空间与共享卷一致，组件版本要求最低）。仅当确有只读/顺序读大数据量场景时，再按 `storageclass-ossfs2.yaml` 切换。

## 6. 步骤 4：镜像推送 ACR

```bash
docker login --username=<your-acr-username> registry.cn-hangzhou.aliyuncs.com

# 1. workspace-operator（必须用本次代码重新构建，tag 建议 v1.1.0）
cd /path/to/agent-deploy
docker build --platform linux/amd64 -t registry.cn-hangzhou.aliyuncs.com/<your-ns>/workspace-operator:v1.1.0 .
docker push registry.cn-hangzhou.aliyuncs.com/<your-ns>/workspace-operator:v1.1.0

# 2. api-server（复用离线包 tar，无需重新构建）
docker load -i deploy/api-server.tar
docker tag api-server:v1.0.0 registry.cn-hangzhou.aliyuncs.com/<your-ns>/api-server:v1.0.0
docker push registry.cn-hangzhou.aliyuncs.com/<your-ns>/api-server:v1.0.0

# 3. Agent 运行镜像（Mac 开发机保留 --platform）
docker pull --platform linux/amd64 smanx/opencode:latest
docker tag smanx/opencode:latest registry.cn-hangzhou.aliyuncs.com/<your-ns>/opencode:latest
docker push registry.cn-hangzhou.aliyuncs.com/<your-ns>/opencode:latest
```

> Workspace CR 暂不支持 `imagePullSecrets`：通过 API 传入的自定义镜像需在 ACR 中**设为公开**，或安装免密组件（aliyun-acr-credential-helper）覆盖 Workspace 命名空间。

## 7. 步骤 5：部署 Operator 与 API-Server

```bash
cd /path/to/agent-deploy

# 1. Operator 一键安装包（镜像地址内嵌，无需手改 YAML）
GOWORK=off make build-installer IMG=registry.cn-hangzhou.aliyuncs.com/<your-ns>/workspace-operator:v1.1.0
kubectl apply -f dist/install.yaml
kubectl -n agent-deploy-system get deploy   # 控制器副本 Ready

# 2. API-Server
#    先编辑 deploy-ack/api-server-deploy.yaml：ACR 镜像、WORKSPACE_DOMAIN、WORKSPACE_BASE_URL
kubectl apply -f deploy-ack/api-server-deploy.yaml
```

API-Server 对外暴露二选一：保留 NodePort 30000（在 ACK 控制台**节点安全组**放通 30000）；或将 `api-server-svc` 改为 `LoadBalancer`（自动关联 SLB，生产推荐）。

## 8. 步骤 6：Ingress 与 DNS

```bash
# 确认 IngressClass 名称（必须包含 nginx）
kubectl get ingressclass

# 确认 SLB（EXTERNAL-IP 即泛域名解析目标）
kubectl -n ack-ingress-nginx get svc

# 如需 Snippet 注解（外部 Nginx Cookie 转发依赖），按实际 ConfigMap 名称开启
kubectl -n ack-ingress-nginx get cm
```

DNS 泛域名解析：`*.yourdomain.com  IN  A  <nginx-ingress-lb 的 SLB IP>`。

## 9. 步骤 7：部署验证

```bash
# 编辑 workspace-sample.yaml 中的 ACR 镜像地址后提交
kubectl apply -f deploy-ack/workspace-sample.yaml
```

**验证清单**（逐项确认后部署才算成功）：

```bash
# 1. Workspace Running
kubectl get workspace ws-ack-oss-test -o jsonpath='{.status.phase}'

# 2. Operator 自动创建静态 PV，子路径为 workspaces/ws-ack-oss-test，且带 V4 签名参数
kubectl get pv ws-ack-oss-test-pv -o jsonpath='{.spec.csi.volumeAttributes}' | tr ',' '\n'

# 3. PVC Bound
kubectl get pvc ws-ack-oss-test-pvc -n default

# 4. FUSE Pod（ossfs2 客户端运行于此）已创建且 Running
kubectl -n ack-csi-fuse get pods

# 5. 数据读写持久化：写入 → 停止 → 唤醒 → 文件仍在
kubectl exec -it <ws-pod> -- sh -c 'echo ack-oss-ok > /workspace/ack-test.txt'
kubectl exec -it <ws-pod> -- cat /workspace/ack-test.txt

# 6. 公共共享卷挂载成功
kubectl exec -it <ws-pod> -- ls /data/oss-shared

# 7. 删除清理：OSS 上 workspaces/ws-ack-oss-test/ 前缀被清空、PV 无残留
kubectl delete workspace ws-ack-oss-test
kubectl get pv | grep ws-ack-oss-test
```

## 10. 步骤 8：安全隔离（ACK 适配）

1. **网络策略**：确认 Terway 已开启网络策略能力；`blockedCIDRs` 建议加入阿里云元数据服务：

```yaml
spec:
  networkPolicy:
    blockedCIDRs:
      - "100.100.100.200/32"   # 阿里云 ECS 元数据（ACK 节点 RAM 凭证入口，务必拦截）
      - "169.254.169.254/32"   # 通用元数据地址
```

2. **内核沙箱（可选）**：用 ACK **安全沙箱节点池**替代自建方案的手工 Kata——创建节点池时选择安全沙箱运行时，`kubectl get runtimeclass` 确认名称（通常为 `runv`），Workspace 中 `spec.runtime.runtimeClassName` 填写该名称；
3. `enableServiceLinks: false`、`automountServiceAccountToken: false` 由 Operator 内置，无需额外配置。

## 11. 存量数据迁移（旧集群 → ACK）

| 旧集群存储 | 方式 |
| :--- | :--- |
| 旧集群已用 OSS（专有云） | `ossutil sync` 同步旧 Bucket 的 `workspaces/`、`shared-assets/` 前缀到新 Bucket，保持目录结构不变（Workspace 名称不变即可无缝接管）。注意：若新建的公共云 Bucket 强制 V4 签名，ossutil 使用最新版本即可 |
| 旧集群 Local Path / NFS | 停止对应 Workspace → 节点上定位数据目录（参考 [storage_migration_guide.md](../docs/storage_migration_guide.md)）→ `ossutil cp -rf` 上传到新 Bucket 的 `workspaces/<workspace-name>/` → 在 ACK 按原名称重建 Workspace |

要点：上传前先停止（休眠）Workspace；个人数据必须落在 `workspaces/<workspace-name>/` 前缀（挂载与删除清理都按此约定）；`ossutil` 走内网 Endpoint（`-e oss-cn-hangzhou-internal.aliyuncs.com`）。

## 12. 常见问题排查（FAQ）

**Q1：PVC Pending / FUSE Pod CrashLoopBackOff？**

```bash
kubectl -n ack-csi-fuse describe pod && kubectl -n ack-csi-fuse logs <fuse-pod>
```
常见原因：① Bucket 与集群不同 Region / Endpoint 错误（`Unable to connect(host=...)`）；② AK 无权限（AccessDenied，检查 RAM 策略）；③ **签名错误（SignatureDoesNotMatch / 签名版本不支持）**：新建公共云 Bucket 必须带 `sigVersion: v4` + `region`（本包默认已带），存量专有云 Bucket 则必须删掉这两项；④ otherOpts 参数与 fuseType 客户端版本不匹配（如 ossfs 1.0 专属选项 `max_stat_cache_size` 配了 `fuseType: ossfs2`）——**禁止混用**两个版本的挂载参数。

**Q2：删除 Workspace 后 OSS 数据残留？**

检查 `default/oss-secret` 是否存在且有效——清理逻辑直接读取该凭证；PV 带 `sigVersion=v4` 时 Operator(≥v1.1.0) 会以 V4 签名调用清理接口。清理失败不阻塞删除，按 `workspaces/<name>/` 前缀用 `ossutil rm -r` 手工清理。

**Q3：Ingress 已创建但 404 / 不通？**

`kubectl get ingressclass` 是否含 `nginx`（Operator 硬编码）→ 泛域名是否解析到 SLB IP → SLB 安全组放通 80/443 → Pod 是否 Running（FUSE 挂载失败会卡启动）。

**Q4：镜像 ImagePullBackOff？**

私有 ACR 仓库：给 Deployment 加 `imagePullSecrets` 或装免密组件；Workspace 自定义镜像设为公开；确认同 Region（跨 Region 走公网）。

**Q5：NetworkPolicy 不生效？**

Flannel 集群不支持，需 Terway + 网络策略能力；`kubectl get netpol -n default` 确认策略存在。

**Q6：还能用 NAS / 云盘吗？**

可以，`spec.storage.storageClass` 支持多存储类并存（如 ACK 内置 `alicloud-nas`）。但 OSS 删除清理 finalizer 仅对 `ossplugin.csi.alibabacloud.com` 驱动的 PV 生效；云盘为 RWO 块存储，跨节点漂移受限。

## 13. 卸载

```bash
kubectl get workspace -A                                    # ① 确认所有 Workspace 已删除且 OSS 前缀已清理
kubectl delete -f deploy-ack/api-server-deploy.yaml         # ② API-Server
kubectl delete -f dist/install.yaml                         # ③ Operator + CRD
kubectl delete -f deploy-ack/shared-pvc.yaml \
              -f deploy-ack/shared-pv.yaml \
              -f deploy-ack/storageclass.yaml \
              -f deploy-ack/secret.yaml                     # ④ 存储配置（共享 PV 不删 OSS 远端数据）
# ⑤ Bucket 数据按需保留，或通过 OSS 控制台 / ossutil 清理
```

托管组件（csi-plugin、Nginx Ingress、SLB）由 ACK 管理，无需触碰；确认弃用时在控制台释放。

---

## 参考

- [ossfs 2.0 存储卷概述](https://help.aliyun.com/zh/ack/ack-managed-and-ack-dedicated/user-guide/ossfs-2-0/) / [ossfs 2.0 静态卷](https://help.aliyun.com/zh/ack/ack-managed-and-ack-dedicated/user-guide/mount-oss-volumes-through-ossfs-2-0) / [ossfs 2.0 动态卷](https://help.aliyun.com/zh/ack/ack-managed-and-ack-dedicated/user-guide/using-dynamically-provisioned-volumes-with-ossfs-2-0)
- [ossfs 2.0 挂载选项说明](https://help.aliyun.com/zh/oss/developer-reference/description-of-mount-options)（`allow_other` 默认开启等）
- [OSS V1 签名升级 V4 指引](https://help.aliyun.com/zh/oss/developer-reference/guidelines-for-upgrading-v1-signatures-to-v4-signatures)（2025-09-01 起新建 Bucket 强制 V4）
- [csi-plugin 发布记录](https://www.alibabacloud.com/help/zh/ack/product-overview/csi-plugin)（ossfs 2.0 要求 ≥ v1.33.1）
