# agent-deploy (Workspace Operator)

基于 Kubernetes 自定义资源（CRD）的云端 Agent 工作空间管理器。集成 KEDA 实现了按需自动扩缩容（缩容至 0 副本），并内置了空闲超时自动休眠控制器，为每个用户/智能体提供安全隔离、按需启动且持久化的工作环境。

## 项目描述

`agent-deploy` 项目通过引入自定义资源 `Workspace`，将底层 Pod/Container、PVC、Service、Ingress 以及 KEDA `ScaledObject` 封装起来。平台可以通过声明式 API 直接为用户创建 `Workspace` 实例，剩下的资源调度、Git 仓库拉取、按需唤醒（HTTP 流量触发从 0 到 1 扩容）和空闲自动休眠（缩容至 0 副本）都由 Operator 自动处理。

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

2. 使用部署包安装

最终用户只需运行以下 `kubectl` 命令即可一键安装该项目：

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/agent-deploy/<tag or branch>/dist/install.yaml
```

### 方式二：提供 Helm Chart 包

1. 使用 Kubebuilder 的 Helm 插件生成 Chart 脚手架：

```sh
./bin/kubebuilder edit --plugins=helm/v2-alpha
```

2. 该命令会在 `dist/chart` 目录下生成 Helm Chart 结构，最终用户可以通过 Helm 从该目录或您发布的 Chart 仓库来安装：

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
