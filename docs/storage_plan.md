# 存储方案定版：OSS 对象存储卷 + ossfs 1.0

> **状态**：已定版（生产基线） · **定版日期**：2026-11-04
> **适用范围**：`agent-deploy` 的全部生产部署形态（离线自建 Kubernetes 集群）
> **权威性**：本文是存储选型的**唯一权威结论**。其他部署手册、架构说明与本文冲突时，以本文为准，并应提 PR 修正冲突文档。

---

## 1. 方案结论（一句话）

生产环境**统一使用阿里云 OSS 对象存储卷 + ossfs 1.0 客户端**（CSI `fuseType: ossfs`）作为工作空间持久化存储底座；**NAS/NFS 与 Local Path 不再是生产方案**，仅作为附录备选保留；**阿里云 ACK 部署包（`deploy-ack/`）暂不启用**。

| 决策项 | 结论 |
| :--- | :--- |
| 存储介质 | OSS 对象存储（Bucket + 前缀），不落本地盘、不挂 NAS |
| 挂载客户端 | **ossfs 1.0**（`fuseType: ossfs`），全集群统一，不启用 ossfs 2.0 |
| 个人工作空间卷 | Operator 按 StorageClass `alicloud-oss` 自动创建**静态 PV**（子路径 `/workspaces/<workspace-name>`）+ RWX PVC，`reclaimPolicy: Retain` |
| 公共共享卷 | 静态 PV `global-oss-share-pv` + PVC `global-oss-share-pvc`（`path: /shared-assets`），供 `spec.sharedVolumeMounts` 引用 |
| 删除 Workspace | **不删除** PV / PVC / OSS 数据；Operator 不注入清理 finalizer |
| ACK 部署包 | `deploy-ack/` **暂不启用**（目录保留作附录参考，见 §3.1） |
| NAS / NFS、Local Path | **附录备选**，仅用于存量迁移过渡、离线演练或应急回退（见 §3.2） |

---

## 2. 为什么是 OSS + ossfs 1.0

1. **随机写安全**：Workspace 内运行的 Agent（opencode 等）存在大量 edit、编译、`npm/pip` 安装等随机写操作。ossfs 2.0 官方明确随机写场景不建议使用，ossfs 1.0 对随机写成熟稳定，因此**统一锁定 1.0**。
2. **容量与运维成本**：OSS 容量按需扩展、无需维护 NFS 服务端或单机目录；Pod 可在任意节点漂移，不受 Local Path 节点绑定的限制。
3. **与 Operator 能力匹配**：Operator 已内建 OSS 静态 PV 自动创建（`internal/controller/workspace_controller.go` 的 `reconcilePVC`）、子路径隔离、RWX 共享与 `Retain` 保留语义，无需额外组件。
4. **共享资产复用**：公共工具包等共享资源通过独立 Bucket 前缀 + RWX 共享 PVC 下发，与个人数据物理隔离。

### 2.1 ossfs 1.0 的固定参数（不得混用 2.0 参数）

| 位置 | 参数 | 说明 |
| :--- | :--- | :--- |
| `StorageClass alicloud-oss` | `fuseType: "ossfs"` | 指定 1.0 客户端；`ossfs2`、`direct` 均不使用 |
| SC / PV | `otherOpts: "-o max_stat_cache_size=100000 -o allow_other"` | 1.0 参数；`allow_other` 保证容器内非 root 用户可读写 |
| SC / PV | `url: "http://<oss-endpoint>"` | 专有云/内网 OSS 显式写协议与内网 Endpoint（不写协议会被驱动强制补 `https://`） |
| SC（按需） | `sigVersion: "v4"` + `region` | 仅当 Bucket 为公共云新建 Bucket（2025-09-01 起强制 V4）时添加；存量专有云 Bucket 保持 V1，不要加 |

> ⚠️ **禁止混用**：1.0 与 2.0 的挂载参数不兼容。在 `fuseType: ossfs` 的 SC/PV 中出现 2.0 专属参数（或反之）会导致挂载失败。

---

## 3. 明确不用的两个方向

### 3.1 阿里云 ACK 部署包（`deploy-ack/`）：暂不启用

- **现状**：目录与 YAML 保留在仓库中，**当前生产部署不引用、不执行**，CI 也不涉及。
- **原因**：当前交付环境为自建/离线 Kubernetes 集群，不走 ACK 托管组件（托管 CSI、Nginx Ingress、SLB、ACR）。
- **何时重新启用**：确定上 ACK 时另行评审；启用前必须先校正该目录中的过时描述——尤其"删除 Workspace 会清理 OSS 前缀"与当前代码不一致（Operator 删除 Workspace 时**不清理** PV/PVC/OSS 数据，仅移除旧版遗留 finalizer）。
- **当前定位**：附录参考（部署手册附录 C）。

