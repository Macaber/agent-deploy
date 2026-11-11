# 统一出站白名单：实现、部署与会话交接

更新日期：2026-11-09（Asia/Shanghai）

## 2026-11-10：IP/CIDR 同时授权代理访问（尚未现场部署）

- IPv4 TCP 规则同时生成 NetworkPolicy 直连规则和 Envoy HTTP/CONNECT 路由。只匹配规范点分十进制 IP 与指定端口；HTTP 仅在端口 80 时允许省略端口。CONNECT 必须显式指定获准端口。
- IPv6 和 UDP 规则仍仅适用于直连。域名授权保持独立，未列出的域名不会因解析到获准 IP 获得代理权限。CONNECT 仍是 TCP 隧道，不校验隧道内协议或 TLS 身份。
- 不修改 NO_PROXY，不重启 Workspace；后续规则保存仍通过文件 RDS 热更新。既有隧道不保证在删除规则时立即断开。
- guard 的基础设施保护范围保持不变，IP 白名单不覆盖代理的保护范围。目标业务返回的 403 不代表网络未放行。
- 新镜像标签 `api-server:egress-unified-20261110-ip`。本次需要同时更新代理的 initialize-config 和 sync 镜像并滚动更新代理 Pod，因为 bootstrap 增加了有界 RE2 程序复杂度设置。只替换运行中 sync 而不重新初始化 bootstrap 不构成完整升级。API 使用同一镜像更新页面说明。
- 下文此前仅域名代理、IP 仅直连的描述为历史版本行为，以本节为准。
- 本次验证：`go test ./internal/egress ./cmd/egress-sync ./cmd/api-server`、相关包 `go vet`、前端构建和批量解析测试通过。Docker 服务查询无响应，尚未进行新版真实 Envoy HTTP/CONNECT、热更新和 guard 联合实验；上线前需隔离环境验证。完整 lint 工具在本机缓存中缺失。

## 最新代码变更：共用镜像与批量录入（尚未现场部署）

- 当前代码以 `Dockerfile.api-server` 同时构建 `/api-server` 和 `/egress-sync`。API 默认启动前者，代理的 initialize-config 与 sync 使用 `command: ["/egress-sync"]`；就绪探针仍使用 `/egress-sync --ready`。
- 新清单标签为 `api-server:egress-unified-20261109`；此标签需要重新构建，不能使用不含 sync 程序的旧 API 镜像。API 和代理引用同一镜像，仍是独立 Deployment；原有 UID、权限、共享卷和状态确认机制保持不变。
- 标准构建：先在 `web/portal` 执行 `npm ci`、`npm run build`，然后在仓库根目录执行 `docker build --platform linux/amd64 -f Dockerfile.api-server -t api-server:egress-unified-20261109 .`。导出该镜像并导入实际调度节点，或推送内网仓库并统一替换清单镜像地址。
- 常规发布镜像由五种减少为四种：Operator、API（含 sync）、guard、Envoy。`Dockerfile.egress-sync` 仅保留为历史独立构建入口，不再是常规构建步骤。
- 更新代理前确认新镜像已到位；API 与代理按需分别滚动更新。本文后续 r2 标签与五镜像包描述为旧现场快照。
- 网络配置页每类规则提供批量输入：每行一条，Tab 或空格分隔；IP 为 `CIDR 协议 端口`，域名为 `域名 端口`，基础服务为 `用途 命名空间 标签 协议 端口`。用途含空格时用 Tab 分隔；多个标签用逗号分隔。
- “追加到表单”保留已有规则，不会提交线上；检查后点击“保存配置”，沿用后端校验及 resourceVersion 并发保护。“复制当前规则”包含未保存表单内容。浏览器限制剪贴板 API 时使用输入框 Ctrl+C / Ctrl+V。
- 批量解析回归检查：`node web/portal/scripts/test-egress-bulk.mjs`。本次前端构建与解析测试通过；镜像构建及集群运行尚待验证。

## 1. 新会话先读

