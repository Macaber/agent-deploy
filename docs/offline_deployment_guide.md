# Agent 工作空间 Operator - 离线生产环境部署方案

本文档为您提供了将该平台在**完全离线（Air-Gapped / 孤岛网络）**的生产集群上完成部署的详细指南，涵盖镜像打包、自定义 CRD 与 Operator 安装包生成、**本地物理盘动态存储 (Local Path)** 以及 **NAS / NFS 网络共享存储** 双方案部署、Ingress 网关安装及内网 DNS 解析。

---

## 准备工作流阶段

```mermaid
graph TD
    subgraph 有网环境 (开发机/中转机)
        A[1. 离线打包 K8s/IDE/Local-Path/NFS 镜像] --> B[2. 打包 Operator 一键安装包]
        B --> C[3. 收集 Ingress/Local-Path/NFS 部署清单]
    end
    
    C -->|U盘 / 移动硬盘中转| D[4. 离线集群导入私有仓库 Harbor]
    
    subgraph 离线生产环境 (K8s 集群)
        D --> E{5. 存储架构选型}
        E -->|方案 A: 极致性能| E1[部署 Local Path 本地动态存储]
        E -->|方案 B: 跨节点漂移| E2[部署 NAS / NFS 共享动态存储]
        E1 --> F[6. 部署 Ingress 路由网关]
        E2 --> F
        F --> G[7. 部署 CRD 与 Operator 控制器]
        G --> H[8. 部署/启动 API Server 页面管理服务]
    end
```

---

## 步骤 1：有网环境资源打包 (开发机)

在具备公网访问权限的开发机上，下载并打包部署所需的全部镜像与清单到统一的 `deploy` 目录下（同时包含 Local Path 与 NAS/NFS 所需的所有依赖镜像）：

```bash
# 1. 创建统一的离线部署资源目录
mkdir -p ./deploy/local

# 2. 拉取外部依赖及基础镜像 (注意：开发机为 Mac 时，必须指定 --platform linux/amd64 以确保拉取 x86 镜像)
docker pull --platform linux/amd64 smanx/opencode:latest
docker pull --platform linux/amd64 registry.k8s.io/ingress-nginx/controller:v1.9.4
docker pull --platform linux/amd64 registry.k8s.io/ingress-nginx/kube-webhook-certgen:v1.4.0

# Local Path 本地盘镜像
docker pull --platform linux/amd64 rancher/local-path-provisioner:v0.0.30
docker pull --platform linux/amd64 rancher/library-busybox:1.31.1

# NAS / NFS 动态存储镜像
docker pull --platform linux/amd64 registry.k8s.io/sig-storage/nfs-subdir-external-provisioner:v4.0.2

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

此时，开发机上的 **`deploy`** 目录已成为了一个**完全自建、开箱即用的离线交付包**，里面包含了所有的 `.tar` 镜像文件、`./local/` 本地盘清单以及 `./nfs/` NAS 清单。

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
docker load -i workspace-operator.tar
docker load -i api-server.tar

# (若是 containerd/ctr 环境: ctr -n=k8s.io image import <filename>.tar)
```

---

## 步骤 3：部署存储层 (双方案可选)

您可以根据生产业务需求，选择以下**任一存储方案**进行部署：

| 方案对比 | **方案 A：Local Path 本地物理盘** | **方案 B：NAS / NFS 网络共享存储** |
| :--- | :--- | :--- |
| **读写性能** | 🏆 **原生最高 (全 SSD 0 网络延迟)** | ⭐️⭐️⭐️ 一般 (受网络带宽/小文件RPC限制) |
| **Pod 跨物理机漂移** | ❌ 绑定原物理节点 | 🏆 **支持 (Pod 可在任意节点自动恢复)** |
| **共享工具包读取** | 🏆 本地物理盘直读 (0 延时) | 挂载共享 NFS 卷 |
| **适用场景** | 高频代码编译、npm/pip 安装、高频读写 | 物理节点易宕机、强要求数据跨机自动漂移 |

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
kubectl apply -f ./deploy/samples/shared_pv_template.yaml
kubectl apply -f ./deploy/samples/shared_pvc_template.yaml
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
   # 查看动态生成的个人独占 PVC 状态 (应为 Bound)
   kubectl get pvc -n bocomwork

   # 查看自动生成的 PV 及其存储类名称 (local-path 或 standard)
   kubectl get pv
   ```
4. 工作空间进入 `Running` 后，尝试写入数据文件，确认停止并重新启动后数据完美恢复。
