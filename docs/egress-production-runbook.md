# 统一出站控制投产手册（双集群 · 2026-11-20）

> 更新：2026-11-16。**本手册完全替代 2026-11-12 版**，是 11-20 投产的唯一执行依据。Harbor 入库路线已删除（现场无仓库，走 tar + ctr 导入）。
> 写给执行人自己看：每一步都说明**干什么、怎么算成功、失败了查哪里**。概念看不懂翻第 3 节速查表。
> 原理与设计细节见 [egress-envoy-handover.md](egress-envoy-handover.md)、[egress-mode-design.md](egress-mode-design.md)，本手册只保留执行所需的最小原理。

## 0. 本次投产的范围

**一句话**：两个生产集群（新/旧）各新增一套共享出站代理组件（workspace-egress），同时把 Operator/API 升级到支持白名单的版本；首次上线保持 **legacy（默认）模式，网络行为与现在完全一致**。

当天流程一览：

```
体检 → 备份 → 导镜像 → CRD比对 → create初始配置 → apply装代理 → 权限自检
     → 升级Operator → 升级API → 开接管(EGRESS_ENABLED) → legacy验收
```

**不做的事**：

- 不启用白名单。legacy 模式下不注入代理、不修改现有 NetworkPolicy，所有 Workspace 保持原有网络行为。
- 不重新安装 OSS/Ingress/DNS/Kata/存储，不重跑 [offline_deployment_guide.md](offline_deployment_guide.md) 步骤 1-8。
- 不覆盖原 install.yaml / API 全量 YAML——已有 Operator/API 用 `kubectl set image` 换镜像升级。
- 不点页面上的模式切换按钮。白名单启用（allowlist）是后续单独的变更窗口，见第 8 节。

**前提条件**（步骤 1 体检验证，任一不满足就停）：

| # | 前提 | 验证方式 |
| --- | --- | --- |
| 1 | 集群已运行 Operator/API | 体检脚本列出两个 Deployment 且 READY |
| 2 | 从未装过出站功能 | 无 workspace-egress Deployment；无 workspace-egress-config / workspace-egress-history ConfigMap |
| 3 | 镜像已导入实际调度节点 | 步骤 3 的 ctr 核对 |

新集群如果连 Operator/API 基础平台都没装，先走 offline_deployment_guide.md 主线安装，完成后再回来执行本手册。

**生产基线**：不假定 v1.0.42/v1.0.39。上周五（2026-11-13）刚投产过一次，当天用体检脚本记录**实际镜像 tag 和 revision**——那就是回退基线（回退时按记录选版本，不盲目 rollout undo）。

**离线方式**：无 Harbor。tar 传到节点后 `ctr -n k8s.io images import` 导入；YAML 保持本地镜像 tag（所有 imagePullPolicy 均为 IfNotPresent，kubelet 见本地有镜像就不拉）。

**两集群**：流程完全相同，参数各自收集（附录 A 两张表，**不能互相复制**——DNS、网段、节点都是每集群自己的）。建议先完整走完一个集群并验收再做第二个；人手充足且接受风险时可并行。

## 1. 原理速览（给两周后忘光的我）

### 1.1 白名单怎么生效：两条腿

```
管理员在页面保存白名单 → ConfigMap(workspace-egress-config) 的 policy.json
   │
   ├─ 腿1：Operator 生成 NetworkPolicy（管 IP 直连）
   │        allowlist 下 Workspace 默认拒绝所有出站，
   │        仅放行白名单 IP/CIDR+端口、基础服务、DNS、代理端口 3128
   │
   └─ 腿2：共享 Envoy 正向代理（管域名）
            allowlist 下 Pod 注入 HTTP_PROXY=http://workspace-egress:3128
            HTTP/HTTPS 请求先到代理，域名在名单→转发；不在→403
```

guard（iptables init 容器）锁死**代理自身**的出站：只能用指定 DNS、禁止访问保护 CIDR（集群 Pod/Service/节点/管理网段）。这是独立于业务白名单的物理底线，防"借代理之手摸内网"（SSRF）——即使管理员失手把内网段加进业务白名单，guard 照拦。

### 1.2 四个镜像

