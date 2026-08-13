# Agent 工作空间 Operator - 离线生产环境部署方案

本文档为您提供了将该平台在**完全离线（Air-Gapped / 孤岛网络）**的生产集群上完成部署的详细指南，涵盖镜像打包、自定义 CRD 与 Operator 安装包生成、**本地物理盘动态存储 (Local Path)**、**NAS / NFS 网络共享存储** 以及 **OSS 对象存储卷 (CSI / ossfs)** 三方案部署、Ingress 网关安装及内网 DNS 解析。

---

## 准备工作流阶段

```mermaid
graph TD
    subgraph 有网环境 (开发机/中转机)
        A[1. 离线打包 K8s/IDE/Local-Path/NFS/OSS 镜像] --> B[2. 打包 Operator 一键安装包]
        B --> C[3. 收集 Ingress/Local-Path/NFS/OSS 部署清单]
    end
    
    C -->|U盘 / 移动硬盘中转| D[4. 离线集群导入私有仓库 Harbor]
    
    subgraph 离线生产环境 (K8s 集群)
        D --> E{5. 存储架构选型}
        E -->|方案 A: 极致性能| E1[部署 Local Path 本地动态存储]
        E -->|方案 B: 跨节点漂移| E2[部署 NAS / NFS 共享动态存储]
        E -->|方案 C: 海量扩展/云原生| E3[部署 OSS 存储卷 CSI/Secret/PV/PVC]
        E1 --> F[6. 部署 Ingress 路由网关]
        E2 --> F
        E3 --> F
        F --> G[7. 部署 CRD 与 Operator 控制器]
        G --> H[8. 部署/启动 API Server 页面管理服务]
    end
```

---

## 步骤 1：有网环境资源打包 (开发机)

在具备公网访问权限的开发机上，下载并打包部署所需的全部镜像与清单到统一的 `deploy` 目录下（同时包含 Local Path、NAS/NFS 与 OSS CSI 所需的所有依赖镜像）：

```bash
# 1. 创建统一的离线部署资源目录
mkdir -p ./deploy/local ./deploy/nfs ./deploy/oss

# 2. 拉取外部依赖及基础镜像 (注意：开发机为 Mac 时，必须指定 --platform linux/amd64 以确保拉取 x86 镜像)
docker pull --platform linux/amd64 smanx/opencode:latest
docker pull --platform linux/amd64 registry.k8s.io/ingress-nginx/controller:v1.9.4
docker pull --platform linux/amd64 registry.k8s.io/ingress-nginx/kube-webhook-certgen:v1.4.0

# Local Path 本地盘镜像
docker pull --platform linux/amd64 rancher/local-path-provisioner:v0.0.30
docker pull --platform linux/amd64 rancher/library-busybox:1.31.1

# NAS / NFS 动态存储镜像
docker pull --platform linux/amd64 registry.k8s.io/sig-storage/nfs-subdir-external-provisioner:v4.0.2

# OSS CSI 存储驱动镜像 (阿里云 / 自建 CSI 插件，v1.36.2 新架构 = 节点插件 + 控制器 + FUSE Pod)
# 注意：OSS 挂载自 v1.30.4+ 起由独立的 FUSE Pod (csi-ossfs 镜像) 完成，不再依赖宿主机 OpenSSL 等库，
#       必须同时打包以下 4 个镜像
docker pull --platform linux/amd64 registry.cn-hangzhou.aliyuncs.com/acs/csi-plugin:v1.36.2
docker pull --platform linux/amd64 registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.8.0
docker pull --platform linux/amd64 registry.k8s.io/sig-storage/csi-attacher:v4.4.3
docker pull --platform linux/amd64 registry-cn-hangzhou.ack.aliyuncs.com/acs/csi-ossfs:v1.91.11.ack.1-f3157f4

# 3. 本地构建 Operator 控制器与 API Server 镜像 (针对生产 x86 架构交叉编译)
cd /Users/yfsun/mywork/agent-deploy
# 构建 Operator
docker build --platform linux/amd64 -t workspace-operator:v1.0.0 .
# 构建 API Server
docker build --platform linux/amd64 -f Dockerfile.api-server -t api-server:v1.0.0 .
cd -

# 4. 导出所有镜像为 tar 包到 deploy 目录下
docker save smanx/opencode:latest -o ./deploy/opencode.tar
docker save registry.k8s.io/ingress-nginx/controller:v1.9.4 -o ./deploy/ingress-controller.tar
docker save registry.k8s.io/ingress-nginx/kube-webhook-certgen:v1.4.0 -o ./deploy/ingress-certgen.tar
docker save rancher/local-path-provisioner:v0.0.30 rancher/library-busybox:1.31.1 -o ./deploy/local-path-provisioner.tar
docker save registry.k8s.io/sig-storage/nfs-subdir-external-provisioner:v4.0.2 -o ./deploy/nfs-provisioner.tar
docker save registry.cn-hangzhou.aliyuncs.com/acs/csi-plugin:v1.36.2 registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.8.0 registry.k8s.io/sig-storage/csi-attacher:v4.4.3 registry-cn-hangzhou.ack.aliyuncs.com/acs/csi-ossfs:v1.91.11.ack.1-f3157f4 -o ./deploy/oss-csi-plugin.tar
docker save workspace-operator:v1.0.0 -o ./deploy/workspace-operator.tar
docker save api-server:v1.0.0 -o ./deploy/api-server.tar
```