- 实际开发仓库：`D:\E\Code\TS\agent-deploy`；分支：`feature-xjw-networkpolicy-envoy`。
- 不要把 `C:\Users\together\Documents\ChatGPT\agent-deploy` 或其中旧副本当作当前开发基线。
- 先阅读根目录 `AGENTS.md`，检查 Git 状态。2026-11-09 核对时，大量实现仍为未提交修改/未跟踪文件，不能认为切换分支或重新 clone 后就能获得全部实现。不要 reset、clean 或覆盖工作区。
- 旧分支 `feature-xjw-networkpolicy` 的方案废弃，不合并、不 cherry-pick、不整体复制。历史记录显示旧修改已 stash，操作前现场核对，不能删除。
- 本文用于接续工作；`docs/egress-envoy-progress.md` 是历史记录，部分阶段性描述可能过时，以当前代码和现场查询为准。
- 无直接现场集群连接；现场操作由用户执行，结论来自用户截图/反馈。下文现场地址及镜像是已知快照，不保证未来不变。
- 用户已确认 IP 直连、域名代理的基本允许/拒绝行为符合预期；健康检查误走代理问题也已由用户确认修复。不能把这些结果等同于所有需求及生产验收完成。

## 2. 功能边界与架构

统一管理入口是现有 API Server 内嵌的 Workspace 管理前端，不新增管理站点，不要求管理员写 Envoy 路由 YAML。

| 规则 | 路径 | 执行组件 |
| --- | --- | --- |
| IP/CIDR＋TCP/UDP＋端口 | Workspace 直接访问目标 | Kubernetes NetworkPolicy |
| 完整域名＋端口，HTTP/HTTPS | Workspace → 共享 Envoy → 目标 | Envoy 显式正向代理 |
| 明确配置的基础服务 | Workspace 直连所选命名空间/Pod/端口 | NetworkPolicy |

- 域名目标 IP 由 Envoy DNS 机制处理，不由 Operator 定期解析并写入 NetworkPolicy。
- 无企业内网或公网默认放行；IP 直连放行可能绕过域名检查，必须了解两条路径的授权区别。
- 不支持域名通配符、URL 路径规则及非 HTTP/HTTPS 域名代理；不解密 TLS。CONNECT 授权目标域名/端口，不等于校验隧道内 TLS 网站身份。
- Envoy 是共享 Deployment＋Service，不是每个 Workspace 一个代理，不需要改造原模型网关。
- 仅收紧出站，保持已有入站行为；不能声称一并解决用户鉴权、共享存储、OSS 凭据隔离。
- 不承诺 DNS 换址零中断，也不承诺删除规则后立即断开已有隧道。

## 3. 配置与生效链路

默认配置命名空间：`agent-deploy-system`，可通过 `EGRESS_CONFIG_NAMESPACE` 指定。

### 业务白名单 ConfigMap

`workspace-egress-config`，数据键 `policy.json`，包含 `ips`、`domains`、`services`。

- 页面首次保存时由 API 创建，不随 `egress-proxy.yaml` 自动创建。
- API 使用 resourceVersion 乐观锁防止并发编辑覆盖；版本冲突须刷新后再保存。
- 校验完整域名、端口、协议、规范 CIDR、重复项等；保存成功不等于所有执行组件已生效。
- 首次迁移前须有明确 DNS TCP/UDP 53 规则；空业务放行可以保存，但缺 DNS 不满足迁移检查。
- `GET/PUT /api/egress/config`、`GET /api/egress/status` 沿用可选登录：无 USERNAME 时允许匿名；已配置账号则校验现有登录态。没有增加强制账号/非空密码限制；普通 Workspace API 鉴权未擅自改变。

### 代理基础边界 ConfigMap

`workspace-egress-boundary`，供 guard 初始化容器读取：

- `EGRESS_DNS_SERVERS`：明确 DNS IPv4 地址，逗号分隔。
- `EGRESS_PROTECTED_CIDRS`：禁止代理访问的 Pod、Service、节点及管理地址范围，逗号分隔。
- 这是部署维护的禁止范围，不是第二套业务放行规则。需按完整基础设施清单维护，不能认为当前列出的三个节点已覆盖所有管理入口/VIP。
- 域名命中白名单后，目标 IP 仍不能落入保护范围，防止域名解析指向 Workspace/管理地址造成绕过。
- guard 另保护本机、链路本地/元数据等地址，IPv6 新连接拒绝；DNS 例外限定地址及端口，并处理 DNAT 原始目标匹配。
- 修改基础边界需重新运行 init 容器，即代理 Pod 滚动更新；日常业务规则不应要求重启。