| 镜像 | 干什么 | 跑在哪 |
| --- | --- | --- |
| `workspace-operator:egress-20261120` | Operator：监听 Workspace，建 Pod/PVC/NetworkPolicy、注入代理变量、执行模式切换 | agent-deploy-system（升级） |
| `api-server:egress-20261120` | API + 管理页面 + egress-sync 三合一（代理的 init/sidecar 也用它，靠参数区分身份） | default/api-server（升级）＋ 代理 Pod |
| `envoyproxy/envoy:v1.40.2` | 代理本体，监听 3128 | 代理 Pod 主容器（新增） |
| `workspace-egress-guard:egress-20261106` | iptables 保镖 | 代理 Pod init 容器（新增） |

### 1.3 三种模式

| 模式 | 行为 | 本次 |
| --- | --- | --- |
| legacy | 保持原有网络行为：不注入代理、沿用原共享策略及 disabled 语义 | ★ 首次上线用它 |
| allowlist | 所有 Workspace 严格出站：IP 走 NetworkPolicy 白名单，HTTP/HTTPS 走代理域名白名单 | 后续窗口 |
| unrestricted | 全部出站放开（不注入代理），应急用 | — |

两种生效方式：**改 IP/域名规则 = 热更新**（Pod 不重建）；**切模式 = 重建所有 Workspace Pod**（环境变量只有重建才能注入/移除）。

### 1.4 代理 Pod 内部（一个 Pod 四个容器）

```
Pod workspace-egress（2 副本，nodeSelector 指定节点，agent-deploy-system）
├─ init: egress-guard        iptables 锁死本 Pod 出站（读 boundary 参数），失败则整个 Pod 起不来
├─ init: initialize-config   生成 Envoy 启动配置 bootstrap.json（跑完就退出）
├─ envoy                     正向代理本体（UID 101，被 guard 规则约束）
└─ sync (sidecar)            盯白名单 ConfigMap → 热更新路由文件 → 回报实际加载版本
   共享卷 dynamic：init 写 bootstrap.json，sync 写 routes.json，envoy 读
```

### 1.5 关键对象

| 对象 | 位置 | 作用 | 谁创建 |
| --- | --- | --- | --- |
| workspace-egress-config | agent-deploy-system | policy.json：模式 + 白名单 | **我们 create**（步骤 5） |
| workspace-egress-history | agent-deploy-system | `initialized=true` 标记：防配置误删后被当成首次安装而清空白名单 | Operator 首次协调（步骤 10 之后自动出现，正常） |
| workspace-egress-boundary | agent-deploy-system | 代理的 DNS + 保护 CIDR（部署期填好；**改它需重启代理 Pod**） | egress-install（步骤 6） |
| Deployment/Service workspace-egress | agent-deploy-system | 代理本体 + 固定门牌 `workspace-egress:3128` | egress-install（步骤 6） |
| CRD workspaces.ai.example.com | 集群级 | Workspace 类型定义（ai.example.com 是本项目自定义资源的"姓"） | 步骤 4 diff / 按需 apply |

## 2. 交付物清单（每个集群一个发布目录）

| 文件 | 来源 | 用途 |
| --- | --- | --- |
| workspace-operator.tar | 4.1 本机构建 | Operator 镜像（内含 workspace-operator:egress-20261120） |
| api-server.tar | 4.1 本机构建 | API/代理 init/sync 镜像（内含 api-server:egress-20261120） |
| egress-proxy-base-20261120.tar | 4.2 本机导出 | Envoy v1.40.2 + guard egress-20261106 两镜像（沿用已验证版本，包名带新日期仅表示本次投产） |
| egress-legacy-initial.yaml | deploy/production-egress/ | 首次初始化（**kubectl create，绝不能 apply**）。无占位符，两集群通用一份 |
| egress-install.\<集群名\>.yaml | deploy/production-egress/egress-install.production.yaml 填好本集群参数 | **唯一要 apply 的安装文件**（RBAC + boundary + 代理 SA/Deployment/Service） |
| ai.example.com_workspaces.yaml | config/crd/bases/ | 与线上 CRD 比对，有合理差异才 apply |
| egress-production-inspect.sh | deploy/ | 只读体检脚本 |
| 本手册 | docs/egress-production-runbook.md | 执行与排障依据 |
| SHA256SUMS | 发布目录生成 | 传输完整性校验，改任何文件后重新生成 |

⚠️ deploy/production-egress/ 下另有 `egress-proxy.production.yaml` 和 `egress-boundary.production.yaml` 两个旧分文件，内容已全部合并进 egress-install.production.yaml，**本次不使用**（文件头已加 SUPERSEDED 标记，防拿错）。

## 3. 概念速查（看不懂时翻这里）

