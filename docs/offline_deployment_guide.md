# Agent 工作空间 Operator - 离线生产环境部署方案

本文档为您提供了将该平台在**完全离线（Air-Gapped / 孤岛网络）**的生产集群上完成部署的详细指南，涵盖镜像打包、自定义 CRD 与 Operator 安装包生成、NFS/NAS 共享存储配置、Ingress 网关安装及内网 DNS 解析。

---

## 准备工作流阶段

```mermaid
graph TD
    subgraph 有网环境 (开发机/中转机)
        A[1. 离线打包 K8s/IDE 镜像] --> B[2. 打包 Operator 一键安装包]
        B --> C[3. 下载 Ingress/NFS 部署清单]
    end
    
    C -->|U盘 / 移动硬盘中转| D[4. 离线集群导入私有仓库 Harbor]
    
    subgraph 离线生产环境 (K8s 集群)
        D --> E[5. 部署 NAS / NFS 动态存储]
        E --> F[6. 部署 Ingress 路由网关]
        F --> G[7. 部署 CRD 与 Operator 控制器]
        G --> H[8. 部署/启动 API Server 页面管理服务]
    end
```

---

## 步骤 1：有网环境资源打包 (开发机)

在具备公网访问权限的开发机上，下载并打包部署所需的全部镜像与清单到统一的 `deploy` 目录下：

```bash
# 1. 创建统一的离线部署资源目录
mkdir -p ./deploy

# 2. 拉取外部依赖及基础镜像 (注意：开发机为 Mac 时，必须指定 --platform linux/amd64 以确保拉取 x86 镜像)
docker pull --platform linux/amd64 smanx/opencode:latest
docker pull --platform linux/amd64 registry.k8s.io/ingress-nginx/controller:v1.9.4
docker pull --platform linux/amd64 registry.k8s.io/ingress-nginx/kube-webhook-certgen:v1.4.0
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
docker save registry.k8s.io/sig-storage/nfs-subdir-external-provisioner:v4.0.2 -o ./deploy/nfs-provisioner.tar
docker save workspace-operator:v1.0.0 -o ./deploy/workspace-operator.tar
docker save api-server:v1.0.0 -o ./deploy/api-server.tar
```

### 1.2 生成与收集全部 YAML 部署清单

我们需要把平台部署所需的全部清单统一拷贝到 `deploy` 目录下：

```bash
cd /Users/yfsun/mywork/agent-deploy

# 1. 生成 Operator 的一键安装包 (dist/install.yaml)
GOWORK=off make build-installer IMG=workspace-operator:v1.0.0

# 2. 将生成的一键安装包、api-server-deploy.yaml 以及 ingress-deploy.yaml 拷贝到统一离线目录
cp dist/install.yaml ./deploy/

```

此时，开发机上的 **`deploy`** 目录已成为了一个**完全自建、开箱即用的离线交付包**，里面同时包含了所有的 `.tar` 镜像文件与 `.yaml` 部署资源。

---

## 步骤 2：离线导入私有镜像仓库

将整个 `deploy` 目录拷贝到离线环境的镜像节点上，导入镜像并推送（Push）到内部的私有镜像仓库（如 Harbor）：

```bash
cd ./deploy

# 1. 导入镜像到本地 docker 引擎
ctr -n k8s.io images import opencode.tar
ctr -n k8s.io images import ingress-controller.tar
ctr -n k8s.io images import ingress-certgen.tar
ctr -n k8s.io images import nfs-provisioner.tar
ctr -n k8s.io images import workspace-operator.tar
ctr -n k8s.io images import api-server.tar

```

---

## 步骤 3：部署 NAS (NFS) 共享存储

在多节点生产集群中，必须通过网络文件系统（NAS / NFS）对开发环境进行持久化。在离线 K8s 节点上，进入 `deploy` 目录执行部署：

1. **配置 RBAC 授权**：

   ```bash
   kubectl apply -f ./nfs/rbac.yaml
   ```

2. **部署 NFS 动态卷供给器**：
   修改 `./nfs/deployment.yaml` 模板：
   * 将 `image:` 字段修改为私有镜像仓库地址：`myharbor.local/sig-storage/nfs-subdir-external-provisioner:v4.0.2`
   * 配置 `env` 环境变量中的 `NFS_SERVER`（NAS 的 IP）和 `NFS_PATH`（NAS 上共享的根路径）。
   * 执行部署：

     ```bash
     kubectl apply -f ./nfs/deployment.yaml
     ```

3. **创建 StorageClass 存储类**：
   直接执行我们修改好的 `class.yaml`（已配置为默认存储类并命名为 `standard`）：

   ```bash
   kubectl apply -f ./nfs/class.yaml
   ```

---

## 步骤 4：部署 Ingress 路由网关 (Kubeadm 集群适配)

1. **修改配置文件**：
   打开修改好的 `./ingress-deploy.yaml`，将所有 `image:` 字段替换为您私有镜像仓库（Harbor）对应的内网地址。
2. **执行基础部署**：

   ```bash
   kubectl apply -f ./ingress-deploy.yaml
   ```

