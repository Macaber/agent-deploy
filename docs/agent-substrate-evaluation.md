# Agent Substrate / gVisor 捐赠事件评估：agent-deploy 能借鉴什么、能落地什么

> **性质**：纯架构评估文档。**本次不改任何代码**：不新增 CRD 字段、不动控制器与 API-Server、不跑集群实验。
> **评估对象**：2026 年 11 月 KubeCon NA（盐湖城，11/9–11/12）期间 Google 一并捐给 CNCF 的 **Agent Substrate**（v1.0，GKE GA）、**gVisor**、**OpenRL**。
> **仓库基线**：工作区 `HEAD d72aa5b9`（2026-11-06）+ 一处**未提交的既有改动**（`workspaceRevisionHistoryLimit=2`，非本次产出）；文内 `file:line` 以**当前工作区**为准，提交后可能整体偏移约 10 行。
> **证据口径**：外部结论给一手来源 URL 与日期；仓库内结论给 `file:line`；二手报道只做交叉印证，不外推。抓取时间 2026-11-09。
> **未核实项**统一标 **【待验证】**，不参与结论推导。

---

## 0. 一页结论

| 问题 | 结论 |
| :--- | :--- |
| **这套技术是真的吗？** | 真的，且是一手可查的开源项目（Apache-2.0，v1.0 发布于 2026-11-05），不是概念营销。但它解决的是**"大量短命、待机、不可信 agent"**这一类负载，我们的负载画像只与其中一部分重合。 |
| **能整套搬到本项目吗？** | **不能**。它是"runtime + 控制面"替换，而我们是"Pod-per-Workspace + OSS 持久卷"的编排器。整套引入等于重写控制器、API-Server 与网络入口，且与本项目的离线交付（无 OIDC/PostgreSQL/对象存储）前提冲突。 |
| **能借鉴吗？** | **能，而且是四处高价值 + 一处中等价值**：① gVisor 隔离（我们 CRD 里**已有字段**，几乎零改造成本）；② 把"唤醒"从 K8s 调度路径移出去（网关侧请求停车 + 事件驱动恢复）；③ 沙箱内零凭据 + egress 网关注入（我们现在把 Git token 放在 Pod 里）；④ 高频状态不写 etcd（我们 5,000 实例 3.5 万–4.5 万对象的同一病根）；⑤ 若要预热池，**优先复用上游 `kubernetes-sigs/agent-sandbox` 的 `Sandbox`/`SandboxWarmPool`/`SandboxClaim` 原语**，不要自造 CRD（见 §4.7）。 |
| **最该警惕的是什么？** | **"内存快照=亚秒恢复"被过度简化。** 真实实现是 `runsc checkpoint/restore` 原始命令 + 自己写的 worker 内 helper，受 CPU 特性一致性、host 网络 socket 不可保存、CRI 无 restore API、FUSE 语义等约束。我们卷是宿主机内核 FUSE（ossfs），恰好是这条路径上最容易踩雷的一环。 |
| **如果只做三件事** | ① 给"不可信代码类"Workspace 开 `runtimeClassName: gvisor` 并做兼容性验证；② 在统一网关侧做请求停车 + 事件驱动唤醒，把 10–30s 唤醒压到个位数秒（**预热只能是资源级：挂载保持 + 节点亲和，不可能是 Pod 池**，见 §4.6）；③ 把 Git/云凭据从 Pod 内移到出口网关注入。 |
| **现在不要做什么** | 不要为"省唤醒"默认打开 MEMORY 快照；不要把 `Workspace` 语义改造成 `Actor`；不要为了追这条新闻动存储主线（OSS + ossfs 1.0 定版结论不变）。 |

一句话：**Substrate 真正值得我们偷的不是"快照"，而是"它把高频、低延迟的生命周期操作从 K8s 控制面里搬到了一个自己可控的小控制面里"**。我们现在的病是唤醒慢和对象膨胀，这两件事同源。

---

## 1. 事件核验：宣布了什么，流传的说法里哪些要修正

### 1.1 已核实的事实