| 术语 | 一句话 |
| --- | --- |
| Pod | 最小部署单元；一个 Pod 里多个容器共享网络和存储 |
| Deployment | Pod 的控制器：保持副本数、滚动更新、挂了自动拉起。生产从不裸创建 Pod |
| init 容器 | Pod 主容器启动前按序执行、必须全部成功的一次性容器（guard / initialize-config） |
| sidecar | 同 Pod 里辅助主容器常驻干活的容器（sync） |
| ConfigMap | 存配置的对象（键值对），以文件或环境变量进 Pod |
| Service | 一组 Pod 的固定访问入口（DNS 名 + 转发），Pod 重建 IP 变了入口不变 |
| ServiceAccount / Role / RoleBinding | Pod 调 k8s API 的身份和权限。Role=单命名空间门禁，ClusterRole=全集群 |
| CRD | 自定义资源类型定义；装一次，之后 kubectl 就认识 Workspace 这种资源 |
| rollout | Deployment 的发布过程；set image / set env 都会触发一次 rollout |
| ctr -n k8s.io images import | 往 containerd 的 k8s 命名空间导镜像（kubelet 只看这个命名空间；**忘 -n k8s.io 等于没导**） |
| imagePullPolicy: IfNotPresent | 本地已有镜像就不去仓库拉——离线部署能跑的关键 |
| kubectl diff 退出码 1 | 表示"有差异"，不是命令出错；退出码 0 才是无差异 |

## 4. 投产前准备（现在 ～ 11-19，开发机）

### 4.0 提交代码

投产前把当前未提交的修改（YAML、手册、文档）全部提交，保证发布内容与仓库一致、可追溯：

```bash
git status   # 应无未提交内容
```

### 4.1 构建 Operator / API 镜像（PowerShell，目录 D:\E\Code\TS\agent-deploy）

每条成功再执行下一条。注意 Docker 数据盘在 C 盘，先确认剩余空间。

```powershell
Push-Location web\portal
npm ci
npm run build
Pop-Location
docker build --platform linux/amd64 -f Dockerfile -t workspace-operator:egress-20261120 .
docker build --platform linux/amd64 -f Dockerfile.api-server -t api-server:egress-20261120 .
docker save workspace-operator:egress-20261120 -o <发布目录>\workspace-operator.tar
docker save api-server:egress-20261120 -o <发布目录>\api-server.tar
```

验证：`docker image inspect workspace-operator:egress-20261120 api-server:egress-20261120` 两个都能查到。

### 4.2 准备基础镜像 tar

本机已有已验证的两个标签就直接导出；inspect 失败说明本机没有，按命令块补齐后再导出：

```powershell
docker image inspect envoyproxy/envoy:v1.40.2 workspace-egress-guard:egress-20261106
docker save -o <发布目录>\egress-proxy-base-20261120.tar envoyproxy/envoy:v1.40.2 workspace-egress-guard:egress-20261106
```

缺镜像时的准备步骤（guard 需本机 Go 交叉编译，Dockerfile 安装固定 iptables 依赖软件源可达）：

```powershell
docker pull --platform linux/amd64 envoyproxy/envoy@sha256:8de97e654644827d62afdbf7e1d3dc9fee01a8690d410dd1c63a30dc6b6cecbb
docker tag envoyproxy/envoy@sha256:8de97e654644827d62afdbf7e1d3dc9fee01a8690d410dd1c63a30dc6b6cecbb envoyproxy/envoy:v1.40.2
powershell -NoProfile -Command '$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"; & "C:\Program Files\Go\bin\go.exe" build -buildvcs=false -o bin/egress-guard-linux-amd64 ./cmd/egress-guard; exit $LASTEXITCODE'
docker build --platform linux/amd64 -f Dockerfile.egress-guard -t workspace-egress-guard:egress-20261106 .
```

### 4.3 填写每个集群的安装文件

复制 `deploy/production-egress/egress-install.production.yaml` 两份（如 `egress-install.old.yaml` / `egress-install.new.yaml`），每个集群填 3 类占位符：

| 占位符 | 填什么 | 从哪来 |
| --- | --- | --- |
| `__PRODUCTION_DNS_IPV4__` | 该集群 kube-dns 的 ClusterIP | `kubectl -n kube-system get svc kube-dns`（是 IP 地址，不是"coredns"字样） |
| `__PRODUCTION_PROTECTED_CIDRS__` | 该集群 Pod 网段 + Service 网段 + 节点 IP + 管理/VIP 网段，逗号分隔 | 体检脚本的 ippools 输出 + **集群管理员逐段确认**（宁多勿漏，不能按节点 /24 猜总网段） |
| `__PRODUCTION_NODE_NAME__`（nodeSelector） | 代理调度节点名 | 体检脚本节点清单；必须是导入了 3 个 tar 的节点 |