### 动态加载

1. 页面 → API → 白名单 ConfigMap。
2. sync 监听 ConfigMap，校验并通过同目录临时文件＋原子 rename 写入共享卷的 routes.json。
3. 固定 Envoy bootstrap 使用文件 RDS 加载路由；不能随意修改启动 YAML 并期待热更新。未引入完整 xDS 管理服务。
4. sync 从 Envoy 本机 admin 9901 的 config_dump 读取实际路由版本。
5. 通过 Pod 注解 `workspace.bocom.io/egress-status` 回报 desiredRevision、loadedRevision、state、error、observedAt、podUID；本地 9902 就绪检查与加载状态关联。
6. API 聚合副本数、Pod 就绪、UID、版本与心跳，不以“文件已写入”代替 Envoy 实际加载。

当前重要限制：API 的 `policyState` 和 `migrationState` 仍返回 `unverified`。不能从页面代理 loaded 推断所有 NetworkPolicy 已应用或全部 Workspace 已迁移。完整迁移状态展示尚待完善。

## 4. Workspace 接管与网络策略

- `EGRESS_ENABLED=true` 才启用统一出站接管。
- Operator 同时需要配置 `EGRESS_CONFIG_NAMESPACE` 和 `EGRESS_PROTECTED_CIDRS`；只给 guard 配边界不会自动传给 Operator。
- 迁移标记：`workspace.bocom.io/egress-version=envoy-v1`。
- 未迁移且仍有 Pod（包括终止中的 Pod）时，不主动修改模板强制重建。优先等自然休眠、下次唤醒迁移。
- 测试环境用户允许加速迁移：从管理页面休眠，确认旧 Pod 消失，再从页面唤醒；不要直接删除 Pod 或暂停/缩容 Operator。
- 接管前检查 DNS、保护范围、所有代理副本实际加载版本、叠加策略冲突。某些条件不满足会导致创建/唤醒被拒绝，应查 Operator 日志。
- `workspace-egress-v1` 是迁移 Pod 的严格出站策略；允许代理 TCP 3128、页面直连规则及基础服务规则。
- `workspace-shared-ingress` 用于保持旧入站行为；旧 `workspace-shared-netpol` 排除迁移 Pod，避免宽泛出站叠加。
- 自身 Pod IP/本机访问应单独现场验证；不能为实现“自身访问”放开整个 Pod 网段。
- NetworkPolicy 允许范围取并集；管理员创建的其他出站策略可能额外放行。代码会拒绝存在匹配候选 Pod 且带额外出站允许的其他策略时的迁移。
- 现场 `workspace-test-whitelist` 未删除：将其选择器保留 `app=workspace`，增加迁移标签 `NotIn [envoy-v1]`，只覆盖未迁移 Pod。后续不得重新 apply 原始宽泛选择器覆盖这个修改。
- disabled 历史状态也要检查是否已迁移，不能仅凭新 Pod 测试宣称覆盖所有 Workspace。
- 不要把 `EGRESS_ENABLED=false` 当作完整回滚：标签、代理环境变量、已存在 NetworkPolicy 不会自动全部恢复；回滚需核对并恢复成套配置。

## 5. 代理环境变量和本地健康检查

Operator 注入大小写 HTTP_PROXY/HTTPS_PROXY；清理旧 ALL_PROXY 等冲突变量，并通过 Downward API 提供自身 Pod IP。

2026-11-09 代码中的 NO_PROXY/no_proxy 为：

`localhost,127.0.0.1,::1,$(WORKSPACE_POD_IP),.svc,.svc.cluster.local`

- HTTP_PROXY 不会自动实现“所有 IP 直连、所有域名代理”。各客户端对环境变量/CIDR/NO_PROXY 的支持不同，必须验证实际 TS/Node/Python/模型客户端。
- `curl --noproxy '*'` 明确直连；`curl --noproxy '' --proxy ...` 明确使用代理；普通 curl 通常使用注入的代理环境变量。
- NO_PROXY 仅影响路由选择，不绕过 NetworkPolicy。