### 1.2 生成与收集全部 YAML 部署清单

将平台部署所需的全部清单统一收集到 `deploy` 目录下：

```bash
cd /Users/yfsun/mywork/agent-deploy

# 1. 生成 Operator 的一键安装包 (dist/install.yaml)
GOWORK=off make build-installer IMG=workspace-operator:v1.0.0

# 2. 将生成的一键安装包、api-server-deploy.yaml 以及 ingress-deploy.yaml 拷贝到统一离线目录
cp dist/install.yaml ./deploy/
```

此时，开发机上的 **`deploy`** 目录已成为了一个**完全自建、开箱即用的离线交付包**，里面包含了所有的 `.tar` 镜像文件、`./local/` 本地盘清单、`./nfs/` NAS 清单以及 `./oss/` OSS 存储清单。

---

## 步骤 2：离线导入私有镜像仓库

将整个 `deploy` 目录拷贝到离线环境的镜像节点上，导入镜像并推送（Push）到内部的私有镜像仓库（如 Harbor）：

```bash
cd ./deploy

# 1. 导入镜像到本地 docker / ctr 引擎
docker load -i opencode.tar
docker load -i ingress-controller.tar
docker load -i ingress-certgen.tar
docker load -i local-path-provisioner.tar
docker load -i nfs-provisioner.tar
docker load -i oss-csi-plugin.tar
docker load -i workspace-operator.tar
docker load -i api-server.tar

# (若是 containerd/ctr 环境: ctr -n=k8s.io image import <filename>.tar)
```

---

## 步骤 3：部署存储层 (三方案可选)

您可以根据生产业务需求，选择以下**任一存储方案**进行部署：

| 方案对比 | **方案 A：Local Path 本地物理盘** | **方案 B：NAS / NFS 网络共享存储** | **方案 C：OSS 对象存储卷 (CSI / ossfs)** |
| :--- | :--- | :--- | :--- |
| **读写性能** | 🏆 **原生最高 (全 SSD 0 网络延迟)** | ⭐️⭐️⭐️ 一般 (受网络带宽/小文件RPC限制) | ⭐️⭐️ 入门/大文件优化 (受 HTTP/REST & 缓存限制) |
| **Pod 跨物理机漂移** | ❌ 绑定原物理节点 | 🏆 **支持 (Pod 可在任意节点自动恢复)** | 🏆 **支持 (Pod 可在任意节点自动恢复)** |
| **容量扩展性** | 受物理磁盘容量限制 | 受 NAS 磁盘阵列限制 | 🏆 **无限按需扩展 (云原生/海量对象存储)** |
| **适用场景** | 高频代码编译、npm/pip 安装、高频读写 | 物理节点易宕机、强要求数据跨机自动漂移 | 海量数据/模型仓库、冷热数据分离、对象存储集成 |