同时核对：

- 4 处 image 保持本地 tag 原样（无 Harbor 不加前缀），与 tar 内标签一致：`workspace-egress-guard:egress-20261106`、两处 `api-server:egress-20261120`、`envoyproxy/envoy:v1.40.2`。
- namespace（agent-deploy-system）与步骤 1 体检记录的实际 namespace 一致；不一致先改 YAML 再往下。
- 不给 envoy 容器加 NET_ADMIN；该权限只属于 guard init 容器（YAML 已配好，别动）。
- 两副本当前 nodeSelector 固定单节点（与测试部署一致）。若现场要求分散到两个节点，删掉 nodeSelector 让调度器分配，但**候选节点必须都导入镜像**。

体检脚本可以提前一天拿到集群上跑（只读），把附录 A 两张表先填了，当天只做核对。

### 4.4 生成 SHA256SUMS

发布目录内（PowerShell），之后**改任何文件都必须重新生成**：

```powershell
Get-ChildItem -File | Where-Object { $_.Name -ne 'SHA256SUMS' } | ForEach-Object {
  $digest = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
  "$digest  $($_.Name)"
} | Set-Content -LiteralPath SHA256SUMS -Encoding ascii
```

## 5. 投产当天（11-20）

以下 11 步，每个集群按顺序执行一遍。**任何一步"怎么算成功"没达到，先按该步"失败怎么查"排查，不要带病往下走。**

排障通用三板斧（后面反复用到）：

```bash
kubectl -n agent-deploy-system get pods -o wide        # ① 看 Pod 状态/所在节点
kubectl -n agent-deploy-system describe pod <pod名>    # ② 看 Events（调度失败、镜像拉取失败都在这）
kubectl -n agent-deploy-system logs <pod名> -c <容器名> # ③ 看容器日志；--previous 看上一次挂掉的
```

### 步骤 1：只读体检

**干什么**：不动集群，摸清现状；填/核附录 A 参数表；记录回退基线（实际镜像 + revision）；验证第 0 节三个前提。

```bash
cd 发布目录
bash egress-production-inspect.sh 2>&1 | tee inspect-$(date +%H%M).txt
kubectl -n agent-deploy-system rollout history deployment/agent-deploy-controller-manager
kubectl -n default rollout history deployment/api-server
```

**怎么算成功**：

- 找到 Operator/API 两个 Deployment（记录：namespace、Deployment 名、容器名、当前镜像 tag、serviceAccountName、所在节点）；
- `workspace-egress-config` / `workspace-egress-history` 输出为空（不存在）；
- kube-dns IP、节点清单、ippools 拿到，附录 A 表填好。

**失败怎么查**：

- 脚本里 NodeLocal DNS / Calico ippools 显示 NotFound 是**正常**（没装而已）；Forbidden 或超时表示"未知"，不等于"不存在"——换有权限的账号重跑。
- 发现已存在 workspace-egress-config 或 history → **不是首次环境，停止**。查 `kubectl -n agent-deploy-system get cm workspace-egress-config -o yaml` 看内容，保住已有规则，改走升级路线（本手册不覆盖该场景）。
- Operator/API 的实际名字与手册默认（agent-deploy-controller-manager / api-server）不符 → 后续所有命令用实际名替换。

### 步骤 2：备份

**干什么**：出事能回去。回退按这里的记录选版本，不盲目 rollback。

```bash
BACKUP_DIR="backup-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$BACKUP_DIR"
kubectl -n agent-deploy-system get deployment agent-deploy-controller-manager workspace-egress --ignore-not-found -o yaml > "$BACKUP_DIR/operator-proxy.yaml"
kubectl -n default get deployment api-server -o yaml > "$BACKUP_DIR/api.yaml"
kubectl -n agent-deploy-system get cm workspace-egress-config workspace-egress-history workspace-egress-boundary --ignore-not-found -o yaml > "$BACKUP_DIR/configmaps.yaml"
kubectl get networkpolicy -A -o yaml > "$BACKUP_DIR/networkpolicies.yaml"
kubectl get workspaces -A -o yaml > "$BACKUP_DIR/workspaces.yaml"
kubectl get crd workspaces.ai.example.com -o yaml > "$BACKUP_DIR/workspace-crd.yaml"
kubectl -n agent-deploy-system rollout history deployment/agent-deploy-controller-manager > "$BACKUP_DIR/operator-revisions.txt"
kubectl -n default rollout history deployment/api-server > "$BACKUP_DIR/api-revisions.txt"
```