### 3.2 NAS / NFS 与 Local Path：附录备选

| 方案 | 保留位置 | 允许的使用场景 | 明确禁止 |
| :--- | :--- | :--- | :--- |
| Local Path（`deploy/local/`） | 部署手册附录 A | 离线演练、性能对照试验、临时环境 | 作为生产默认存储 |
| NAS / NFS（`deploy/nfs/`、`deploy/nfs-public/`） | 部署手册附录 B | 存量数据迁移过渡期、OSS 不可用时的应急回退 | 新建生产 Workspace 默认使用 |
| Local Path → NAS 迁移 | 迁移手册附录 A | 历史集群的自建存储演进（方案已废弃） | 作为新集群的落地方案 |

> 附录中的命令与清单保持可用，但**不参与主线部署流程**，也不在验收清单中。

---

## 4. 组件与清单对照

| 层次 | 组件 / 清单 | 职责 |
| :--- | :--- | :--- |
| CSI 控制面 | `deploy/oss/csi-controller.yaml`（`csi-controller-oss`，1 副本） | Attach/Detach、在 `ack-csi-fuse` 命名空间创建/删除 FUSE Pod |
| CSI 节点面 | `deploy/oss/csi-plugin.yaml`（`csi-plugin-oss` DaemonSet） | 向 kubelet 注册驱动、NodePublish/Unpublish、bind-mount 到 Workspace Pod |
| 数据面 | FUSE Pod（每卷一个，运行 ossfs 1.0） | 真正挂载 OSS、缓存与上传下载 |
| 凭证 | `deploy/oss/secret.yaml`（`default/oss-secret`） | OSS AccessKey，供 SC 与 PV 的 secret 引用 |
| 个人卷 | `deploy/oss/storageclass.yaml`（`alicloud-oss`） | 声明 Bucket/Endpoint/ossfs 1.0 参数；Operator 据此创建静态 PV |
| 共享卷 | `deploy/oss-public/shared_pv_template.yaml`、`shared_pvc_template.yaml` | 公共资产 RWX 共享（`global-oss-share-pv` / `global-oss-share-pvc`） |

> 运行期数据链路、四组件关系与排障见 [oss_storage_architecture.md](oss_storage_architecture.md)。

---

## 5. 文档地图（谁是权威）

| 文档 | 定位 | 与本文的关系 |
| :--- | :--- | :--- |
| [storage_plan.md](storage_plan.md)（本文） | 存储方案定版结论与索引 | 权威来源 |
| [offline_deployment_guide.md](offline_deployment_guide.md) | 离线生产部署手册 | 主线 = OSS + ossfs 1.0；附录 A = Local Path，附录 B = NAS/NFS，附录 C = ACK（暂不启用） |
| [deployment_architecture.md](deployment_architecture.md) | 系统部署架构与组件职责 | 存储层按 OSS CSI + ossfs 1.0 描述 |
| [oss_storage_architecture.md](oss_storage_architecture.md) | OSS 运行期数据链路与排障 | 主线实现说明 |
| [storage_migration_guide.md](storage_migration_guide.md) | 存量数据迁移 | 主线 = 迁移到 OSS；附录 A = 旧的 Local Path → NAS 迁移 |
| [workspace_user_manual.md](workspace_user_manual.md) | Workspace CR 参数手册 | 默认 `storageClass: alicloud-oss`，OSS 语义 |
| [../README.md](../README.md) | 仓库总览与快速入门 | 与本文结论一致 |
| [../deploy-ack/README.md](../deploy-ack/README.md) | ACK 部署包（附录 C） | 顶部标注"暂不启用" |

---

## 6. 各文档中的附录索引

- **附录 A：Local Path 本地物理盘方案** —— 见 [offline_deployment_guide.md](offline_deployment_guide.md) 附录 A
- **附录 B：NAS / NFS 网络共享方案** —— 见 [offline_deployment_guide.md](offline_deployment_guide.md) 附录 B
- **附录 C：阿里云 ACK 部署包（暂不启用）** —— 见 [offline_deployment_guide.md](offline_deployment_guide.md) 附录 C 与 [../deploy-ack/README.md](../deploy-ack/README.md)
- **附录 A（迁移）：Local Path → NAS** —— 见 [storage_migration_guide.md](storage_migration_guide.md) 附录 A

---

## 7. 变更记录

| 日期 | 说明 |
| :--- | :--- |
| 2026-11-04 | 初版：定版 OSS + ossfs 1.0；ACK 部署包暂不启用；NAS/NFS 与 Local Path 降级为附录备选 |