---

### 🌟 方案 A：本地物理盘 Local Path Provisioner（极致性能选型）

#### 1. 在【集群每台物理节点】上初始化目录

在集群的所有物理节点（含 Master / Worker）上运行：

```bash
# ① 个人独占存储根目录
mkdir -p /data/local-path-storage

# ② 公共工具包共享根目录及子目录
mkdir -p /data/bocomwork-share/skill
mkdir -p /data/bocomwork-share/bocomwork
mkdir -p /data/bocomwork-share/opencodedir
```

#### 2. 在【Master 节点】部署 Local Path Provisioner

进入 `deploy/local` 目录，执行部署：

```bash
# 部署本地动态分配器 (包含 Master 节点污点容忍与辅助 Pod RBAC 授权)
kubectl apply -f ./deploy/local/local-path-storage.yaml

# 部署公共工具包共享 PVC
kubectl apply -f ./deploy/local/public-toolkits-pvc.yaml
```

> **注意**：如果集群所有节点均为 Master 节点，部署的 `local-path-storage.yaml` 中已自动包含了针对 `node-role.kubernetes.io/master` 和 `control-plane` 的 NoSchedule 容忍（Tolerations），以及创建辅助目录 Pod 的 `create`/`delete` 权限。

#### 3. 设置 `local-path` 为集群默认存储类

```bash
# 取消其他 StorageClass 的默认标记 (若有)
kubectl annotate storageclass standard storageclass.kubernetes.io/is-default-class- --overwrite

# 将 local-path 设置为集群默认存储类
kubectl annotate storageclass local-path storageclass.kubernetes.io/is-default-class="true" --overwrite
```

---

### 🌐 方案 B：NAS / NFS 动态网络存储（数据高可用选型）

如果在生产环境中搭建了外部 NAS 服务，或希望 Pod 在物理节点宕机时能自动漂移到其他节点拉起并无缝恢复数据：

#### 1. 部署 NFS 动态卷授权 RBAC

```bash
kubectl apply -f ./deploy/nfs/rbac.yaml
```

#### 2. 部署 NFS 动态卷供给器 (NFS Provisioner)

编辑 `./deploy/nfs/deployment.yaml` 文件：

* 将 `image:` 修改为您内网私有 Harbor 仓库的地址：`yourharbor.domain.com/sig-storage/nfs-subdir-external-provisioner:v4.0.2`
* 配置 `env` 环境变量中的 `NFS_SERVER`（NAS 的 IP 地址）和 `NFS_PATH`（NAS 上共享的根路径，如 `/nfs/workspaces`）。
* 执行部署：

  ```bash
  kubectl apply -f ./deploy/nfs/deployment.yaml
  ```

#### 3. 部署 NFS StorageClass 并设置为默认存储类

`deploy/nfs/class.yaml` 定义了名为 `standard` 的 StorageClass：

```bash
# 部署 NFS 存储类
kubectl apply -f ./deploy/nfs/class.yaml

# 将 standard 设置为集群默认存储类
kubectl annotate storageclass local-path storageclass.kubernetes.io/is-default-class- --overwrite
kubectl annotate storageclass standard storageclass.kubernetes.io/is-default-class="true" --overwrite
```

#### 4. 创建 NAS 共享 PVC (针对公共工具包)

如果在 NAS 架构下需要挂载公共共享资源：

```bash
# 部署基于 NFS 的共享 PV 和 PVC 模板
kubectl apply -f ./deploy/nfs-public/shared_pv_template.yaml
kubectl apply -f ./deploy/nfs-public/shared_pvc_template.yaml
```

---