**怎么算成功**：每条命令退出码 0，文件非空（`wc -l $BACKUP_DIR/*` 抽查）。networkpolicies.yaml / workspaces.yaml 行数为 0 说明命令失败或集群为空，必须查清。

**失败怎么查**：某条失败通常是资源名不对（用步骤 1 记录的实际名）或权限不足（换管理账号）。**备份不完整不继续后面的步骤。**

### 步骤 3：传输校验 + 镜像导入

**干什么**：让 kubelet 在调度节点上能找到 4 个镜像。

管理机先校验：

```bash
sha256sum -c SHA256SUMS    # 必须全部 OK，否则重传
```

目标节点 = 步骤 1 记录的 Operator 所在节点 + API 副本所在节点（2 副本可能跨 2 节点）+ 代理 nodeSelector 指定节点。把 3 个 tar 传到每个目标节点，逐节点执行：

```bash
ctr -n k8s.io images import workspace-operator.tar
ctr -n k8s.io images import api-server.tar
ctr -n k8s.io images import egress-proxy-base-20261120.tar
ctr -n k8s.io images ls | grep -E "workspace-operator:egress-20261120|api-server:egress-20261120|envoy:v1.40.2|egress-guard:egress-20261106"
```

**怎么算成功**：每个目标节点上最后一条 grep 输出 4 行（4 个镜像都在）。

**失败怎么查**：

- import 报 "already exists" 类提示 → 无害，只要最终 ls 对得上。
- ls 少了镜像 → 检查是否漏了 `-n k8s.io`（导到默认命名空间 kubelet 看不见，重导）；或 tar 传错版本。
- 不确定哪些节点会调度 → 所有工作节点全导一遍，最稳妥。
- `docker load` 对 kubelet 无效，必须 ctr（或 crictl）。

### 步骤 4：CRD 比对（按需升级）

**干什么**：确认线上 Workspace CRD 与当前代码一致。仓库 CRD 已与代码同步（2026-11-05 后类型无改动），但线上是 11-13 投产版本，可能有差异。

```bash
kubectl diff -f ai.example.com_workspaces.yaml
```

**怎么算成功 / 怎么读输出**：

- 无输出（退出码 0）→ 线上一致，**跳过 apply**，本步骤结束。
- 有输出（退出码 1）→ **正常现象不是报错**。逐行看差异：只是**新增**列（printer columns）或可选字段 → 执行 `kubectl apply -f ai.example.com_workspaces.yaml`；出现**删除或修改**已有字段、收紧旧对象校验 → 停下找开发确认，不要 apply。

**失败怎么查**：Forbidden → 用有权限的账号；连接错误 → kubeconfig/context 指错集群（`kubectl config current-context` 核对）。

### 步骤 5：首次初始化（create，不能 apply）

**干什么**：显式建立出厂设置 `mode=legacy`、空白名单。和代码自动首次初始化结果一致，但显式做可控。

先演练再正式：

```bash
kubectl create --dry-run=server -f egress-legacy-initial.yaml   # 演练：应返回 configmap/workspace-egress-config created (server dry run)
kubectl create -f egress-legacy-initial.yaml                    # 正式
```

**怎么算成功**：输出 `configmap/workspace-egress-config created`。

**失败怎么查**：

- `AlreadyExists` → **停止**。说明该集群已有配置（谁配过？）。绝不能改成 apply"强行过"——会覆盖历史白名单。核对内容后走升级路线。
- dry-run 报 schema 错误 → 文件被改动过，与仓库 `deploy/production-egress/egress-legacy-initial.yaml` 比对恢复。

### 步骤 6：安装代理（唯一 apply 的安装文件）

**干什么**：一次 apply 建立全部基础设施：8 个 RBAC（给 API/Operator/代理发权限）+ boundary ConfigMap + 代理的 SA/Deployment/Service。

先演练再正式：

```bash
kubectl apply --dry-run=server -f egress-install.<集群名>.yaml
kubectl apply -f egress-install.<集群名>.yaml
kubectl -n agent-deploy-system rollout status deployment/workspace-egress --timeout=180s
```

**怎么算成功**：rollout 输出 `successfully rolled out`；`kubectl -n agent-deploy-system get pods -o wide` 两个 Pod 均 `2/2 Running`（2/2 = envoy + sync 两个主容器）。

