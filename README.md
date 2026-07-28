# agent-deploy (Workspace Operator)

基于 Kubernetes 自定义资源（CRD）的云端 Agent 工作空间管理器。内置基于 `idleTimeout` + `lastActiveTime` 的会话超时休眠，由 Operator 统一管理 Deployment 副本，为每个用户/智能体提供安全隔离、按需启动且持久化的工作环境。

## 项目描述

`agent-deploy` 项目通过引入自定义资源 `Workspace`，将底层 Deployment、PVC、Service、Ingress 封装起来。平台可通过声明式 API 创建 `Workspace` 实例，由 Operator 完成资源编排与生命周期管理。

**核心特性：**

- **存储与配置挂载：** 支持独立工作区 PVC、预先存在的共享存储卷（`sharedVolumeMounts`）以及 Kubernetes ConfigMap 卷挂载（`configMapVolumeMounts`），方便为 Agent 注入共享资源和外部配置文件（如 `bocomwork-config`）。
- **副本与休眠：**
  - `stopped=true`：Operator 将 Deployment 副本置为 0（手动停止）。
  - 配置了 `idleTimeout`：自 `status.lastActiveTime` 起超过该时长后缩容为 0（`Sleeping`）。`lastActiveTime` 在创建/API 唤醒时刷新，**不是** Ingress 实时流量闲置探测。
  - 省略 `idleTimeout`：不会按会话窗口自动休眠，副本由 Operator 保持为 1（除非手动停止）。

---

## 项目目录结构说明

本项目包含 Kubernetes Operator 控制器与 API 网关（API-Server）两部分，整体文件布局与说明如下：

```text
├── api/                    # K8s 自定义资源 (CRD) 架构规范定义
│   └── v1alpha1/
│       ├── workspace_types.go # 核心：定义了 Workspace 的配置字段 (Spec) 与状态字段 (Status)
│       └── groupversion_info.go # 定义 API 组与版本元信息 (ai.example.com/v1alpha1)
│
├── cmd/                    # 应用程序启动入口
│   ├── main.go             # 核心：Operator 启动程序（注册控制器并启动调和主循环）
│   └── api-server/
│       └── main.go         # 核心：对外暴露的轻量级 RESTful API 网关服务器
│
├── internal/               # 平台核心业务逻辑
│   └── controller/
│       ├── workspace_controller.go # 核心：工作空间控制器实现（负责编排创建/休眠/唤醒等逻辑）
│       └── workspace_controller_test.go # 单元测试文件
│
├── config/                 # Kubebuilder / Kustomize 编排生成物，用于 K8s 集群部署配置
│   ├── crd/                # 自动生成的 CRD 资源文件 (Yaml)
│   ├── rbac/               # 自动生成的 RBAC 授权文件（安全策略相关）
│   ├── manager/            # Operator 控制器 Deployment 基础配置文件
│   └── samples/            # 样本 Workspace 实例 YAML 示例配置
│
├── deploy/         # 离线交付部署包目录 (专用于生产无网环境下部署)
│   ├── nfs/                # NFS-client-provisioner 动态分配器的 K8s 配置
│   ├── api-server-deploy.yaml # API-Server 网关的 K8s 部署配置
│   ├── ingress-deploy.yaml    # Ingress-Nginx 网关控制器的 K8s 部署配置
│   ├── install.yaml           # 本项目 Operator 的一键式部署清单
│   └── docs/                  # 离线部署辅助手册
│
├── docs/                   # 项目主要系统设计与操作文档
│   ├── api_manual.md          # API-Server 网关接口调用手册 (Curl 示例)
│   ├── workspace_user_manual.md # 工作空间使用与规格定义手册
│   └── deployment_architecture.md # 系统整体架构设计与组件协作说明
│
├── bin/                    # 内部编译出的可执行文件及 envtest 环境包 (K8s API 本地模拟)
├── test/                   # 集群端到端 (E2E) 测试代码
├── Dockerfile              # Operator 镜像打包配置
├── Dockerfile.api-server   # API-Server 镜像打包配置
├── PROJECT                 # Kubebuilder 项目脚手架配置文件
└── Makefile                # 项目生命周期构建命令脚本 (如 make manifests, make generate)
```