### ☁️ 方案 C：OSS 对象存储卷（海量扩展 & 共享选型）

在拥有内部 OSS/对象存储服务（或阿里云 ACK 环境）中，将 OSS 挂载为 Pod 内的虚拟文件系统，适用于存储海量数据集、AI 模型资产或作为共享持久化存储。

#### 1. 离线部署 OSS CSI 驱动（若为 ACK 集群通常已预装）

若为自建/离线集群，需将导出的 `oss-csi-plugin.tar`（含 4 个镜像）推送到私有 Harbor 仓库后部署 CSI 驱动。
**注意：`csi-ossfs`（FUSE Pod）镜像必须保持仓库路径 `acs/csi-ossfs` 与 Tag `v1.91.11.ack.1-f3157f4` 不变地推送到 Harbor**，
控制器通过 `DEFAULT_REGISTRY` 环境变量将 FUSE Pod 镜像解析为 `{DEFAULT_REGISTRY}/acs/csi-ossfs:v1.91.11.ack.1-f3157f4`。

部署前先按私有仓库地址修改两处：

* `deploy/oss/csi-plugin.yaml`：DaemonSet 中 `csi-plugin` 与 `driver-registrar` 的 `image:` 改为内网 Harbor 地址。
* `deploy/oss/csi-controller.yaml`：`csi-controller` 与 `external-oss-attacher` 的 `image:` 改为内网地址，并将 `DEFAULT_REGISTRY` 环境变量改为 Harbor 地址（如 `yourharbor.domain.com`）。若节点上导入的 `csi-ossfs` tag 与默认值 `v1.91.11.ack.1-f3157f4` 不同，同步修改 `OSS_FUSE_OSSFS` 环境变量的 `image-tag=` 值（与 `ctr -n k8s.io images list | grep csi-ossfs` 的输出保持一致）。

```bash
# 部署节点插件 DaemonSet（含 RBAC / Namespace ack-csi-fuse / CSIDriver attachRequired=true）
kubectl apply -f ./deploy/oss/csi-plugin.yaml

# 部署控制器（v1.30.4+ 必须：FUSE Pod 由 ControllerPublishVolume 创建，缺了它挂载必然失败）
kubectl apply -f ./deploy/oss/csi-controller.yaml
```

> **架构说明**：旧版本（v1.26.9）通过 `systemd-run` 在宿主机上直接执行 ossfs，要求宿主机安装 OpenSSL 1.0（`libssl.so.10`）等依赖库，自建集群常见 `libssl.so.10: cannot open shared object file` 挂载失败。
> v1.36.2 新架构改为在目标节点创建 **FUSE Pod**（`ack-csi-fuse` 命名空间，每个卷一个）在容器内完成挂载，宿主机只需有 `systemd`，无需安装任何 ossfs 依赖库。
>
> **内网 http 环境注意**：driver 默认会给不带协议的 URL 强制拼 `https://` 前缀。如果内部 OSS/对象存储只支持 **http**（如专有云 OSS），必须：
> 1. 在 `csi-controller.yaml` 与 `csi-plugin.yaml` 中保持环境变量 `PRIVATE_CLOUD_TAG: "true"`（专有云开关，禁止 driver 改写 URL 与协议）；
> 2. StorageClass 与 PV 的 `url` 参数显式写成 `http://oss-cn-xxx.internal...`。
> 否则挂载报错 `ossfs: Failed to check bucket and directory for mount point: Unable to connect(host=https://...)`。

部署完成后确认组件就绪：

```bash
kubectl get ds -n default csi-plugin-oss          # 每个节点 1 个 Pod
kubectl get deploy -n default csi-controller-oss  # 控制器 1 个副本 Ready
kubectl get csidriver ossplugin.csi.alibabacloud.com -o yaml | grep attachRequired  # 必须为 true
```

#### 2. 创建 OSS 凭证 Secret (`deploy/oss/secret.yaml`)