### 已遇到的健康检查问题

容器内部请求 `http://0.0.0.0:3001/health`，因 NO_PROXY 未包含 0.0.0.0，被发到 Envoy，返回 `server: envoy` 的 403。强制直连 0.0.0.0、直连 127.0.0.1 均返回 200。

- 服务仍应监听 `0.0.0.0:3001`，方便 Pod 外前端访问。
- 本地检查请求可使用 `http://127.0.0.1:3001/health` 并明确不使用代理；这不要求改服务监听或前端 URL。
- 将 0.0.0.0/32 加入 IP 白名单不能解决误走代理问题；绝不能加 0.0.0.0/0 来“修复”。
- 用户在最新会话明确表示此问题已修复，但未提供最终脚本/镜像/环境改法。接手人应确认并记录；本仓库当前 NO_PROXY 仍未包含 0.0.0.0，不得声称 Operator 已加入该兼容修复。
- 脚本名用户称 `startpord.sh`，位于 Workspace 镜像内。不要未经核实推断该脚本已在此仓库修改。

## 6. 现场快照（来自用户截图及反馈）

- Kubernetes v1.25.5；containerd 1.7.22；Calico 历史确认 v3.26.4。
- Workspace 命名空间：`bocomwork`。
- 节点：bocomworkuat2/3/4，对应 12.244.142.251/252/253。
- Calico 地址池：`10.244.0.0/16`（不要只用 Node.spec.podCIDR 三个 /24 推断实际池）。
- Service 范围：`10.243.0.0/16`；kube-dns：`10.243.0.10`，标签 `k8s-app=kube-dns`。
- Operator：`agent-deploy-system/agent-deploy-controller-manager`；nodeSelector `kubernetes.io/hostname=bocomworkuat2`，必须保留。
- API：`default/api-server`，ServiceAccount `api-server-sa`；截图为 2 个就绪副本。
- 代理：`agent-deploy-system/workspace-egress`，Service TCP 3128；两个副本都固定 UAT2，因为只确认该节点导入全部镜像。这是测试部署，不是跨节点高可用。
- 代理 Service 曾为 `10.243.184.193`，不要硬编码依赖这个快照地址。
- 当前明确配置的保护范围：`10.244.0.0/16,10.243.0.0/16,12.244.142.251/32,12.244.142.252/32,12.244.142.253/32`。其他管理地址完整性待核查。
- 后续用户已设置 Operator EGRESS_ENABLED=true 并完成测试 Workspace 的休眠/唤醒，截图出现 envoy-v1 标签。
- 旧测试策略历史允许 DNS 与 `12.244.167.46/32:80`；旧 shared 策略放行除常见私网外的 0/0，导致 12.244.* 原先被放行。

## 7. 镜像、清单和许可证

| 镜像 | 现场测试标签 |
| --- | --- |
| workspace-operator | egress-test-20261106 |
| api-server | egress-test-20261106 |
| workspace-egress-guard | egress-test-20261106 |
| workspace-egress-sync | egress-test-20261106-r2（已修复加载版本问题） |
| workspace-envoy | egress-test-20261106 |

Envoy 上游版本 v1.40.2，固定 digest：
`sha256:8de97e654644827d62afdbf7e1d3dc9fee01a8690d410dd1c63a30dc6b6cecbb`

- 原 5 镜像包：`deploy/egress-images-egress-test-20261106.tar`，其中 sync 是旧版本！
- 单独修复包：`deploy/egress-sync-egress-test-20261106-r2.tar`，约 20 MB。使用旧大包必须另行导入 r2。
- `deploy/egress-proxy.yaml` 的 initialize-config 和 sync 两处都引用 r2；guard、Envoy 标签不变。
- 内网使用 `ctr -n k8s.io images import 文件.tar`，仅 docker load 不代表 kubelet 可见。镜像需在实际调度节点导入或上传内网仓库。
- `deploy/api-server-deploy.yaml` 追加了 operator 命名空间中的 Role/RoleBinding，授予 default/api-server-sa 配置读写、Pod 状态读取及代理 Deployment 读取。ConfigMap create 无法按 resourceName 限制；get/update 限定 workspace-egress-config。
- `deploy/install.yaml` 是 Operator/CRD/RBAC 安装清单，不包含整套 API 和独立代理部署。生成后必须核对环境变量、nodeSelector、镜像，不可盲目覆盖现场。
- Envoy 主项目 Apache-2.0 不等于镜像全部依赖 Apache-2.0。guard 安装固定 `iptables=1.8.7-1ubuntu5.2`，含其他许可证依赖。`docs/egress-licenses/` 是已有材料，不代表完整 SBOM/公司许可证审核已完成。

