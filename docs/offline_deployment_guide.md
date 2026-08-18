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
mkdir -p ./deploy/local ./deploy/nfs ./deploy/oss ./deploy/kata

# 2. 下载 Kata Containers 静态运行环境包 (针对 x86_64 / amd64 物理节点，388MB 兼容 glibc 2.17+)
# 官方下载: wget -P ./deploy/kata/ https://github.com/kata-containers/kata-containers/releases/download/3.2.0/kata-static-3.2.0-amd64.tar.xz
# 加速下载: curl -Lo ./deploy/kata/kata-static-3.2.0-amd64.tar.xz https://ghfast.top/https://github.com/kata-containers/kata-containers/releases/download/3.2.0/kata-static-3.2.0-amd64.tar.xz

# 3. 拉取外部依赖及基础镜像 (注意：开发机为 Mac 时，必须指定 --platform linux/amd64 以确保拉取 x86 镜像)
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

#### 6. 删除 Workspace 时的数据清理语义（三方案一致）

删除 Workspace 时，Operator finalizer 与 Kubernetes 回收机制配合，实现 **PV / PVC / 存储介质数据** 一并删除：

| 方案 | PVC | PV | 存储数据 |
| :--- | :--- | :--- | :--- |
| Local Path | ownerRef 级联删除 | provisioner Delete 回收自动删 | provisioner 删除节点目录数据 |
| NAS / NFS | ownerRef 级联删除 | provisioner Delete 回收自动删 | provisioner 删除 NAS 子目录数据 |
| OSS | ownerRef 级联删除 | Operator finalizer 显式删除 | Operator 清空 OSS 上 `volumeAttributes.path` 前缀下的全部对象 |

> OSS 数据清理由 Operator 通过 OSS Go SDK 完成（`ForcePathStyle` 适配专有云 http 内网地址），凭证取自 PV 引用的 `oss-secret`，分页列出并删除该 workspace 路径下的全部对象。
> **删除范围安全保证**：删除前缀完全由 workspace 名称推导（`workspaces/<name>`），不采用 PV 上可被篡改的 `path` 属性；且按目录边界过滤（仅删除 key 等于前缀或以前缀 `/` 开头的对象），删除 `ws-aikc` 绝不会误删 `ws-aikc-dev`、公共共享目录等其他路径的内容；workspace 名称含 `/` 等非法字符时直接拒绝执行。
> 若清理失败（凭证错误、网络不可达等），Operator 记录错误日志并继续删除 PV，不阻塞 workspace 删除，残留数据需人工介入。
> 公共共享 PV（`global-oss-share-pv`）不属于任何 Workspace，其数据不受影响。

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

## 步骤 7：Agent Sandbox 三维安全隔离配置 (Kata + RBAC + NetworkPolicy)

在运行自主型 AI Agent（如 OpenCode、Pi 等具有代码执行、系统 Shell 与网络访问能力的 Agent）时，为了彻底防范容器逃逸、内网横向移动与环境变量凭据泄露，系统已内置三维安全沙箱：

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

---

### 7.1 维度一：Kernel 隔离 (Kata Containers 离线部署与 containerd 配置)

适用于 Kubernetes v1.25.5 + containerd + linux/amd64 生产宿主机环境：

#### 1. 检查物理节点虚拟化支持
在所有 Worker 物理节点上检查 CPU 硬件虚拟化支持：
```bash
# 确认 CPU 开启 VT-x / AMD-V 支持 (输出大于 0 即支持)
grep -E -c '(vmx|svm)' /proc/cpuinfo

# 加载宿主机 KVM 内核模块
modprobe kvm
modprobe kvm_intel # 若为 AMD CPU 则为 modprobe kvm_amd
ls -l /dev/kvm     # 确认设备文件存在 (crw-rw---- 1 root kvm ...)
```

#### 2. 离线安装 Kata Containers 静态运行环境 (`kata-static-3.2.0-amd64.tar.xz`)

> 💡 **版本与兼容性说明**：`kata-static-3.2.0-amd64.tar.xz`（体积仅 388MB）兼容各主流企业 Linux 系统（CentOS 7/8、RHEL 7/8、Ubuntu 等，兼容 glibc 2.17+），与 K8s v1.25.5 + containerd 深度适配，且解压使用系统原生 `tar` 即可，无需额外安装其他工具。