---

## 快速入门

### 前提条件

- Go 语言版本: `v1.24.6+` (或更高)
- Docker 版本: `17.03+`
- Kubectl 版本: `v1.11.3+`
- 可访问的 Kubernetes 集群 (`v1.11.3+`)

### 部署到集群中

**1. 构建控制器镜像并推送到指定的镜像仓库 `IMG`:**

```sh
GOWORK=off make docker-build docker-push IMG=<your-registry>/agent-deploy:tag
```

> **注意**: 该镜像需要推送到您有权限访问的个人或公共镜像仓库中，确保集群节点能够拉取该镜像。

**2. 在集群中安装 CRD (Custom Resource Definition):**

```sh
GOWORK=off make install
```

**3. 在集群中部署 Manager 控制器，使用刚才推送的 `IMG` 镜像:**

```sh
GOWORK=off make deploy IMG=<your-registry>/agent-deploy:tag
```

> **注意**: 如果在部署过程中遇到 RBAC 权限错误，请确保您在集群中拥有 `cluster-admin` 权限或当前以 admin 身份登录。

**4. 创建工作空间实例**
您可以使用 `config/samples/` 中的示例 YAML 资源来测试：

```sh
kubectl apply -k config/samples/
```

> **注意**: 请在应用前确保样本 manifest 中已配置符合测试环境的默认值。

---

### 卸载清理

**1. 从集群中删除已创建的工作空间实例 (CR):**

```sh
kubectl delete -k config/samples/
```

**2. 从集群中卸载 CRD (APIs):**

```sh
GOWORK=off make uninstall
```

**3. 从集群中卸载 Manager 控制器:**

```sh
GOWORK=off make undeploy
```

---

## 项目发布与分发

您可以选择以下方式将此解决方案发布并分发给最终用户：

### 方式一：提供单文件 YAML 部署包 (Kustomize)

1. 生成包含指定镜像的单文件部署包：

```sh
GOWORK=off make build-installer IMG=<your-registry>/agent-deploy:tag
```

> **注意**: 上述命令将在 `dist` 目录下自动生成一个 `install.yaml` 文件。该文件包含了通过 Kustomize 打包好的所有资源声明（CRD、RBAC、Deployment 等），用户无需安装其他依赖即可一键安装本项目。

1. 使用部署包安装

最终用户只需运行以下 `kubectl` 命令即可一键安装该项目：

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/agent-deploy/<tag or branch>/dist/install.yaml
```

### 方式二：提供 Helm Chart 包

1. 使用 Kubebuilder 的 Helm 插件生成 Chart 脚手架：

```sh
./bin/kubebuilder edit --plugins=helm/v2-alpha
```

1. 该命令会在 `dist/chart` 目录下生成 Helm Chart 结构，最终用户可以通过 Helm 从该目录或您发布的 Chart 仓库来安装：

```sh
helm install my-release ./dist/chart/ --namespace agent-system --create-namespace
```

> **注意**: 如果您后续修改了项目的配置或 API 定义，需要重新运行上述命令来同步最新的更改。另外，如果您添加了 Webhook，再次运行时需要加上 `--force` 参数，并且在执行后手动恢复之前对 `dist/chart/values.yaml` 或 `dist/chart/manager/manager.yaml` 所做的任何自定义修改。

---

## 参与贡献

欢迎对 `agent-deploy` 项目做出贡献！您可以提交 Issue、参与讨论或提交 Pull Request。

- 运行 `make help` 可以查看所有可用的 Makefile 指令。
- 更多详细信息请参阅 [Kubebuilder 官方文档](https://book.kubebuilder.io/introduction.html)。

---

## 开源协议

Copyright 2026.

本项目遵循 Apache 2.0 开源协议。详情参见 [LICENSE](http://www.apache.org/licenses/LICENSE-2.0)。