| 事实 | 证据 | 日期 |
| :--- | :--- | :--- |
| gVisor 项目（含名称与商标）捐给 CNCF，2026-09-07 提交申请、09-28 接受，随后进入 **Sandbox** 状态 | [gvisor.dev/blog/2026/10/02/gvisor-cncf](https://gvisor.dev/blog/2026/10/02/gvisor-cncf/)（gVisor 官方博客） | 2026-10-02 |
| gVisor 治理将从 Google 单方主导转为 maintainers 模型，后续迁 Incubation；Ant Group、Modal、Tines 承诺加入 maintainer，OpenAI/Tencent/NVIDIA 继续贡献 | 同上 | 2026-10-02 |
| Agent Substrate 已是 CNCF 项目页上的 **Sandbox** 级项目 | [cncf.io/projects/agent-substrate](https://www.cncf.io/projects/agent-substrate/) | 抓取 2026-11-09 |
| Agent Substrate v1.0 发布（2026-11-05），仓库采用 Apache-2.0；GKE 上 GA | [github.com/agent-substrate/substrate](https://github.com/agent-substrate/substrate)（README / releases） | 2026-11-05 |
| gVisor 与 Agent Substrate 各自的 CNCF Sandbox 申请单为 `cncf/sandbox#521` 与 `#523` | gVisor 博客正文链接 | 2026-10 |

### 1.2 流传摘要里需要修正的五处

原摘要（含"待机 95%""Request Parking=快照写分级存储""单节点数千 Agent""三层标准栈"等表述）方向大体对，但若直接拿去立项会踩坑：

| # | 流传说法 | 实际情况 | 影响 |
| :--- | :--- | :--- | :--- |
| 1 | "Request Parking = 把运行环境快照写入分级存储" | **错位**。Request Parking 特指 `atenet-router`（Envoy + ext_proc）在 WorkerPool 瞬时饱和时**把 HTTP 请求挂起重试**：默认 `--parked-request-budget=5s`、停车位 `--parked-request-max=1024`，满了直接 503，且同 actor 的并发请求按 flight 去重。见 [request-parking.md](https://github.com/agent-substrate/substrate/blob/main/docs/concepts/request-parking.md)。**快照落盘**是 `SuspendActor` 的职责（`atelet` + objectstore sidecar）。 | 若把两者混为一谈，会误判"我们有 30s 阻塞轮询 ≈ 已有 Request Parking"。两者解决的问题完全不同。 |
| 2 | "500ms 内唤醒恢复" | **是真实厂商口径**（README：resume < 500ms、> 500 activations/s），但**不是设计目标**。架构文档的 north-star 是 **p95 100ms**、单集群 1000 次唤醒/秒、10 亿 actor。 | 引用时应写"厂商口径 500ms / 目标 100ms"，避免被当成实测。 |
| 3 | "单节点承载数千高密度 Agent" | **半对**。解耦后**挂起态** actor 只占快照存储（节点本地 + 对象存储），一个节点上"可以挂起很多"；但**活跃** actor 仍受 worker Pod 的 CPU/内存约束。官方演示是 **~250 actor / 8 个物理 Pod**，密度口径为 10–30 倍超分。 | "数千/节点"对内存快照场景不成立：Sentry 本身有每沙箱固定内存开销（[gVisor 性能指南](https://gvisor.dev/docs/architecture_guide/performance/)）。 |
| 4 | "gVisor 在用户态拦截系统调用并虚拟化内存表" | 机制描述基本正确（Sentry + systrap/ptrace/KVM 平台），但**快照不是靠它自动完成的**：需要 `runsc checkpoint --image-path=...` / `runsc restore`，且 Substrate 明文备注 gVisor 后端**需要一个带 `--allow-connected-on-save` 的 runsc 版本**，绕开 checkpoint 时网络恢复的 bug（[architecture.md](https://github.com/agent-substrate/substrate/blob/main/docs/concepts/architecture.md)）。 | 这句备注比任何宣传材料都更能说明"亚秒恢复"的工程代价：它是在修 bug。 |
| 5 | "三层标准栈：gVisor / Agent Substrate / OpenRL" | 这是**媒体归纳**，不是官方框架。OpenRL 是 Google 自家的 **RL 后训练编排**（Python + K8s、Tinker 兼容 API、2026-02 research preview），与 agent pod 运行时不是同一层，对我们当前的 Workspace 平台**无直接关系**。 | 不要把它写进我们的技术路线图；L3 与我们无关。 |

> 关于日期：v1.0 发布是 **2026-11-05**；捐赠宣布发生在 **KubeCon NA 2026 当周（11/9–11/12）**。流传的"11 月 5 日宣布"更可能把 v1.0 发布日与宣布日合并了。二者对工程判断没有影响。

> 一处诚实的保留：gVisor 自己的治理页（[/community/governance](https://gvisor.dev/community/governance/)）截至抓取时仍描述旧的"Maintainer Council"，博客也说明治理迁移将在"未来几周"完成；CNCF 项目页目前只有 Agent Substrate 一个。这与"刚接受、正在迁移"一致，但"gVisor 已完成 CNCF 托管"这个更强表述【待验证】。

---

## 2. 它到底怎么做到的（机制拆解）

### 2.1 三个解耦

| 解耦 | 做法 | 为什么关键 |
| :--- | :--- | :--- |
| **Actor ↔ Pod** | `Actor`（有状态会话记录）与 `Worker`（预热好的 Pod）解耦；actor 挂起时释放 worker，请求到达时恢复到**任意**空闲 worker | 这是整套东西的地基。它换来的不是"更省内存"，而是**绕开 K8s 调度链路的唤醒**：不新建 Pod、不走 scheduler、不拉镜像 |
| **高频状态 ↔ K8s API** | `WorkerPool`/`SandboxConfig` 是 CRD（低频）；`Actor`/`Worker`/`ActorAssignment` 是 **PostgreSQL** 记录（高频） | 官方明写理由：K8s API Server 不适合存"数百万个离散资源、每秒数千次写" |
| **业务字典 ↔ API 面** | `ActorTemplate`（镜像/配置/环境）是 ate 控制面记录，不是 K8s 对象；首次恢复用模板生成的 **golden snapshot**（version 0） | 首次启动也不需要冷启动，直接恢复"预热快照" |

### 2.2 快照的三档保真度（这是最值得抄的"分层"思想）

`ActorTemplate.snapshotConfig.preferredFidelity` 决定快照抓多深：

```text
VOLUMES  ⊂  ROOTFS  ⊂  MEMORY（默认）
VOLUMES  = DurableDir 卷的内容（等价于"持久卷"）
ROOTFS   = VOLUMES + 各容器可写 overlay 的增量
MEMORY   = ROOTFS + 进程树与内存
```

配套的真实约束（读文档就能看出来的工程债）：

- `MEMORY` 档与 **ActorTemplate UID** 和 **sandboxRuntime 版本**绑定；换了模板或 runtime，恢复只能退到 `ROOTFS` 冷启动。
- 挂起有**同步/异步**两种 durability：默认异步（本地 checkpoint 写完就返回，后台传对象存储，节点挂了会丢状态）；同步模式才保证落对象存储后才返回。
- 恢复优先走**节点本地快照副本**（`locality`），超过 `--snapshot-detach-delay` 才删本地文件；跨节点恢复要下载。
- 快照存储走 `objectstore-plugin`（GCS/S3），`atelet` 自己**不持有对象存储凭据**（凭据在 sidecar，按 sha256 校验后再入缓存）。

### 2.3 组件与调用链

```text
Client ──HTTP(ate-target-actor)──▶ atenet-router (Envoy + ext_proc)
                                      │ ResumeActor(gRPC)
                                      ▼
                                 ate-api-server ──▶ 调度器挑 warm worker
                                      │                    │
                                      │              atelet (DaemonSet, 每节点)
                                      │                    │ download snapshot
                                      │                    ▼
                                      │             ateworker (物理 Pod 内)
                                      │                └─ runsc restore / kata restore
                                      ◀── worker IP ──────┘
                  mTLS CONNECT tunnel ──▶ atunnel:8443 ──▶ actor (私有 veth)
```

要点：**物理 Pod 生命周期与沙箱内进程生命周期分离**（`ateworker` 是 Pod 里的 helper，不是 agent 本身）；路由层负责"人来了就恢复"，而不是靠 API 调用显式唤醒。

### 2.4 安全模型（比快照更值钱的部分）

- 每个 actor 一个 **gVisor 或 microVM 沙箱**；"Plain Linux containers are not supported"（README）。
- 每 actor **短时 SPIFFE X.509**；出口网关可按策略即时签发 per-actor JWT。
- **沙箱内零凭据**：JWT/API key/云 token 都不进沙箱，出口网关（`atenet-egress`）在**沙箱外**注入到已批准的外发请求里；网关是**可替换的 PEP**（可挂 DLP、提示注入检测、云 IAM）。
- 默认拒绝网络：无入向（除 router）、无 actor-to-actor、无控制面访问。

### 2.5 代价（原文"没有免费午餐"这一节的翻译）

| 它换掉了什么 | 代价 |
| :--- | :--- |
| 大量 idle Pod 占资源 | 换成"百万 actor 的快照数据管理"问题：存储量、频繁更新、跨节点搬运 |
| 低延迟 | 必须自建控制面（mTLS + OIDC + OpenFGA 按 atespace 鉴权），并接受"专门控制面 vs K8s 一致性"的集成成本 |
| 可观测性 | actor 与 worker 被复用，必须补 actor 级 / pool 级 OTel 指标，否则排障退化 |
| 密度 | worker 池**常驻成本**：池子不是免费的，warm 池越大越浪费，越小越排队（所以才有 Request Parking） |

---

## 3. 我们的现状画像（对照基线）

### 3.1 一个 Workspace 是什么

`Workspace`（`ai.example.com/v1alpha1`，[workspace_types.go:352](../api/v1alpha1/workspace_types.go#L352)）代表**单用户 agent 运行时**。调和链固定为 5 类子资源：PVC → Deployment（`Recreate`，0/1 副本）→ Service → Ingress → 共享 NetworkPolicy（[workspace_controller.go:296-324](../internal/controller/workspace_controller.go#L296-L324)）。阶段机：`Pending/Starting/Running/Sleeping/Stopped/Failed`（[workspace_types.go:301-312](../api/v1alpha1/workspace_types.go#L301-L312)），**没有"恢复中""停放中"这类中间态**。

### 3.2 状态到底在哪（最关键的一条对照）

| 状态层 | 我们 | Substrate | 差异结论 |
| :--- | :--- | :--- | :--- |
| 文件（代码、会话记录、配置） | **已在 OSS**（PV 子路径 `/workspaces/<ws>`，RWX/Retain，[workspace_controller.go:516-712](../internal/controller/workspace_controller.go#L516-L712)） | VOLUMES 档快照 | **我们这块已经做对了**：文件态天然外置，Pod 死了不丢 |
| 容器 overlay 增量（装过的包、改过的 /usr） | **丢**（Pod 重建即回滚到镜像） | ROOTFS 档 | 我们无从恢复，只能靠镜像/initContainer 重放 |
| 进程与内存（终端、dev server、编译中、in-flight 工具调用） | **丢**（`terminationGracePeriodSeconds: 2`，[workspace_controller.go:1203](../internal/controller/workspace_controller.go#L1203)，2 秒后 SIGKILL） | MEMORY 档 | 这就是"亚秒恢复"真正买到的东西 |
| 期望态/生命周期 | CR（etcd） | WorkerPool CRD + PG | 我们把高频状态也放在 etcd 里了 |

**结论**：我们不是"状态会丢"，而是"**只有文件能回来，进程回不来，所以要等一次冷启动**"。这决定了快照技术对我们的边际收益是"省掉冷启动 + 保住会话"，而不是"防止数据丢失"。

### 3.3 唤醒路径的真实延迟预算

```text
POST /api/workspaces/wakeup
  ├─ 更新 spec.stopped=false / status.lastActiveTime   (main.go:966-1003)
  ├─ 控制器 Requeue/调和 → Deployment replicas 0→1     (controller:1245-1248)
  ├─ ReplicaSet 建 Pod → 调度 → 拉镜像(IfNotPresent) → CSI attach
  │    └─ OSS：控制面创建 FUSE Pod → ossfs 挂载 → NodePublish bind-mount
  ├─ postStartScript + StartupProbe(1s/3s×30) + ReadinessProbe(1s/3s×3)
  └─ API-Server 每 500ms 轮询 + Ingress 探活，上限 30s (main.go:697,704)
```

可观测的"用户体感"上限：**30s**（API-Server 默认；文档写 90s，nginx 按 90s 配）。这与 Substrate 的 100ms/500ms 差 **20–300 倍**，而且它是**同步阻塞**的——早高峰并发唤醒时，API-Server 的 goroutine 与轮询流量会一起堆起来（这正是 `docs/large_scale_optimization_todo.md` Task 4.1 要解决的）。

### 3.4 隔离程度

- 容器以**镜像自带 UID（多数是 root）**运行，`seccompProfile: RuntimeDefault`，掐 11 个危险 capability、留 7 个建卷必需的（[workspace_controller.go:2192-2232](../internal/controller/workspace_controller.go#L2192-L2232)）。
- 网络：命名空间级**共享** NetworkPolicy（禁私网/CTA/link-local 出站），Ingress 入向全开；per-workspace CIDR 例外已被废弃（[workspace_types.go:205-239](../api/v1alpha1/workspace_types.go#L205-L239)）。
- 横向：任意 Workspace 可挂任意已存在的共享 RWX PVC（`spec.sharedVolumeMounts`），这是设计上的跨租户面。
- 凭据：`spec.repository.secretRef` 只是**声明**，代码里没有任何消费方（[workspace_types.go:169](../api/v1alpha1/workspace_types.go#L169)）；实际 Git/云凭据通过 `env`/镜像内配置进 Pod。

**对照**：Substrate 的安全边界是"内核面（gVisor）+ 身份（SPIFFE）+ 凭据（出口网关注入）+ 网络（默认拒绝）"四件套；我们目前只有"内核面的一半（seccomp+caps）"。gVisor 只能补齐第一件。

---

## 4. 逐项可应用性矩阵

评级：★ 越多越值得做；"成本"是**改造与验证**成本，不是采购成本。

| # | 能力 | 评级 | 成本 | 收益 | 主要风险 | 落点（现有扩展面） |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| A | gVisor 作为 RuntimeClass 隔离不可信 agent | ★★★★★ | 低 | 内核面隔离从"容器+seccomp"升到"用户态内核" | 镜像/工作流兼容、I/O 性能、ossfs FUSE 语义 | **字段已存在**：`spec.runtime.runtimeClassName` |
| B | 网关侧请求停车 + 事件驱动恢复 | ★★★★ | 中 | 唤醒 10–30s → 个位数秒；消掉同步阻塞与惊群 | 需要新的路由/网关组件与"人来了"信号 | Task 1.4 统一网关；API-Server 唤醒路径 |
| C | 高频状态移出 etcd（Actor/Worker 式分层） | ★★★★ | 中 | 直击 5,000 实例 3.5–4.5 万对象的病根 | 与"CR 是唯一真源"的既有设计冲突 | `workspace_cache`（已做了一半）、Redis |
| D | 沙箱内零凭据 + 出口网关注入 + per-ws 身份 | ★★★★★ | 低–中 | 提示注入/代码执行拿不到 token；供应链面收窄 | 出口网关成为新单点；需策略模型 | API-Server 注入参数 + 新 egress 网关 |
| E | Actor/WorkerPool 解耦（warm pool 复用 Pod） | ★★★ | 高 | 密度与唤醒双收益，但只对**高频唤醒**负载划算 | 语义重写；与 Pod-per-WS/Ingress-per-WS/OSS-PVC 全冲突 | 新 CRD（概念上对齐 `WorkerPool`） |
| F | MEMORY 档内存快照恢复 | ★★ | 很高 | 会话不中断、dev server 保活 | CPU 特性、网络 socket、CRI 无 restore、快照体积与成本 | 落点存在但要新建：PodSpec 单一构建点可注入 helper sidecar（[controller:1206-1222](../internal/controller/workspace_controller.go#L1206-L1222)），但缺 CRI restore API 与 grace 窗口 |
| G | 快照保真度分层（VOLUMES/ROOTFS） | ★★★ | 中 | 把"容器内装过的东西"也持久化，冷启动少重放 | 与镜像升级/不可变基础设施冲突；ossfs 成本 | 新增 rootfs 持久层（或 PV 子路径约定） |
| H | "预热"而非快照（W1 挂载保持 / 仓库预克隆 / 依赖缓存） | ★★★ | 低–中 | 用资源预热换冷启动时间；镜像已预置，不是变量 | 挂载保持的成本与节点亲和约束（见 §4.6） | 新增"hot set + 节点亲和"策略；`spec.repository`（声明未实现）+ initContainer |
| I | OpenRL（RL 后训练） | ☆ | — | 与 Workspace 平台无关 | — | 不适用 |
| J | 复用上游 `kubernetes-sigs/agent-sandbox` 原语（`Sandbox`/`SandboxWarmPool`/`SandboxClaim` + Sandbox Router） | ★★★★ | 中 | 预热池、稳定身份、无 per-sandbox Service、连接触发恢复，都是**现成的 API** | 与现有 `Workspace` 语义重叠；需要适配层而不是直接替换 | 新 CRD/适配层（见 §4.7） |

### 4.1 A：gVisor —— 唯一"零改代码即可试"的一项

`runtimeClassName` 从 CRD 一路透传到 PodSpec（[types:99-102](../api/v1alpha1/workspace_types.go#L99-L102) → [controller:1200](../internal/controller/workspace_controller.go#L1200) → 差异比对 [controller:1272-1275](../internal/controller/workspace_controller.go#L1272-L1275)），API-Server 也接受该参数。也就是说：**只要集群里注册了 `gvisor` 这个 RuntimeClass（runsc），现有 Workspace 就能按需切换运行时而无需改一行代码**——这在本项目里是罕见的低成本高杠杆点。

但必须一次性想清楚四个坑：

1. **ossfs FUSE 语义**。我们的 `/workspace` 是**宿主机内核 FUSE**（CSI NodePublish 把 ossfs 的宿主机挂载点 bind-mount 进容器）；gVisor 的 FUSE 是另外两种机制：沙箱内 `/dev/fuse`，或 host FUSE server 通过 socketpair + `--pass-fd` 直接说裸 FUSE 协议（[gVisor FUSE 文档](https://gvisor.dev/docs/user_guide/fuse/)）。把"宿主机内核 FUSE bind-mount"放进 gVisor 沙箱，至少要验证：读/写、`mmap`、`rename` 原子性、文件锁、`inotify`（git 状态、编译器文件监视、前端热更新全都依赖它）。**这一项的结论将直接决定 A 能不能上生产。**
2. **root 与建卷动作**。我们的镜像以 root 起、靠 `tar`/`chown` 建卷（[controller:2172-2192](../internal/controller/workspace_controller.go#L2172-L2192)）。gVisor 下 chown 到 20008 之类的操作、以及 OSS FUSE 上 chown 是 no-op 的事实（`docs/workspace-nonroot-migration.md`）叠加后，行为可能与 runc 下不同。
3. **性能画像**。gVisor 的成本是结构性的：**CPU 与内存访问零开销**，**syscall 密集型高开销**，外加**每个沙箱固定的 Sentry 内存**（[性能指南](https://gvisor.dev/docs/architecture_guide/performance/)）。我们的负载是 `npm/pip install` + 编译 + 文件监视：偏 I/O 与 syscall，不是纯 CPU。请按"装依赖 / 全量编译 / 增量编译 / git status / WebIDE 打字回显"五项分别测，不要用一个平均分。
4. **平台与嵌套虚拟化**。默认平台是 `systrap`（`ptrace` 已弃用，[平台指南](https://gvisor.dev/docs/architecture_guide/platforms/)）；若集群本身跑在 VM 里，`KVM` 平台收益会打折。**这决定我们选哪个平台、以及未来快照能力是否可用。**

### 4.2 B：把"唤醒"移出 K8s 调度路径 —— 收益最大、最该做

现状唤醒是**离散的 0→1**：改 CR → 控制器调和 → Deployment 扩 1 → ReplicaSet 建 Pod → 调度 → 拉镜像 → CSI attach（OSS 还要起 FUSE Pod）→ 探针就绪。**每一步都是异步收敛 + 网络跳数**，加起来就是 10–30s。Substrate 的判断非常直接（[architecture.md](https://github.com/agent-substrate/substrate/blob/main/docs/concepts/architecture.md)）：

> 让 Pod 在几秒内起来，对一个跑几小时/几天的工作负载很好；但对"跑毫秒到个位数秒"的工作负载，这个延迟不可接受。

它把这件事拆成两半：**低频的**（Pod/worker 生命周期）交给 K8s；**高频的**（唤醒、恢复、路由）交给自己 100ms 级的控制面。

我们不需要照抄它的 Actor/Worker，也能拿到大半收益：

1. **"人来了"信号**：现在 `lastActiveTime` 只在**创建/API 唤醒**时刷新，`idleTimeout` 不是流量探测（[types:179-183](../api/v1alpha1/workspace_types.go#L179-L183)）。而真实需求是"用户点开链接/建立 WebSocket 的那一刻"。这需要入口层（Ingress/OpenResty/Envoy ext_proc 或 API-Server 反代）把首包/连接事件变成唤醒信号。
2. **请求停车**：请求到达但实例还没起来时，**在网关侧 hold 住并重试**（对应 `--parked-request-budget`/`--parked-request-max`），而不是让 API-Server 用 30s 同步阻塞 + 500ms 轮询去扛。这一条能同时解决 Task 4.1 的异步化与早高峰惊群。
3. **预热池**：即便不做快照，也可以维护一个"空闲但已建好、已挂好卷、已拉好镜像"的 standby 队列，把冷启动里最贵的一段（调度+拉镜像+CSI attach）提前付掉。注意别做过头：standby 也是真金白银。

**注意它与 F 的边界**：Request Parking 只是"请求排队"，它**不产生恢复能力**；没有快照，实例仍然要从头起。所以 B 能把 30s 压到 ~2–5s（去掉调度与拉取），压不到 500ms。把这条写清楚，避免验收时被"为什么没到 500ms"打回。

### 4.3 E/F：Actor-Worker 解耦与内存快照 —— 技术上诱人，工程上最难

先说结论：**这两项在本项目"现阶段"不建议动**，理由不是不先进，而是前置条件一个都不满足：

| 前置条件 | 我们的情况 |
| :--- | :--- |
| 能直接驱动 `runsc checkpoint/restore` | K8s 的 CRI 没有 restore API（`runsc` 的 `create`+`restore` 是原始命令级别；Docker 明确不支持"恢复到新容器"，[checkpoint/restore 文档](https://gvisor.dev/docs/user_guide/checkpoint_restore/)）。要做得像 Substrate 那样，**必须自己在 worker Pod 里放一个 helper 进程驱动 runsc**——这正是它 `ateworker-gvisor` 存在的理由 |
| 节点 CPU 特性一致 | `runsc` 恢复时校验 CPU 特性，跨机型会直接失败；只能靠 `dev.gvisor.internal.cpufeatures` 固定，或建同构节点池 |
| 卷不是"宿主机内核 FUSE" | 见 4.1 第 1 条。MEMORY 档还要把 DurableDir 卷做成 `fs/` 快照 bundle，经 ossfs 读写一遍的代价与一致性都要重算 |
| 网络会话可恢复 | host 网络下 connected socket 恢复即 `ECONNRESET`；我们的入口是 Ingress + WebSocket 长连接，恢复语义要专门设计 |
| 快照存储成本可控 | MEMORY 档 ≈ cgroup 内存量级。按每实例 200MB 估，5,000 个挂起实例 ≈ 1TB 级对象存储 + 上传带宽；异步挂起还有"节点挂掉即丢"的窗口 |
| 能优雅停顿 | 我们现在 `terminationGracePeriodSeconds: 2` 且无 `preStop`（[controller:1203](../internal/controller/workspace_controller.go#L1203)）。要 park，就得先有"停前 checkpoint"的钩子，且 checkpoint 必须在 grace 内完成——大内存直接不成立。 |

还有一条**商业前提**：Substrate 的模型是"用常驻 worker 池的固定成本，换高频唤醒的低延迟"。我们的 `replicas: 0` 已经把空闲成本压到**零**。所以引入它等于**主动增加空闲成本**。只有当"唤醒频率 × 冷启动损失"超过"常驻池成本"时才划算——见 §5。

### 4.4 C：把高频状态搬出 etcd —— 我们已经走在正确方向上

Substrate 用了两个理由拒绝把 Actor 放进 K8s：(a) API Server 不适合数百万离散资源；(b) 每秒数千次写会打爆它。我们有同款病：5,000 实例 7–9 对象（`docs/large_scale_optimization_todo.md:13`）、状态 `Update` 写放大、`Task 1.2/1.3` 就是在止血。

已经做对的部分：API-Server 侧把 Workspace 投影成轻量 DTO、debounce 合并、原子快照（`cmd/api-server/workspace_cache.go`），列表查询零 API-Server 往返（Task 4.2）。

还可以再进一步（都是"思想借用"，不需要引入 Substrate）：把 `status.lastActiveTime`/`podName` 这类**高频、可重建**字段的更新从 CR 里拿出来（走 Redis 或本地缓存），CR 只保留期望态。这与 `Task 2.2`（裸 Pod）和 `Task 4.1`（异步化）是同一方向的三条腿。

### 4.5 D：零凭据 + 出口网关注入 —— 安全收益最高、成本最低

现状：agent 在 Pod 里跑不可信代码（这本来就是 Agent 平台的前提），而 **Git token / 模型 API key / 云凭据都在 Pod 里**（`spec.runtime.env` 直接注入），`spec.repository.secretRef` 甚至没有被实现。一次提示注入 → 代码执行 → 凭据外带，是这类平台的典型事故链。

Substrate 的做法值得逐条搬：**凭据不进沙箱，出口网关注入；网关是可替换的 PEP**。落在我们这里就是：API-Server 不再把 token 放进 `env`，而是把"调用目标 + 身份"交给一个出口代理，由代理在**集群侧**换取/附加凭据。哪怕只做"Git 只读走代理、写操作走审批"，风险面也能收窄一大截。

顺带一个修正项：`spec.repository` 声明了"克隆 Git 仓库"但**没有任何消费方**（[types:169](../api/v1alpha1/workspace_types.go#L169)）。要么实现它（配一个 initContainer 做 clone，正好也是"预热/golden snapshot"的落点 H），要么从 API 里删掉，别让它继续误导调用方。

### 4.6 "预热"在本项目里到底能干什么（按卷绑定关系重新定义）

先纠正本文上一版一处不严谨的措辞：**"standby 队列 / 通用 warm pod 池"在 per-workspace PVC 模型下不成立**。这不是实现细节，而是两条硬约束相乘的结果：

1. **Pod 身份与卷绑定。** 每个 Workspace 自带一条 PVC（`spec.storage` 必填 → `<ws>-pvc`），OSS 主线还是**静态 PV 专用子路径** `/workspaces/<ws>`（[types:148-153](../api/v1alpha1/workspace_types.go#L148-L153)、[controller:516-712](../internal/controller/workspace_controller.go#L516-L712)）。一个"预热好的通用 Pod"在创建时不知道自己该挂谁的卷，而 **Pod 的 volumes 创建后不可变**，所以它无法被"领取"给任意 Workspace。
2. **挂载是节点本地的，而且会随休眠被拆掉。** OSS 走 CSI Attach → 控制面建 FUSE Pod → ossfs 挂载 → NodePublish bind-mount（`docs/deployment_architecture.md:104-112`）；休眠时这条链路被逆序拆除（NodeUnpublish → flush → **删除 FUSE Pod**，`docs/deployment_architecture.md:115`、`docs/oss_storage_architecture.md:131-137`）。

所以能预热的，只有**与身份无关的资源**：

| 编号 | 预热对象 | 为什么它不带身份 | 收益 | 成本 / 风险 | 必须先实测 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **W1** | **存储挂载保持**：hot set 的 FUSE Pod / 内核 mount 不随休眠拆除，唤醒时只做 Pod 创建 + NodePublish bind-mount | mount 是**节点级**的（FUSE Pod 每个卷一个，bind-mount 可复用）；OSS 主线 PV/PVC 是 **RWX**（[controller:634](../internal/controller/workspace_controller.go#L634)、[:675-676](../internal/controller/workspace_controller.go#L675-L676)），允许"占位消费者"与真实 Pod 同时挂载 | 直接砍掉唤醒链路里最贵的一段（建 FUSE Pod + ossfs 冷挂载），并让 stat 缓存留在热态 | 每个保持中的卷 = 一个常驻 ossfs 进程（`max_stat_cache_size=100000` 的内存需实测）；占节点 Pod 配额；占 OSS 连接/QPS；**必须配节点亲和** | FUSE Pod 创建→mount 就绪耗时；ossfs 单实例 RSS；hot set 规模上限 |
| **W2** | **入口预热**：统一网关 + 预置路由 + 请求停车 | 路由不属于任何 Pod | 去掉 per-Workspace Ingress reload（Task 1.4），并把"人来了"变成唤醒信号 | 需要新网关组件；与 nginx 90s 超时的预算要重算 | 首包→实例就绪的端到端时间 |
| **W3** | **控制面/探针预热**：探针节拍收紧、轮询改推送、可选预绑定节点 | 与卷无关 | 砍掉"实例已就绪但外部还不知道"的观测死时间 | 低（参数/协议层为主） | 见下表 t5/t6 |
| ~~W4~~ | ~~通用 warm pod 池 / Actor-Worker 解耦~~ | **不成立**，理由见上面两条约束 | — | — | — |

**W1 必须配"节点亲和"，这正好对应 Substrate 的 `locality`。** Substrate 把最新快照留在节点本地（`--snapshot-detach-delay` 到期才删除），恢复优先在本节点；我们对应的是"挂载留在本节点 + 唤醒时用 `nodeAffinity` 落回该节点 + TTL 到期才拆"。区别在于：它的状态是**快照文件**，我们的状态是**远端 OSS 上的卷 + 节点本地 mount 的缓存**。这也解释了为什么 Substrate 的快照不是"锦上添花"：**worker 池必须无卷，快照才是把"卷的身份"从 Pod 上摘下来的手段**。我们要么改成"对象存储快照 + worker 无卷"，要么就老老实实做 W1/W2/W3。

#### 4.6.1 唤醒耗时分解（镜像已预置，不在表内）

> 前提按你给的事实：镜像在部署前已 load 到每个节点，`ImagePullPolicy: IfNotPresent`，所以**拉镜像不是变量**。去掉这一项之后，关键路径变成下面六段。

| 段 | 内容 | 现状量级 | 测法（不改代码） |
| :--- | :--- | :--- | :--- |
| t1 | API 写 CR（`stopped`/`lastActiveTime`）+ 控制器调谐 | ~0.1–1s | API-Server 日志时间戳 vs Workspace `status` 更新 |
| t2 | Deployment→ReplicaSet→Pod 创建 + 调度 | ~0.5–1.5s | Pod `metadata.creationTimestamp` vs Deployment 变更时间 |
| t3 | **CSI Attach → 建 FUSE Pod → ossfs 挂载 → NodePublish** | **未知，疑为主项** | `ack-csi-fuse` 命名空间里 FUSE Pod 的创建/Ready 时间；`/run/fuse.ossfs` 目录；ossfs 日志 |
| t4 | 容器启动（镜像本地） | ~0.3–1s | Pod `status.startTime` → 容器 `Started` 事件 |
| t5 | 应用 init + startupProbe 节拍 | **≥2–5s**（initialDelay 1s / period 2s / timeout 3s，[controller:2262-2276](../internal/controller/workspace_controller.go#L2262-L2276)） | Pod 事件与 `Running` 时间戳 |
| t6 | API-Server 500ms 轮询 + Ingress 探活（2s 超时） | +0.5–2.5s | 接口返回时间 vs 实际进入 Running 的时间 |

一条一手旁证：代码注释自己写着首次 `/health` 会因为 "the FUSE client warms up" 而慢于 1 秒，所以他们把 `TimeoutSeconds` 调到 3s、`PeriodSeconds` 调到 2s（[controller:2262-2276](../internal/controller/workspace_controller.go#L2262-L2276)）。**这条注释本身就是"挂载冷启动在关键路径上"的证据。**

结论：镜像预置之后，可优化的优先级是 **t3 ≫ t5/t6 > t2 > t1**。W1 攻 t3，W3 攻 t5/t6，W2 让外部请求不再同步等待 t1–t6 全部走完。

### 4.6.2 G/H：慢一点、但确实能拿到的收益

- **H（预热而非快照）**：镜像已预置 → **不作为变量**；真正还能做的是**仓库预克隆**（把 clone 放进 PVC 子路径或 initContainer）、**依赖缓存预热**（`node_modules`/`pip cache` 落 PVC，与 Task 3.2 的本地 cache 同一件事）、以及 W1 的**挂载保持**。这三件事都不动 runtime，**投入产出比远高于 MEMORY 快照**。
- **G（ROOTFS 持久化）**：把"容器 overlay 增量"也持久化，能让"装过的包/改过的环境"跨休眠存活。但它与"镜像不可变 + 可回滚"冲突，且 OSS 小文件写入成本高（Task 3.2 已承认 OSS 元数据瓶颈）。建议只在**按需**开启（例如 `spec` 加一个可选的 rootfs 持久化开关），不要默认。

### 4.7 一个比"抄 Substrate"更现实的落点：上游 `agent-sandbox`

做 B（预热 + 事件驱动恢复）时，最容易犯的错是自造一套 CRD。上游已经有一个**目标负载与我们高度重合**的项目：`kubernetes-sigs/agent-sandbox`（Apache-2.0，**SIG Apps 托管，非 CNCF**，与本次捐赠无关）。

已核实的能力（[README](https://github.com/kubernetes-sigs/agent-sandbox)）：

- 定位就是 "isolated, stateful, **singleton** workloads"，明确点名 AI agent runtime、RL 评测、开发环境、Notebook——**这几乎就是本项目的负载描述**。
- 核心 `Sandbox` CRD：稳定 hostname/网络身份、持久存储、生命周期（创建、定时删除、**pause/resume**）。
- 扩展三件套：`SandboxTemplate`（模板）、`SandboxClaim`（从池中领取）、`SandboxWarmPool`（**预热沙箱池**）。
- 隔离**委托**给 Sandbox Runtime（gVisor / Kata），通过 `RuntimeClass` 生效——与 §4.1 的 A 天然一致，开 gVisor 不需要另做集成。
- 可选 `Sandbox Router`：HTTP 反向代理，把 SDK/外部客户端流量路由到 sandbox Pod；README 明确它是为 gVisor/Kata 这类**无法直接 port-forward** 的运行时准备的。
- "Desired characteristics" 一节直接列出：**deep hibernation**、**automatic resume on network connection**、**no per-sandbox Services**、rich identity。

对我们的含义：

1. **B 的工程形态可以更便宜**：`SandboxWarmPool` + `SandboxClaim` 就是"预热池 + 领取"的标准形状；`Sandbox Router` 就是"人来了"的入口层；两者都不需要我们从零设计。
2. **保留 `Workspace` 作为租户侧 API，内部做 `Workspace → Sandbox` 适配**，可以渐进地拿到 `pause/resume` 语义，同时把 per-workspace 的 Service/Ingress（Task 1.4 的痛点）换掉。
3. **注意它解决的问题边界**：它仍是"一个 Sandbox 一个 Pod"，**不解决密度与快照**；而且 `SandboxWarmPool` 预热的是**与卷无关**的沙箱——我们每 ws 一条专用 PVC 的模型直接套用会退化（见 §4.6 的两条约束）。5,000 实例的 CR+Pod 对象基数依旧存在（它省的是 Service/Ingress 那一层）。所以它能替代的是"编排原语"，不是"运行时解耦"。
4. **版本前提**：要求 k8s ≥ 1.25（推荐 1.29+；用 Gateway API 路由时要 1.31+）。本项目客户端是 1.36 语义，方向兼容；但落地前要确认目标生产集群的实际版本。

> 一句话：**想拿 Substrate 的"预热 + 连接触发恢复"，最省事的不是自研，而是把上游 `Sandbox` 原语接进来，然后保留 Workspace 作为产品面。**

---

## 5. 分品类决策：我们该不该引入 warm pool

同一套基础设施不能服务两种负载画像。用一条粗糙但足够决策的公式：

```text
冷启动型（现状）：日均成本 ≈ 唤醒次数 × (冷启动秒数 × 单价 + 用户等待的业务损失)
warm-pool 型：    日均成本 ≈ 常驻 worker 池核秒 + 快照存储成本 + 复杂度税
```

| 品类 | 唤醒频率 | 建议模型 | 理由 |
| :--- | :--- | :--- | :--- |
| **交互式开发工作区**（WebIDE/终端，本仓库当前的主场景） | 每日 1–3 次 | **保持 scale-to-zero**，只做**资源预热**（W1+W2+W3） | 30s 冷启动一天付 2 次是可接受成本；常驻池一天付 24 次是浪费；而 W1 的常驻成本只按 hot set 计 |
| **无头/自主 coding agent**（Claude Code / Codex / MCP 工具型） | 每小时数十次 | **warm pool + 解耦**才划算，**前提是状态改成对象存储快照、worker 无卷**（否则 per-ws PVC 会把池子堵死，见 §4.6） | 每次唤醒都付 10–30s 是灾难；这正是 Substrate 的目标负载，也是它必须有 VOLUMES/MEMORY 快照的原因 |
| **批量 RL rollout / 评测** | 秒级、海量并行 | 看 OpenRL/agent-sandbox 生态，不在本平台 | 与"每人一个长期工作区"模型正交 |

结论：**不要为了现有交互式工作区引入 warm pool**；若要试，**新开一个"无头 agent"品类试点**，与现有 `Workspace` 语义并存，避免把两类负载拧在同一个 CR 上。

---

## 6. 三条路线

### 路线 A（2–3 周，评估级）：只做隔离层
1. 在测试集群注册 `gvisor` RuntimeClass（runsc + systrap）。
2. 用现有 Workspace 通过 API-Server 传 `runtimeClassName` 起 3 类样本：Node/前端类、Python/数据类、带 WebIDE 终端的交互类。
3. 按 §7 的验证清单打勾，产出"哪类镜像可上、性能损失区间、ossfs 是否可用"的结论。
4. **不改代码**；结论决定 A 是否升为 P0。

### 路线 B（6–8 周，工程级）：隔离 + 加速
1. 网关侧统一入口（承接 `Task 1.4`），把"首包/WS 建连"变成唤醒信号 + 请求停车重试；优先评估复用 `agent-sandbox` 的 `Sandbox Router`/`SandboxWarmPool`（§4.7），而不是自造 CRD。
2. **资源预热（不是 Pod 池）**：W1 hot set 挂载保持 + 节点亲和；W3 探针节拍与"轮询改推送"；仓库预克隆（实现 `spec.repository`）+ 依赖缓存落 PVC。**镜像不作为变量**（已预置到节点）。
3. 凭据外置（D 的最小可用版：Git 与模型 key 走出口代理）。
4. **不要**引入快照；验收口径写"唤醒 P99 ≤ 5s"，不要写 500ms。

### 路线 C（季度级，战略级）：把运行时层换成 Substrate 式
前置条件（缺一不可）：离线环境能提供 OIDC/PostgreSQL/对象存储（当前交付是离线包，无这些）；镜像全部能跑在 gVisor 下；放弃 per-workspace Ingress/Service/PVC 语义；控制面与 API-Server 大半重写。
**建议**：只在"无头 agent"品类用独立命名空间试点，**不要动现有 Workspace 主线**。

---

## 7. 必须先验证的风险清单（不改代码即可执行）

| # | 验证项 | 判定标准 | 不通过的后果 |
| :--- | :--- | :--- | :--- |
| 1 | gVisor 下 OSS 卷读写 | 1GB 随机小文件写、`git status`、全量编译、`inotify` 事件数正常 | A 直接否掉 |
| 2 | gVisor 下 root/建卷 | `tar`+`chown` 到 20008 成功且耗时可比 | 需改镜像或改非 root 迁移顺序 |
| 3 | 装依赖场景 | `npm ci` / `pip install` 相对 runc 的耗时倍数 | 决定是否只对部分品类开 gVisor |
| 4 | CPU/内存开销 | 纯 CPU 任务无损失；单沙箱固定内存增量可接受（影响同节点密度） | 密度下降压缩成本模型 |
| 5 | WebSocket/长连接 | 终端打字回显、大文件传输正常 | 交互类不能开 gVisor |
| 6 | `runsc checkpoint` 可行性 | 在 systrap 平台下能 checkpoint/restore 一个真实工作区容器 | F 永久搁置 |
| 7 | CPU 特性一致性 | 跨机型 restore 是否失败；固定 `cpufeatures` 是否可行 | 需建同构节点池 |
| 8 | 快照体积与成本 | 单个真实工作区 MEMORY 快照大小、上传耗时 | 决定 F 是否具备经济性 |
| 9 | 2s grace 能否承载 checkpoint | 大内存容器 checkpoint 耗时 vs `terminationGracePeriodSeconds` | 需改优雅停机语义 |
| 10 | 唤醒信号可得性 | Ingress/网关能否在首包处触发唤醒（含 WS 升级） | B 退化为"API 显式唤醒" |
| 11 | 停车重试的边界 | 网关 hold 上限、与 nginx 90s 超时的叠加 | 需重新定义超时预算 |
| 12 | 凭据外置可行性 | Git 只读/写在代理侧完成鉴权，Pod 内无 token | D 只能做一半 |
| 13 | **FUSE Pod 能否"保持"** | 卷不再被 Workspace Pod 消费时，CSI 控制面是否保留 FUSE Pod；若默认拆除，则需"占位消费者 Pod" | W1 的实现形态要改写（占位 Pod vs 驱动参数） |
| 14 | **hot set 常驻成本** | 单个 ossfs 实例 RSS（`max_stat_cache_size=100000`）、hot set=100 / 500 时的节点内存与 OSS 连接/QPS | 决定 hot set 上限（可能只有几十到低百级） |
| 15 | **节点亲和可行性** | 唤醒时用 `nodeAffinity` 落回挂载所在节点，是否与调度/自动扩缩容冲突、是否需预留资源 | W1 收益会被"落不回原节点"吃掉 |

> 统一提醒：gVisor/Substrate 的性能数字都是**厂商口径**（README 的 <500ms、10x density），不是第三方基准；我们的验收必须用自己的镜像、自己的 OSS、自己的节点机型复测。

---

## 8. 建议：三件该做的事 + 三件不该做的事

**该做（按顺序）**

1. **把 gVisor 当成"已有开关"来验证**（P0，成本最低、杠杆最高）：`runtimeClassName` 字段已经在，验证清单跑通即可按品类灰度；不可信代码类 Workspace 优先。
2. **把"唤醒"从 K8s 调度路径里搬出去**（P1，收益最大）：统一网关 + 请求停车 + 事件驱动恢复，优先复用上游 `Sandbox`/`SandboxWarmPool`/`Sandbox Router` 原语（§4.7）。验收写 P99 个位数秒，不写 500ms。
3. **把凭据从 Pod 里拿出去**（P1，安全收益最高）：Git/模型凭据走出口代理；顺手把未实现的 `spec.repository` 要么实现（预热式克隆）要么删除。

**不该做（现阶段）**

1. 不为"省唤醒"默认打开 MEMORY 快照——前置条件（CRI restore、CPU 一致性、FUSE 语义、grace 窗口、成本模型）一个都不成立。
2. 不把 `Workspace` 改造成 `Actor`，不动 OSS + ossfs 1.0 存储主线（定版结论不变）。
3. 不把 OpenRL 写进技术路线图：它是 RL 后训练编排，与本平台当前目标正交。

**一句话给决策层**：这条新闻对我们**不是"要不要换运行时"，而是"要不要承认我们把高频生命周期操作塞进了 K8s 控制面，并且现在有了一个被验证过的替代做法"**。先借它的隔离层和请求面，快照层等我们把四个前置条件凑齐再说。

---

## 9. 参考来源

**一手（外部）**
- gVisor 官方博客《gVisor is being donated to CNCF》，2026-10-02 — https://gvisor.dev/blog/2026/10/02/gvisor-cncf/
- Agent Substrate 仓库（README / docs/concepts/architecture.md / docs/concepts/request-parking.md），Apache-2.0，v1.0 — https://github.com/agent-substrate/substrate
- CNCF 项目页 Agent Substrate（Sandbox） — https://www.cncf.io/projects/agent-substrate/
- gVisor 文档：Checkpoint/Restore — https://gvisor.dev/docs/user_guide/checkpoint_restore/
- gVisor 文档：Performance Guide — https://gvisor.dev/docs/architecture_guide/performance/
- gVisor 文档：Platform Guide（systrap/ptrace/KVM） — https://gvisor.dev/docs/architecture_guide/platforms/
- gVisor 文档：FUSE（沙箱内 / 外部 FUSE server） — https://gvisor.dev/docs/user_guide/fuse/
- `kubernetes-sigs/agent-sandbox` 仓库 README（`Sandbox`/`SandboxWarmPool`/`SandboxClaim`/Sandbox Router，Apache-2.0，SIG Apps） — https://github.com/kubernetes-sigs/agent-sandbox

**二手（仅交叉印证，不外推）**
- Google Cloud 博客《Open infrastructure for autonomous agents comes to CNCF》（含 OpenRL 描述：Python/K8s 的 RL 后训练编排、Tinker 兼容 API、2026-02 research preview） — https://cloud.google.com/blog/products/containers-kubernetes/open-infrastructure-for-autonomous-agents-comes-to-cncf
- Google Open Source 博客《Sandboxing Untrusted AI Agents at Scale》，2026-11 — https://opensource.googleblog.com/2026/11/sandboxing-untrusted-ai-agents-at-scale.html
- KubeCon + CloudNativeCon North America 2026（11/9–11/12，盐湖城） — https://events.linuxfoundation.org/kubecon-cloudnativecon-north-america/

**仓库内**
- [`api/v1alpha1/workspace_types.go`](../api/v1alpha1/workspace_types.go) — CRD 字段、阶段机、`runtimeClassName`、`idleTimeout` 语义
- [`internal/controller/workspace_controller.go`](../internal/controller/workspace_controller.go) — 调和链、PodSpec 构建、副本计算、安全上下文
- [`cmd/api-server/main.go`](../cmd/api-server/main.go) — 创建/唤醒/停止、500ms 轮询与 30s 上限
- [`docs/large_scale_optimization_todo.md`](large_scale_optimization_todo.md) — 对象膨胀、网关收敛、异步化、存储瓶颈
- [`docs/bare_pod_orchestration_evaluation.md`](bare_pod_orchestration_evaluation.md) — Deployment/RS 死重与本方案的可用性代价
- [`docs/storage_plan.md`](storage_plan.md) — OSS + ossfs 1.0 定版结论
- [`docs/workspace-nonroot-migration.md`](workspace-nonroot-migration.md) — 非 root 迁移与 OSS FUSE 的 chown 限制