保存连接 OSS 所需的 AccessKey 凭证（部署在 `default` 命名空间，与 NFS 保持一致）：

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: oss-secret
  namespace: default              # 与 NFS 统一保持 default 命名空间
type: Opaque
stringData:
  akId: "YOUR_ACCESS_KEY_ID"
  akSecret: "YOUR_ACCESS_KEY_SECRET"
```

执行部署：

```bash
kubectl apply -f ./deploy/oss/secret.yaml
```

#### 3. 部署全集群通用的 OSS 动态 StorageClass (`deploy/oss/storageclass.yaml`)

通过配置 `csi.storage.k8s.io/*-secret-namespace` 参数，统一引用 `default` 命名空间下的 Secret 凭证，使任何 Namespace（如 `default`, `ns-a`, `ns-b`）下的 Workspace 均可使用：

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: alicloud-oss              # 全集群通用的 StorageClass 名称
provisioner: ossplugin.csi.alibabacloud.com
parameters:
  bucket: "your-user-workspaces-bucket"
  url: "http://oss-cn-hangzhou-internal.aliyuncs.com"   # 内网/专有云 OSS 显式写 http://（不写会被强制 https://）
  otherOpts: "-o max_stat_cache_size=100000 -o allow_other"
  fuseType: "ossfs"               # 仅支持 ossfs / ossfs2，不能填 direct（direct 是沙箱专用）
  csi.storage.k8s.io/provisioner-secret-name: "oss-secret"
  csi.storage.k8s.io/provisioner-secret-namespace: "default"
  csi.storage.k8s.io/node-publish-secret-name: "oss-secret"
  csi.storage.k8s.io/node-publish-secret-namespace: "default"
reclaimPolicy: Retain
allowVolumeExpansion: true
```

执行部署：

```bash
kubectl apply -f ./deploy/oss/storageclass.yaml
```

#### 4. 创建基于 OSS 的公共共享 PV 与 PVC (参考 NFS 共享设计)

在 `deploy/oss-public/` 目录下提供了用于共享资产的 PV 与 PVC 模板：

* **`deploy/oss-public/shared_pv_template.yaml`**：

  ```yaml
  apiVersion: v1
  kind: PersistentVolume
  metadata:
    name: global-oss-share-pv
  spec:
    capacity:
      storage: 100Gi
    accessModes:
      - ReadWriteMany             # 支持多 Pod 节点同时读写
    persistentVolumeReclaimPolicy: Retain
    csi:
      driver: ossplugin.csi.alibabacloud.com
      volumeHandle: global-oss-share-pv-id
      nodePublishSecretRef:
        name: oss-secret
        namespace: default
      controllerPublishSecretRef:   # v1.30.4+ 新架构必需：FUSE Pod 在 ControllerPublish 阶段创建，需要凭证
        name: oss-secret
        namespace: default
      volumeAttributes:
        bucket: "your-shared-bucket-name"
        url: "http://oss-cn-hangzhou-internal.aliyuncs.com"   # 内网 OSS 显式写 http://
        path: "/shared-assets"
        otherOpts: "-o max_stat_cache_size=100000 -o allow_other"
        fuseType: "ossfs"           # 仅支持 ossfs / ossfs2，不能填 direct
  ```

* **`deploy/oss-public/shared_pvc_template.yaml`**：

  ```yaml
  apiVersion: v1
  kind: PersistentVolumeClaim
  metadata:
    name: global-oss-share-pvc     # 共享 PVC 名称
    namespace: default
  spec:
    accessModes:
      - ReadWriteMany
    resources:
      requests:
        storage: 100Gi
    volumeName: global-oss-share-pv # 精确绑定对应的 OSS PV
    storageClassName: ""           # 静态绑定必须留空 ""
  ```

执行部署：

```bash
kubectl apply -f ./deploy/oss-public/shared_pv_template.yaml
kubectl apply -f ./deploy/oss-public/shared_pvc_template.yaml
```

#### 5. 在平台 Workspace CRD 中配置存储介质与共享挂载

创建 Workspace 时，可以通过 `spec.storage.storageClass` 指定个人独占 PVC 使用的存储介质（如 `alicloud-oss`、`local-path` 或 `standard`），并通过 `spec.sharedVolumeMounts` 挂载公共共享卷：

```yaml
apiVersion: ai.example.com/v1alpha1
kind: Workspace
metadata:
  name: workspace-oss-sample
  namespace: default
spec:
  owner: "user-001"
  runtime:
    image: "smanx/opencode:latest"
  storage:
    size: "10Gi"
    storageClass: "alicloud-oss"   # 核心参数：指定个人独占 PVC 使用的存储介质名称 (如 alicloud-oss / local-path / standard)
  sharedVolumeMounts:
    - pvcName: "global-oss-share-pvc" # 引用前面创建的公共共享 OSS PVC
      mountPath: "/data/oss-shared"   # 容器内共享访问路径
      readOnly: false
```

---

## 步骤 4：部署 Ingress 路由网关 (Kubeadm 集群适配)

1. **修改配置文件**：
   打开 `./ingress-deploy.yaml`，将所有 `image:` 字段替换为您私有镜像仓库对应的内网地址。
2. **执行基础部署**：

   ```bash
   kubectl apply -f ./ingress-deploy.yaml
   ```

3. **开启物理机 HostNetwork 模式（推荐，性能最好）**：

   ```bash
   kubectl patch deployment ingress-nginx-controller -n ingress-nginx -p '{"spec":{"template":{"spec":{"hostNetwork":true,"dnsPolicy":"ClusterFirstWithHostNet"}}}}'
   ```

4. **开启 Ingress Snippet 注解权限**：

   ```bash
   kubectl patch configmap ingress-nginx-controller -n ingress-nginx -p '{"data":{"allow-snippet-annotations":"true"}}'
   ```

---

## 步骤 5：企业内网 DNS 泛域名解析

为方便用户访问各自独立的工作空间，在内网 DNS 服务器（或局域网 CoreDNS）中配置泛域名解析（Wildcard A Record）：

```text
*.yourdomain.local    IN    A    <Ingress 物理节点 IP>
```

---

## 步骤 6：部署 Operator 与 API Server

1. **部署自定义 CRD 及 Operator 控制器**：

   ```bash
   kubectl apply -f ./install.yaml
   ```

2. **部署并启动 API Server 管理后台**：

   ```bash
   kubectl apply -f ./api-server-deploy.yaml
   ```

---

## 步骤 7：验证运行与持久化

1. 打开浏览器访问 API Server Launcher 管理页面 (`http://<节点IP>:30000`)。
2. 创建或启动一个工作空间，观察 Pod 启动日志。
3. 执行命令验证存储状态与动态 PV 生成：

   ```bash
   # 查看存储卷声明状态 (Local-path, NFS 或 OSS PVC 应均为 Bound 状态)
   kubectl get pvc -n bocomwork

   # 查看自动生成或绑定的 PV 及其存储类名称
   kubectl get pv

   # OSS 方案专属验证：
   # 1. VolumeAttachment 状态必须为 true（FUSE Pod 创建成功的标志）
   kubectl get volumeattachments

   # 2. 每个已挂载的 OSS 卷在 ack-csi-fuse 命名空间下对应一个 FUSE Pod
   kubectl -n ack-csi-fuse get pods -o wide

   # 3. 挂载失败时查看 FUSE Pod 日志（常见原因：凭证缺失、OSS URL 不可达）
   kubectl -n ack-csi-fuse logs <fuse-pod-name>
   ```

> **存量 PV 迁移提示**：若集群中存在按旧版本创建的 OSS PV（含 `fuseType: direct` 等旧属性），因 PV 的 `volumeAttributes` 不可变，需删除旧 PV/PVC 后由 Operator 重新创建（`Retain` 回收策略下 OSS 数据不受影响）。

4. 工作空间进入 `Running` 后，尝试写入数据文件，确认停止并重新启动后数据完美恢复。