### 构建要点

- Windows PowerShell＋Docker Desktop Linux 引擎可构建 linux/amd64；无需为了 docker build 进入 WSL。
- 前端先执行 npm ci、npm run build（web/portal），构建结果复制到 cmd/api-server/portal.html，再构建 API 镜像。
- sync/guard Dockerfile 要求预先以 CGO_ENABLED=0、GOOS=linux、GOARCH=amd64 编译到 bin/egress-sync-linux-amd64 和 bin/egress-guard-linux-amd64。
- Operator/API 常规 Dockerfile 使用 golang:1.26 编译，distroless static:nonroot 运行。曾因 Docker Hub 认证域名解析/连接失败，改为本机 Go 交叉编译＋临时 Dockerfile 封装，同样使用原运行时和非 root 用户。
- 项目 go.mod 要求 Go 1.26.0；用户 WSL 安装了 Go 1.27.3。Windows Go 不等于 WSL Go。
- WSL 生成清单：`GOWORK=off make build-installer IMG=workspace-operator:egress-test-20261106`。输出 deploy/install.yaml，会再生 CRD/RBAC 并修改 manager 镜像引用，不会部署。
- 用户访问 proxy.golang.org 超时，曾临时使用 GOPROXY=https://goproxy.cn,direct；公司有内部代理时优先使用，不关闭校验。

## 8. 已修复的重要缺陷：sync r2

现象：只有 DNS/IP 规则变化时，两个代理长时间 1/2 Ready；状态 error 为空、state=syncing，desiredRevision 与 loadedRevision 不同。

原因：统一版本哈希覆盖所有规则，但 RDS RouteConfiguration 原先只由域名规则生成。只有 envelope version_info 改变而资源内容不变时，Envoy 去重并保留旧加载版本。

修复：在 RouteConfiguration.metadata.filter_metadata["workspace.egress"].revision 中加入统一配置版本，使每次白名单版本变化都反映到资源内容。保留实际 config_dump 版本核验，不伪造 loaded。

验证：本机隔离 Envoy 复现了仅版本更新不刷新、增加资源元数据后加载新版本；相关 Go 测试通过；用户部署 r2 后两个代理均 2/2 Running，rollout 成功。

代码：internal/egress/envoy.go；回归测试 internal/egress/envoy_test.go。仅修复此问题时只需更新 sync 镜像；生成的初始配置也由 initialize-config 使用同一镜像。

## 9. 验证结论与仍需验收的事项

### 用户已确认的现场结果

- 代理两个副本启动就绪；r2 解决版本不同步。
- 迁移测试 Pod 的未授权 IP 强制直连被阻断。
- IP 规则配置后直连可访问。
- 域名白名单经显式代理可访问；强制绕过代理无法靠域名规则直连，符合设计。
- 代理拒绝未授权请求返回 403。
- 本地健康检查误走代理原因定位，用户确认已修复（具体改法未记录）。

### 不应直接宣称完成的验收

- 全部现存 Workspace（包括历史 disabled）覆盖、迁移状态页面、所有叠加策略审计。
- 现场每个真实客户端对代理/NO_PROXY 的支持；模型 SSE、长连接、取消、超时和并发。
- 现场白名单新增/删除不重启、删除后新连接拒绝、并发修改冲突、失败反馈。
- 现场 DNS 多 IP、换址、故障恢复、DNS 指向受保护目标的行为。
- Workspace 间隔离、自身 Pod IP、节点管理/元数据绕过、完整基础设施保护范围。
- 多节点高可用、完整镜像许可证/SBOM、完整回滚演练。
- 本地已有部分 Docker 隔离 HTTP/CONNECT、热更新、SSE、并发、取消和 guard 实验，但不等同于 Calico 集群端到端验收；旧分支的测试不作为本方案验收依据。