**失败怎么查**（本步骤最容易出问题，按 Pod 状态对号入座）：

```bash
kubectl -n agent-deploy-system get pods -o wide
kubectl -n agent-deploy-system describe pod <pod名>
```

| Pod 状态 | 看哪 | 常见原因 |
| --- | --- | --- |
| Pending | describe 的 Events | nodeSelector 节点名写错 / 节点资源不足 / 节点 NotReady |
| Init:ImagePullBackOff | describe 的 Events | 镜像没导到该节点或 tag 打错 → 回步骤 3 补导 |
| Init:Error / Init:CrashLoopBackOff | `logs <pod> -c egress-guard` | boundary 值没填还是 `__XXX__`、CIDR 格式非法、DNS 填了非 IP——guard 校验 fail closed（宁可不启动也不裸奔）。改好 YAML 重新 apply 后还需 `kubectl -n agent-deploy-system rollout restart deployment/workspace-egress`（boundary 是 init 一次性读取的） |
| envoy CrashLoopBackOff | `logs <pod> -c envoy` | 前面的 init 曾失败过（bootstrap 没生成）；describe 确认 init 都 Completed 再看 |
| 1/2 Running（sync 不就绪） | `logs <pod> -c sync` | sync 连不上 API（RBAC 未生效/卷挂载问题）；再看 Pod 注解 `workspace.bocom.io/egress-status` |
| rollout 超时 | get pods 卡在上述某种状态 | 对号入座处理后重跑 rollout status |

### 步骤 7：权限自检

**干什么**：验证 RBAC 真的绑对了人。apply 成功 ≠ 权限生效（比如 SA 名和实际不一致）。

```bash
kubectl auth can-i create configmaps -n agent-deploy-system --as=system:serviceaccount:agent-deploy-system:agent-deploy-controller-manager
kubectl auth can-i create configmaps -n agent-deploy-system --as=system:serviceaccount:default:api-server-sa
kubectl auth can-i get configmaps/workspace-egress-history -n agent-deploy-system --as=system:serviceaccount:default:api-server-sa
kubectl auth can-i list pods --all-namespaces --as=system:serviceaccount:default:api-server-sa
kubectl auth can-i list deployments.apps --all-namespaces --as=system:serviceaccount:default:api-server-sa
```

**怎么算成功**：5 条全部 `yes`。

**失败怎么查**：有 `no` → 步骤 1 记录的实际 serviceAccountName 与 YAML 里 RBAC subjects 的名字对不上（改 YAML 重新 apply）；管理账号不允许 impersonate → 让集群管理员执行这 5 条，**不能把命令被拒绝当成权限已具备**。

### 步骤 8：升级 Operator

**干什么**：把 Operator 换到新版镜像（此刻 EGRESS_ENABLED 未开，行为与旧版一致）。容器名 manager 以步骤 1 记录为准。

```bash
kubectl -n agent-deploy-system set image deployment/agent-deploy-controller-manager manager=workspace-operator:egress-20261120
kubectl -n agent-deploy-system rollout status deployment/agent-deploy-controller-manager --timeout=180s
kubectl -n agent-deploy-system logs deployment/agent-deploy-controller-manager -c manager --since=5m --tail=100
```

**怎么算成功**：rollout 完成；日志无持续报错刷屏。

**失败怎么查**：rollout 卡住 → 三板斧（get pods / describe / logs --previous）；镜像拉不到 → 步骤 3；新版起不来 → `kubectl -n agent-deploy-system rollout undo deployment/agent-deploy-controller-manager` 回退（回退后记录现象找开发）。

### 步骤 9：升级 API

**干什么**：同上，升级 API（含管理页面和 egress-sync）。

```bash
kubectl -n default set image deployment/api-server api-server=api-server:egress-20261120
kubectl -n default rollout status deployment/api-server --timeout=180s
```

**怎么算成功**：rollout 完成；浏览器打开管理页面正常、能登录、Workspace 列表正常。

**失败怎么查**：同步骤 8；页面 502 → 先看 API Pod 是否 Ready、再查 Ingress 指向。

### 步骤 10：启用接管

**干什么**：让新版 Operator 开始管理出站模式。legacy 模式 = 行为不变，但这是"新逻辑接管"的分界线。