##### 【步骤 1：解压部署到根目录】
将 `kata-static-3.2.0-amd64.tar.xz` 拷贝至目标节点，执行解压（文件会自动释放到 `/opt/kata/`）：

```bash
sudo tar -xvf kata-static-3.2.0-amd64.tar.xz -C /
```

##### 【步骤 2：配置 containerd-shim 软链接】
```bash
sudo ln -sf /opt/kata/bin/containerd-shim-kata-v2 /usr/local/bin/containerd-shim-kata-v2
```

##### 【步骤 3：执行 Kata 运行环境健康自检】
```bash
/opt/kata/bin/kata-runtime kata-check
```

**自检成功标志（标准输出）**：
```text
WARN[0000] Not running network checks as super user      arch=amd64 name=kata-runtime pid=632626 source=runtime
System is capable of running Kata Containers
System can currently create Kata Containers
```
> 📌 **输出说明**：
> * `System is capable of running Kata Containers` 和 `System can currently create Kata Containers` 表示该物理节点的 CPU 硬件虚拟化（VT-x / AMD-V）、宿主机 `/dev/kvm` 驱动以及 glibc 依赖已 **100% 就绪**！
> * 开头的 `WARN... Not running network checks as super user` 是因为未以 root 权限检查底层网桥，对实际运行没有任何影响。

##### 【步骤 4：配置 Kata 运行参数 (`/opt/kata/share/defaults/kata-containers/configuration.toml`)】
编辑 `/opt/kata/share/defaults/kata-containers/configuration.toml` 文件，在 `[hypervisor.qemu]` 段配置高性能 `virtio-fs` 与 chroot 安全沙箱参数（避开 namespace 限制并确保启动稳定）：

```toml
[hypervisor.qemu]
shared_fs = "virtio-fs"
virtio_fs_daemon = "/opt/kata/libexec/virtiofsd"

# 关键配置：指定 4 线程与 chroot 沙箱模式（避开 Linux user namespace 限制，确保 virtiofsd 稳定启动）
virtio_fs_extra_args = ["--thread-pool-size=4", "--sandbox=chroot"]

# 设为 0 或注释掉（避开 QEMU 静态版本的 DAX 内存申请冲突）
virtio_fs_cache_size = 0
```

---

#### 3. 配置 containerd (`/etc/containerd/config.toml`)
编辑 containerd 配置文件（通常位于 `/etc/containerd/config.toml`），找到原有的 `runc` 配置段，在其下方**平级对齐**注册 `kata` 运行时：

```toml
        # 1. 宿主机原有的 runc
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
          runtime_type = "io.containerd.runc.v2"
          privileged_without_host_devices = false

        # 2. 紧随 runc 下方注册 kata 运行时（层级平级对齐）
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata]
          runtime_type = "io.containerd.kata.v2"
          privileged_without_host_devices = true
          [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata.options]
            ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration.toml"
```

重启 containerd 引擎使配置生效：
```bash
sudo systemctl daemon-reload
sudo systemctl restart containerd
```

---