## 10. 排障与测试命令

以下管理命令在有 kubeconfig 的管理终端执行：

```bash
kubectl -n bocomwork get workspaces
kubectl -n bocomwork get deployment,pods -o wide
kubectl -n bocomwork get pods --show-labels
kubectl -n bocomwork get networkpolicy -o yaml
kubectl -n agent-deploy-system get pods -o wide
kubectl -n agent-deploy-system get configmap workspace-egress-config workspace-egress-boundary
kubectl -n agent-deploy-system logs deployment/agent-deploy-controller-manager -c manager --since=15m --tail=150
kubectl -n bocomwork get events --sort-by=.metadata.creationTimestamp
kubectl -n agent-deploy-system logs -l app.kubernetes.io/name=workspace-egress -c sync --tail=60 --prefix=true
kubectl -n agent-deploy-system logs -l app.kubernetes.io/name=workspace-egress -c envoy --tail=40 --prefix=true
kubectl -n agent-deploy-system get pods -l app.kubernetes.io/name=workspace-egress -o go-template='{{range .items}}{{.metadata.name}}{{"\n"}}{{index .metadata.annotations "workspace.bocom.io/egress-status"}}{{"\n\n"}}{{end}}'
```

sync 的某些错误只写注解，不打印日志；日志为空不代表同步成功。

下面的 curl 必须在 Workspace Pod 内运行（先 exec，或用 kubectl exec POD -- curl ...），不要在节点上测试后误认为 Workspace 策略无效：

```bash
# 直连测试，单引号内是星号本身，不要加反斜杠
curl --noproxy '*' -v --connect-timeout 5 --max-time 10 http://12.244.130.39:80/

# 显式代理测试：替换成页面批准的真实域名/端口
curl --noproxy '' --proxy http://workspace-egress.agent-deploy-system.svc.cluster.local:3128 -v --connect-timeout 5 --max-time 15 https://实际域名:443/

# 本地健康检查
curl --noproxy '*' --fail --max-time 5 http://127.0.0.1:3001/health
```

判断结果：
- 返回目标 nginx 403/业务 JSON：目标网络已连通，不能当作 NetworkPolicy 拒绝。
- 已连接 Envoy，返回其本地 403：代理规则拒绝；响应头、连接目标和配置共同确认，不能只看状态码。
- 未能连接代理：查 Workspace 出站、Service endpoint、代理入站，不是域名规则拒绝。
- 503：查 DNS、上游连通性及 guard 保护范围。
- kubectl exec 失败：先查容器状态，不能视作网络阻断成功。
- CrashLoopBackOff：看实际 Pod 的 logs --previous 与 describe，区分本地检查误走代理、远程依赖被拒绝、探针或进程错误。

## 11. 代码导航与后续工作约束

- internal/egress/config.go、store.go：数据结构、校验、版本与 ConfigMap 存储。
- internal/egress/envoy.go、sync.go、status.go：路由/bootstrap、原子更新、实际加载检查及副本状态。
- internal/egress/policy.go、preflight.go、guard.go：严格策略、迁移前置条件、代理本机防护命令。
- cmd/egress-sync、cmd/egress-guard：两个新增配套组件。
- internal/controller/egress.go、workspace_controller.go：迁移、环境变量、策略协调。
- cmd/api-server/egress.go、egress_status.go：接口及状态。
- web/portal/src/components/egress-view.tsx：页面。
- deploy/egress-proxy.yaml、deploy/api-server-deploy.yaml、deploy-ack/api-server-deploy.yaml：部署和权限。
- test/egress/：隔离测试辅助程序；.tmp/ 下是本地实验/预览文件，不是发布清单。

用户曾讨论独立 Envoy 入站反向网关替代每 Workspace Ingress，但只授权评估，未实施；不要把它混入本次出站工作。

继续开发时遵守 AGENTS.md：生成文件用工具再生，相关 Go 检查，端到端测试在隔离 Kind 环境。现场不要暂停/缩容 Operator，不直接删除旧策略，不擅自强制重建运行中 Workspace。用户明确授权的测试休眠/唤醒按本次范围处理，不推导为生产随意重启授权。