```bash
# 用本集群真实值（与 4.3 填的 boundary 保持一致），此示例值不得直接执行
PROTECTED_CIDRS='<本集群Pod网段,Service网段,节点IP/32,管理网段，逗号分隔>'
kubectl -n agent-deploy-system set env deployment/agent-deploy-controller-manager EGRESS_ENABLED=true EGRESS_CONFIG_NAMESPACE=agent-deploy-system EGRESS_PROTECTED_CIDRS="$PROTECTED_CIDRS"
kubectl -n agent-deploy-system rollout status deployment/agent-deploy-controller-manager --timeout=180s
kubectl -n agent-deploy-system get cm workspace-egress-config -o jsonpath='{.data.policy\.json}{"\n"}'
```

**怎么算成功**：rollout 完成；policy.json 输出 `"mode":"legacy"`；已有 Workspace Pod **没有**被重建（legacy 不触发重建）；Operator 日志无 egress 相关报错。

**预期内的正常现象**：`workspace-egress-history` ConfigMap 会在 Operator 首次协调 Workspace 时自动出现（`initialized=true`），不用管。

**失败怎么查**：

- Operator 日志报 egress 配置错误 → 检查 workspace-egress-config 是否被动过。
- 已有 Workspace Pod 开始重建 → **异常**（legacy 不应触发）：`rollout undo` 回退，保留日志找开发。
- set env 只增改指定变量，原有 NETPOL_ALLOWED_NAMESPACES 等环境变量不受影响，不必担心。

### 步骤 11：legacy 验收

**干什么**：证明"升级 + 接管"之后业务与升级前无差别。

业务验收：

1. 管理页面正常、能登录。
2. 新建一个测试 Workspace → 正常 Running。
3. 挑一个已有 Workspace 休眠再唤醒 → 正常恢复。
4. 在 Workspace 里跑一个真实任务（模型调用等）。

无痕验收（证明 legacy 没注入任何东西）：

```bash
# 期望：无输出（没有 envoy-v1 迁移标签）
kubectl -n bocomwork get pods --show-labels | grep envoy-v1

# 期望：无输出（步骤 2 新建的 Workspace Pod 没有被注入代理变量；用户自配的除外）
kubectl -n bocomwork get pod <新建的Pod名> -o yaml | grep -iE "http_proxy|https_proxy|workspace-egress"
```

策略对比（与备份比，应无本次组件造成的变化）：

```bash
kubectl get networkpolicy -A -o yaml > networkpolicies-after.txt
diff backup-*/networkpolicies.yaml networkpolicies-after.txt
```

**怎么算成功**：以上全部符合预期。两个集群都完成 → 投产结束。白名单启用见第 8 节，**不在今天做**。

**失败怎么查**：Workspace 创建失败 → `kubectl -n bocomwork get events --sort-by=.metadata.creationTimestamp` + Operator 日志；网络策略意外变化 → diff 输出逐条核对，确属本次组件所为则 `rollout undo` Operator 回退。

## 6. 排障速查表（按现象查）

| 现象 | 第一查 | 常见原因 |
| --- | --- | --- |
| ImagePullBackOff / ErrImagePull | describe pod 的 Events | 镜像没导到该节点 / tag 打错 / ctr 没带 `-n k8s.io` |
| Pod 一直 Pending | describe 的 Events | nodeSelector 节点名错 / 资源不足 / 节点 NotReady |
| Init:Error / Init:CrashLoop | `logs -c egress-guard` | boundary 占位符没填 / CIDR 格式非法 / DNS 不是 IP（guard fail closed） |
| envoy CrashLoop | `logs -c envoy` | 前置 init 失败导致 bootstrap 缺失 |
| 代理 Pod 1/2 Ready | `logs -c sync` + Pod 注解 | sync 连不上 API（RBAC / token 卷） |
| rollout status 超时 | `get pods -o wide` 看卡在哪种状态 | 对号入座上行各行 |
| Operator 日志刷错 | `logs deployment/agent-deploy-controller-manager -c manager --since=10m` | 配置 CM 被动过 / 环境变量值非法 |
| 页面打不开 | API Pod Ready？Ingress？ | 步骤 9 回退 |
| create 报 AlreadyExists（步骤 5） | `get cm workspace-egress-config -o yaml` | 非首次环境，停止走升级路线 |
| kubectl diff "失败"（退出码 1） | 看输出内容 | 有差异是正常，不是命令坏 |
| sha256sum -c 不 OK | 重传 | 传输损坏或文件被改过没重算 SHA256SUMS |

代理状态注解（sync 只写注解不打日志，**日志为空不代表同步成功**）：