#### 4. 在 K8s 集群中创建 `RuntimeClass`
在 Master 节点执行，注册集群级 `kata` RuntimeClass：

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata
handler: kata
```
```bash
kubectl apply -f - <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata
handler: kata
EOF
```

验证 RuntimeClass 注册成功：
```bash
kubectl get runtimeclass
# 输出应包含: kata   kata   ...
```

---

### 7.2 维度二：API & 服务发现隔离 (RBAC & 环境变量阻断)

* **环境变量自动注入阻断 (`EnableServiceLinks: false`)**：
  Kubernetes 默认会在 Pod 启动时将同命名空间下**所有已存在的 Service** 注入为容器的环境变量（例如 `WS_B_SVC_SERVICE_HOST` 等）。Operator 默认在生成的 Pod 中注入 `enableServiceLinks: false`，彻底杜绝 `ws-a` 获取 `ws-b` 的服务内网 IP 与端口。
* **K8s API 凭据阻断 (`AutomountServiceAccountToken: false`)**：
  默认关闭 ServiceAccount Token 自动挂载，Agent 容器内无法获取 `/var/run/secrets/kubernetes.io/serviceaccount/token`，杜绝调用 K8s API 进行集群侦察或提权。

---

### 7.3 维度三：网络隔离 (NetworkPolicy 零信任安全网)

当用户在 Workspace 中声明 `spec.networkPolicy` 时，Operator 会自动创建专属的 `<workspace-name>-netpol` 网络策略：
1. **入站 (Ingress) 全放行**：放行来自 Ingress 网关、Kubelet 探针及外部用户的访问流量，保证外部 Web 访问 100% 畅通。
2. **东西向横向阻断**：严禁 `ws-a` 与 `ws-b` 等多个 Agent Pod 之间相互直接探测与直连通信。
3. **出站安全出口 (Egress - 纯自定义配置，无隐式默认值)**：
   * 允许访问集群 CoreDNS（UDP/TCP 53）进行正常域名解析；
   * 允许与 Ingress 网关进行双向响应回包；
   * **按需声明禁止网段 (`blockedCIDRs`)**：仅拦截在 `spec.networkPolicy.blockedCIDRs` 中显式填写的网段；未填则不拦截公网与任何网段。
   * **白名单放行 (`allowedCIDRs`)**：可在 `spec.networkPolicy.allowedCIDRs` 中添加明确放行的 IP/CIDR（如私有 LLM 网关 `10.10.20.5/32`、内网自建 GitLab `192.168.1.100/32`）。

> **CNI 插件要求**：确保集群 CNI 插件支持 NetworkPolicy（如 Calico、Cilium、Kube-router 或阿里云 ACK Terway）。

---

### 7.4 在 Workspace 中启用 Kata 沙箱与自定义网络策略

在提交 Workspace CR 时，通过 `spec.runtime.runtimeClassName: "kata"` 启用 MicroVM 内核沙箱，并在 `spec.networkPolicy` 中灵活指定拦截/放行网段：

```yaml
apiVersion: ai.example.com/v1alpha1
kind: Workspace
metadata:
  name: ws-agent-secure
  namespace: default
spec:
  owner: "user-001"
  runtime:
    image: "smanx/opencode:latest"
    runtimeClassName: "kata"       # 核心参数：开启 Kata 独立内核沙箱
    cpu: "2"
    memory: "4Gi"
  storage:
    size: "10Gi"
    storageClass: "local-path"
  networkPolicy:
    # 纯自定义禁止出站的私有网段（仅拦截此处显式列出的网段，无任何隐式默认拦截）
    blockedCIDRs:
      - "10.0.0.0/8"
      - "192.168.0.0/16"
    # 精准白名单放行（如公司内网私有大模型网关或内网代码库）
    allowedCIDRs:
      - "10.10.20.5/32"           # 内部 LLM Gateway
      - "192.168.1.100/32"        # 内部私有 GitLab
```

---

## 步骤 8：验证运行、安全隔离与持久化

1. 打开浏览器访问 API Server Launcher 管理页面 (`http://<节点IP>:30000`)。
2. 创建或启动一个工作空间，观察 Pod 启动日志。
3. 执行命令验证存储状态与动态 PV 生成：

   ```bash
   # 查看存储卷声明状态 (Local-path, NFS 或 OSS PVC 应均为 Bound 状态)
   kubectl get pvc -n default

   # 查看自动生成或绑定的 PV 及其存储类名称
   kubectl get pv
   ```

4. **验证 Agent 安全沙箱隔离效果**：

   ```bash
   # 1. 验证独立 Guest OS 内核 (Kata Pod 的内核版本与宿主机独立)
   kubectl exec -it <workspace-pod-name> -- uname -r

   # 2. 验证环境变量隔离 (确认不再包含其他 Workspace 的 SERVICE_HOST/PORT 变量)
   kubectl exec -it <workspace-pod-name> -- env | grep _SERVICE_

   # 3. 验证 API Token 阻断 (确认不存在 SA token 挂载)
   kubectl exec -it <workspace-pod-name> -- ls /var/run/secrets/kubernetes.io/serviceaccount

   # 4. 验证网络策略生效 (确认 NetworkPolicy 正常创建)
   kubectl get netpol -n default

   # 5. 验证云元数据与内网拦截 (请求 169.254.169.254 与内网 IP 将被丢弃超时)
   kubectl exec -it <workspace-pod-name> -- curl --connect-timeout 2 http://169.254.169.254/latest/meta-data/
   ```

5. 工作空间进入 `Running` 后，尝试写入数据文件，确认停止并重新启动后数据完美恢复。