3. **针对 Kubeadm 集群进行流量暴露（选择以下任一方案）**：

   由于 Kubeadm 物理/虚拟机私有云部署默认不支持 `LoadBalancer` 类型的 Service，通常需要通过以下方式将 Ingress 网关暴露到集群外部：

   * **方案 A：HostNetwork 物理直通模式 (推荐，性能最好)**：
     直接让 Ingress Nginx 容器绑定宿主机的 `80` 和 `443` 端口。

     1. 编辑 Ingress Controller 部署：

        ```bash
        kubectl edit deployment ingress-nginx-controller -n ingress-nginx
        ```

     2. 在 `spec.template.spec` 区域下新增 `hostNetwork: true` 和 `dnsPolicy: ClusterFirstWithHostNet`：

        ```yaml
        spec:
          template:
            spec:
              hostNetwork: true              # 开启宿主机网络直通
              dnsPolicy: ClusterFirstWithHostNet # 保证容器在 hostNetwork 下能正常解析 K8s 内部 DNS
              containers:
              - name: controller
                ...
        ```

     3. 记录运行 Ingress 容器所在的**物理节点 IP**（外部即可通过此 IP 的 80/443 端口访问工作空间）。

   * **方案 B：NodePort 模式 + 外部反向代理 (硬件 F5 或 外部 Nginx/HAProxy)**：
     使用宿主机高端口进行流量映射，再由外部负载均衡反向代理。

     1. 获取 K8s 自动分配的宿主机 NodePort 端口：

        ```bash
        kubectl get svc ingress-nginx-controller -n ingress-nginx
        ```

        获取映射的端口（例如 `80:31253/TCP, 443:30864/TCP`）。

     2. 在外部的统一负载均衡器（如外网反代 Nginx/HAProxy）上配置 upstream，将 80/443 流量分发至 K8s 各个节点的 `31253` 和 `30864` 端口。

   * **方案 C：安装 MetalLB (私有云 LoadBalancer 模式)**：

     1. 在 Kubeadm 集群中安装 MetalLB 插件并为其配置局域网的 VIP IP 池。
     2. 将 Ingress Service 类型直接修改为 `LoadBalancer`：

        ```bash
        kubectl patch svc ingress-nginx-controller -n ingress-nginx -p '{"spec":{"type":"LoadBalancer"}}'
        ```

     3. 记录 MetalLB 自动为 Ingress 分配的 **`EXTERNAL-IP`**。

4. **开启 Ingress Snippet 注解权限 (Header 动态路由必须启用)**：

   新版本 Ingress-Nginx 默认禁用了 Configuration Snippet 注解。请在 Kubeadm 集群中执行以下命令以启用：

   ```bash
   kubectl patch configmap ingress-nginx-controller -n ingress-nginx -p '{"data":{"allow-snippet-annotations":"true"}}'
   ```

---

## 步骤 5：企业内网 DNS 泛域名解析

为方便用户访问各自独立的工作空间，需将整个泛域名解析指向 Ingress 的暴露入口。
在企业内网 DNS 服务商（或局域网 CoreDNS 服务中）配置泛域名解析（Wildcard A Record）：

```text
*.yourdomain.local    IN    A    <步骤 4 中获取的 Ingress 物理 IP>
```

配置完成后，无论后续 Operator 动态创建了多少个类似 `ws-user1.yourdomain.local` 的路由，流量均会被内网 DNS 自动解析转发给 Ingress 网关。

---

## 步骤 6：部署 Operator 与 API Server

在离线 K8s 节点上，进入 `deploy` 目录，执行平台应用的拉起：

1. **部署自定义 CRD 及 Operator 控制器**：

   ```bash
   kubectl apply -f ./install.yaml
   ```

   * *注意：Operator 默认在 `default` namespace 或您指定的 Namespace 下以 Pod 运行。它在集群内运行且没有设置 `WORKSPACE_DATA_DIR` 环境变量时，会自动检测为生产集群状态，**自动将挂载模式切换为 PVC（NAS）持久化模式**。*

2. **部署并启动 API Server 管理后台**：

   我们在有网环境已经打包好了 `api-server.tar` 镜像，并已推送到私有 Harbor。现在只需应用其部署清单：

   1. 编辑 `./api-server-deploy.yaml`，将镜像地址修改为您已推送的 Harbor 地址，将 `WORKSPACE_DOMAIN` 修改为您的实际物理域名（如 `yourdomain.local`）。
   2. 运行应用部署（会自动创建 ServiceAccount 并授权 RBAC 角色）：

      ```bash
      kubectl apply -f ./api-server-deploy.yaml
      ```

   3. **网络访问方式**：
      * **方式一**：通过 NodePort 端口直接访问，地址为 `http://<任意节点IP>:30000`。
      * **方式二**：通过 Ingress 域名访问，将域名 `launcher.workspace.localhost`（可在 YAML 中自定义）解析到 Ingress 网关 IP，然后浏览器直接访问。

---

## 步骤 7：验证持久化与运行

1. 在 API Server launcher 页面选择一个用户，点击“启动工作空间”。
2. 工作空间就绪后，点击生成的 `http://ws-<user>.yourdomain.local` 链接访问 `opencode` IDE。
3. 输入一些配置，创建聊天会话，然后点击“关闭工作空间”（容器副本数缩容至 0，Pod 被销毁，但 NAS 上的存储卷保留）。
4. 再次点击“启动工作空间”，容器恢复就绪，进入 IDE 验证所有的历史会话数据是否完美恢复。