```bash
kubectl -n agent-deploy-system get pods -l app.kubernetes.io/name=workspace-egress -o go-template='{{range .items}}{{.metadata.name}}{{"\n"}}{{index .metadata.annotations "workspace.bocom.io/egress-status"}}{{"\n\n"}}{{end}}'
```

## 7. 回退（分级，按走到了哪一步）

| 走到哪步 | 想撤什么 | 怎么撤 |
| --- | --- | --- |
| 步骤 5 之前 | 什么都没改 | 直接停，无残留 |
| 步骤 6 后 | 撤代理 | `kubectl delete -f egress-install.<集群名>.yaml`。两个配置 CM 可保留（无害）；要删就 config 和 history **一起删**（只删 config 留 history 会触发"配置丢失"保护性报错） |
| 步骤 8/9 后 | 回旧版应用 | 按步骤 1/2 记录的镜像和 revision：`kubectl set image ... <旧tag>` 或 `rollout undo` |
| 步骤 10 后 | 关接管 | `kubectl set env deployment/agent-deploy-controller-manager EGRESS_ENABLED=false` + rollout。legacy 本无注入，关掉即恢复"不接管"。⚠️ 将来切了 allowlist 后此法**不构成回退**（已注入的环境变量不会自动清），须先在页面切回 legacy 等全部完成 |

本次全程 legacy、无 Workspace 注入，回退窗口宽松；**将来启用 allowlist 后再回退旧版程序，必须先切回 legacy 并等全部 Workspace 完成**（旧程序读不懂含 mode 字段的配置）。

## 8. 后续：启用白名单（另行变更窗口，不在 11-20 做）

前提：legacy 稳定运行一段时间。操作：管理页面把全局模式切到 allowlist（页面会提示**所有 Workspace Pod 将重建、连接中断**，需确认）。验收要点：

- 授权 IP 直连成功（`curl --noproxy '*' ...`）；授权域名经代理成功（`curl --noproxy '' --proxy http://workspace-egress.agent-deploy-system.svc:3128 ...`，在 Workspace 容器内执行）。
- 未授权 IP/域名**新连接**失败；DNS 正常；真实模型 POST/SSE/取消。
- 热更新：增删规则后 Pod UID 不变、新连接按新规则生效。
- 目标业务返回 403/404 ≠ 网络阻断（网络通但业务拒）；Envoy 的 403（响应头 server: envoy）才是代理拒绝。
- 六方向切换（legacy→allowlist→unrestricted→legacy→unrestricted→allowlist→legacy）每步等旧 Pod 消失、目标 Pod Ready、页面待完成清空。

## 附录 A：双集群参数表（投产当天填/核）

### 旧集群

| 项 | 值 |
| --- | --- |
| kubeconfig context | |
| Operator Deployment（ns/name/容器名） | |
| Operator 当前镜像 tag（回退基线）+ revision | |
| API Deployment（ns/name/容器名/副本数） | |
| API 当前镜像 tag（回退基线）+ revision | |
| Workspace 命名空间（默认 bocomwork） | |
| kube-dns ClusterIP → `EGRESS_DNS_SERVERS` | |
| Pod 网段（ippools 全集） | |
| Service 网段 | |
| 节点 IP 清单（/32 逐个） | |
| 管理/VIP/其他保护网段（管理员确认） | |
| → 合成 `EGRESS_PROTECTED_CIDRS` | |
| 代理调度节点名（nodeSelector） | |
| 目标节点镜像导入完成（4 镜像 × 节点数） | ☐ |
| 发布目录路径 / 备份目录名 | |

### 新集群

（同上表，另填一份；**两表任何值不得互相复制**）

## 附录 B：只读命令速查（排障用，都不改集群）

```bash
kubectl -n agent-deploy-system get pods -o wide
kubectl -n agent-deploy-system describe pod <pod名>
kubectl -n agent-deploy-system logs <pod名> -c envoy --tail=50
kubectl -n agent-deploy-system logs <pod名> -c sync --tail=50
kubectl -n agent-deploy-system logs <pod名> -c egress-guard          # init 容器日志
kubectl -n agent-deploy-system logs deployment/agent-deploy-controller-manager -c manager --since=10m --tail=150
kubectl -n agent-deploy-system get cm workspace-egress-config workspace-egress-history workspace-egress-boundary
kubectl -n bocomwork get deployment,pods -o wide
kubectl -n bocomwork get pods --show-labels
kubectl -n bocomwork get networkpolicy
kubectl -n bocomwork get events --sort-by=.metadata.creationTimestamp
kubectl get nodes -o wide
kubectl -n kube-system get svc kube-dns -o wide
```
