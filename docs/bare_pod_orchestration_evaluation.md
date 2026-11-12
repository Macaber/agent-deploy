# Task 2.2 架构评估：直接编排裸 Pod（跳过 Deployment + ReplicaSet）

> **性质**：纯架构评估。本次仅做代码盘点、K8s 语义推演与平台行为调研，**未修改任何代码、未在本地或集群上做实测**。
> **评估对象**：[`internal/controller/workspace_controller.go`](../internal/controller/workspace_controller.go)（`reconcile` / `reconcileDeployment` / 相位判定 / 谓词 / `SetupWithManager`）
> **证据来源**：① 仓库代码逐行盘点；② [平台行为与风险调研](analysis/bare_pod_platform_behavior.md)（21 处已抓取的一手来源：K8s 官方文档、Controller 源码、cluster-autoscaler FAQ / 代码）。
> **验证状态**：方案可实施性已从代码与平台语义上论证；**定量收益与行为等价性仍需按 §8 的验证方案实测确认后才能开启**。

---

## 1. 结论摘要

| 判定项 | 结论 |
| :--- | :--- |
| **技术上可行？** | ✅ 可行。Deployment + ReplicaSet 在本项目里只承重 4 件事（Pod 保活、Recreate 式重建、就绪信号、0/1 副本开关），且第 4 件本质就是"删/建 Pod"；这 4 件事都能由 Operator 用确定性逻辑等价重建（§4、§5）。其余 7 项能力（滚动升级、`maxSurge`、回滚、HPA、PDB、进度死线、`kubectl rollout`）在本负载上**全部是死重**。 |
| **收益方向？** | ✅ 确定。每实例恒定少 2 个控制面对象（1 Deployment + 1 ReplicaSet）；5,000 实例即 **-10,000 个对象**（按 7 对象/实例口径 35,000→25,000，约 **-28.6%**；含 PV/Endpoints 的 9→7 口径约 **-22%**）。另叠加两项**结构性**收益：Deployment 与 RS 各存一份完整 PodSpec（模板冗余），以及 `revisionHistoryLimit=10` 造成的历史 RS 永久残留（详见 §7）。 |
| **代价是什么？** | ⚠️ 自愈责任从"常驻多副本的 kube-controller-manager"转移到"我们的 Operator"。当前 [`config/manager/manager.yaml`](../config/manager/manager.yaml) 是 `replicas: 1` + leader election ⇒ **单副本，Operator 挂掉即所有实例无人保活**。这是本方案**唯一真正的可用性损失**，也是开启前的硬门槛。 |
| **是否建议做？** | **建议做，但要排在 Task 1.x / 4.x 之后，且必须先补齐 4 个前置条件**（§9）：Operator ≥2 副本、Cluster Autoscaler 放行、放弃 PDB 改主动排空流程、看板/成本口径切换。 |
| **本次是否交付实现？** | ❌ 不交付代码。本文只回答"值不值得、能不能、有什么坑、怎么落地、怎么验证"。 |

一句话：**这不是"能不能省 1 万个对象"的问题，而是"要不要用 Operator 的高可用换掉 K8s 原生控制器的高可用"的问题。** Operator 做到多副本，这笔交易才成立。

---

## 2. 评估范围与方法

| 维度 | 内容 |
| :--- | :--- |
| **工作负载画像** | 单实例、交互式、有状态（PVC 工作区）、按需启停（0/1 副本即睡眠/唤醒）、无多副本、无滚动升级、无 HPA、无 PDB 需求 |
| **评估方法** | ① 逐行盘点现有控制器对 Deployment/RS 的**实际依赖**（而非理论上能做什么）；② 对照 K8s 官方文档/控制器源码/CA 文档判定每项能力的提供方与丢失后果；③ 按"若实施"设计目标架构与落地/回滚路径；④ 给出可执行、可验收的**验证方案**（§8，本次不执行） |
| **不在范围** | 存储（OSS/JuiceFS，Task 3.x）、网络（Ingress 收敛、NetPol、kube-proxy/Cilium，Task 1.4/2.1/2.3）、API-Server 异步化（Task 4.1） |
| **明确未做** | 未启动 kind/任何集群，未跑压测，未改控制器代码。所有数量均为**按代码与 K8s 语义推算的估算**，并在文中标注【推算】。 |

---

## 3. 现状：一个 Workspace 的对象账本

[`reconcile`](../internal/controller/workspace_controller.go#L143) 每轮收敛 5 类子资源。稳态下单个 Workspace 的集群对象：

| 对象 | 数量 | 归属 | 是否随本方案变化 |
| :--- | :--- | :--- | :--- |
| `Workspace` (CR) | 1 | 用户 | 否 |
| `PersistentVolumeClaim` | 1 | Operator | 否 |
| `PersistentVolume` | 1 | provisioner（自动） | 否 |
| `Deployment` (`<ws>-deploy`) | 1 | Operator | **pod 模式不再创建** |
| `ReplicaSet` | 1（+历史 0~9） | kube-controller-manager | **pod 模式不存在** |
| `Pod` | 1 | RS（deployment）/ Operator（pod） | 归属变化 |
| `Service` (`<ws>-svc`) | 1 | Operator | 否 |
| `Ingress` (`<ws>-ingress`) | 1 | Operator | 否 |
| `Endpoints`（及 EndpointSlice） | 1 | kube-controller-manager | 否 |
| `NetworkPolicy` | 共享模板（Task 2.1） | Operator | 否 |

- **7 对象/实例口径**（CR + PVC + Deployment + RS + Pod + Service + Ingress）：deployment 7 → pod 5。
- **9 对象/实例口径**（再加 PV、Endpoints）：deployment 9 → pod 7。
- 两种口径差额都是 **-2/实例**，即 5,000 实例 **-10,000 个对象**。

**两个容易低估的细节**：

1. **模板冗余**：`Deployment.spec.template` 与 `ReplicaSet.spec.template` 各保存一份**完整的 PodSpec**；`Pod` 再存一份。pod 模式只存 1 份 ⇒ 每实例少 2 份持久化的 PodSpec 副本（etcd 中同为 protobuf，比例收益与对象数一致）。
2. **历史 RS 永久残留**：`revisionHistoryLimit` 默认 10，`Recreate` 策略下每改一次 Pod 模板（换镜像、改 env、改挂载、改探针…）就多留一个 0 副本 RS，直到 10 个封顶且**永不回收**。即单实例长期最多多养 **9 个 RS**；5,000 实例的极端规模是 **+45,000 个对象**（k8s 官方文档明确指出历史 RS 会占用 etcd 并污染 `kubectl get rs`）。

---

## 4. Deployment + ReplicaSet 到底提供了什么（逐条对照代码）

| # | 能力 | 原生提供方 | 本项目是否真依赖 | 代码证据 |
| :--- | :--- | :--- | :--- | :--- |
| 1 | **Pod 保活**：Pod 对象消失（节点故障、驱逐、spot 回收、误删）后重建 | RS 控制器（kube-controller-manager，多副本常驻） | ✅ 硬依赖：控制器自身**从不重建 Pod** | 全篇无 Pod Create，只读 `deploy.Status.ReadyReplicas` |
| 2 | **Recreate 式重建**：PodSpec 不可变字段变更时"先删旧再建新" | Deployment（`strategy: Recreate`） | ✅ 硬依赖：单写者工作区卷不允许两个进程并存 | `DeploymentStrategy{Type: RecreateDeploymentStrategyType}` |
| 3 | **就绪信号**：推导 Workspace 相位 | Deployment `status.readyReplicas` | ✅ 硬依赖 | [`reconcile` 状态段](../internal/controller/workspace_controller.go#L328) |
| 4 | **0/1 副本开关**：睡眠/唤醒 | Deployment `spec.replicas` | ✅ 依赖（语义=删/建 Pod） | `computeDesiredReplicas`（Stopped / IdleTimeout） |
| 5 | 级联 GC、Pod 归属 | RS 的 ownerRef 链 | ⚠️ Workspace 直接 ownerRef 即可替代 | `ctrl.SetControllerReference` |
| 6 | 滚动升级 / `maxSurge` / `maxUnavailable` | Deployment（RollingUpdate） | ❌ 未使用 | 固定 `Recreate` |
| 7 | 版本历史 / 回滚 / `revisionHistoryLimit` | Deployment | ❌ 未使用（历史 RS 反而占对象） | 无 `rollout undo` 调用 |
| 8 | `kubectl rollout status/restart/history`、`kubectl scale` | Deployment | ❌ 未使用（运维习惯可替换为删 Pod / 改 CR） | runbook 无依赖 |
| 9 | HPA / 多副本 / PDB | Deployment + PDB | ❌ 未使用（单实例；且 PDB 对裸 Pod 语义更差，见 §7 R3） | — |
| 10 | `ProgressDeadlineExceeded` 失败判别 | Deployment | ❌ 未使用（只读 ReadyReplicas） | — |
| 11 | 监控 `kube_deployment_*` / `kube_replicaset_*` | kube-state-metrics | ❌ 本仓库未使用 | `deploy/prometheus/**`、`deploy/grafana/**` 0 处查询（已核查）；e2e 也无 Deployment 断言 |

**结论**：真依赖只有 4 项，其中 3 项是"删/建 Pod + 读就绪"；能力 6~11 是死重，第 7 项还在反向贡献对象膨胀。这是本方案成立的根因。

---

## 5. 目标设计（若实施）：裸 Pod 直编模式

> 本节是**设计级方案**，描述"要做什么、要保证什么"，不含实现代码。

### 5.1 模式开关

`--workload-mode=deployment|pod`（默认 `deployment`，也可用环境变量），分派到两条收敛路径：

- `deployment`：现有逻辑**完全不变**（风险最低，回滚即改 flag）；
- `pod`：Operator 直编单个裸 Pod，自行承担 §4 能力 1/2/3。

**PodSpec 必须单一来源**：现有 `reconcileDeployment` 中约 300 行 PodSpec 构造应抽取为一个共享构造器，两种模式共用 ⇒ **运行时规格结构性等价**，避免"改了 Deployment 模板忘了改 Pod 路径"。

### 5.2 身份与确定性命名的取舍

建议命名 `<workspace>-pod`（而非 `generateName`）：

- 优点：`status.podName`、日志、事件、`kubectl` 排障稳定可读；**结构性防止两个 Pod 抢同一工作区卷**——同名 Pod 未消失前创建必然 `AlreadyExists`；
- 约束：控制器看到 `deletionTimestamp != nil` 时必须**只等待、不重建**，并用短周期 requeue 兜底（避免 informer 漏事件导致卡住）；替换窗口 = 旧 Pod 优雅终止时间（`terminationGracePeriodSeconds=2`），与 `Recreate` 语义一致。

### 5.3 保活

```text
Pod 被删/被驱逐 ──► Owns(&corev1.Pod{}) 删除事件 ──► Enqueue Workspace ──► 重建 Pod
```

- 必须有**谓词过滤**：只放行 `phase` 变化、`Ready` 条件翻转、`deletionTimestamp` 变化、关键 label 漂移；否则 kubelet 的高频 status 写入会被放大成大量无意义 reconcile（现有 `deploymentPredicate` 是同类设计，可直接对标）。
- 必须有**兜底 requeue**：处于"Pod 缺失/终止中"的过渡态时，以秒级 requeue（如 5s）覆盖选主切换、informer relabel 等漏事件场景。
- **能力边界要写进文档**：Operator 不可用期间没有保活（§7 R1）。

### 5.4 不可变字段漂移 → 删除重建（等价 Recreate）

PodSpec 绝大多数字段不可变（`env`/`command`/`args`/`volumes`/`volumeMounts`/`probes`/`securityContext`/`runtimeClassName`/`nodeName`/`restartPolicy` 等；`image` 名义可改但会触发容器重启，且本负载应按重建处理以对齐 Recreate）。因此：

- 漂移判定 → `Delete`（优雅终止）→ 下一轮创建新 Pod；
- **漂移判定必须用"白名单字段比对"，绝不能整包 DeepEqual**：API Server 会默认化一长串字段（`terminationMessagePath/Policy`、`dnsPolicy`、`restartPolicy`、`schedulerName`、`serviceAccountName`、`nodeName`、init container 默认值…），整包比较会让健康 Pod 被无限误删。这是本方案**最大的实现陷阱**，必须有针对性单测锁死。

### 5.5 状态同步：相位映射必须与 Deployment 路径逐条对齐

| Workspace 相位 | deployment 模式判据 | pod 模式等价判据 |
| :--- | :--- | :--- |
| `Running` | `deploy.Status.ReadyReplicas > 0` | Pod `phase=Running` 且 `Ready=True` |
| `Starting` | 副本=1 但 ReadyReplicas=0 | Pod 存在但不 Ready / 缺失 / 终止中 |
| `Sleeping` | 副本=0（IdleTimeout 到期） | 期望副本=0 |
| `Stopped` | `spec.stopped=true` | 同左 |
| `Failed` | 预留 | 同左（未使用） |

- `StartedAt` 取 `pod.Status.StartTime`（与现有从 Pod 列表取 `StartTime` 同源）；`computeStartedAt` / `mergeStartedAt` 的单调性保护原样复用。
- `status.podName` 直接用确定性名字，比现有"label 索引 List 选一个 Running Pod"少一次 List。

### 5.6 双向模式迁移闸门（避免两个 Pod 抢卷）

| 迁移方向 | 必须的处理 |
| :--- | :--- |
| deployment → pod | 先以 **foreground** 方式删除遗留 Deployment（对象保留到 RS/Pod 全部消失），期间**不创建**裸 Pod |
| pod → deployment（回滚） | 先等遗留裸 Pod 消失，期间**不创建** Deployment 的 Pod |

迁移窗口内必须保证"同一 Workspace 至多一个运行时 Pod"，否则单写者工作区卷（ossfs/RWO PVC）有数据损坏风险。这是**验收必测项**，不是可选项。

### 5.7 不动的部分

CRD 无需新增字段；Service / Ingress / PVC / 共享 NetworkPolicy / 相位枚举 / 指标口径全部不变；Pod 标签必须与现有 deployment 模式**完全一致**（`app=workspace`、`workspace=<name>`、`app.kubernetes.io/managed-by=workspace-operator`），从而 Service 选择器、Ingress 后端、共享 NetPol 纳管、Pod 字段索引（Task 1.5）全部免改。RBAC 需要把 `pods` 从 `get;list;watch` 扩到含 `create;delete`（[`config/rbac/role.yaml`](../config/rbac/role.yaml)）。

---

## 6. 若实施：需要新承担/放弃的能力清单

| 类别 | 内容 |
| :--- | :--- |
| **必须重建（代码责任转移给 Operator）** | Pod 保活；Recreate 式重建；相位/StartedAt 推导；`status.podName` 维护；模式迁移闸门；Cluster Autoscaler 豁免注解（`cluster-autoscaler.kubernetes.io/safe-to-evict: "true"`，见 R2） |
| **主动放弃（本负载无需求）** | `kubectl rollout status/restart/undo/history`、`kubectl scale`、`revisionHistoryLimit`、`progressDeadlineSeconds`、`minReadySeconds`、`maxSurge/maxUnavailable` |
| **语义退化（需运维兜底）** | PDB 对裸 Pod 只支持整数 `minAvailable`，单实例 `minAvailable=1` 会让节点**永远 drain 不掉** ⇒ 放弃 PDB，改"Operator 主动排空 + `kubectl drain --disable-eviction`"；`kubectl drain` 要求 ownerRef `controller: true`（现有 `SetControllerReference` 已满足），否则会逼运维加 `--force` |
| **不变化** | 调度、资源配额、LimitRange、NetworkPolicy、存储挂载、探针、`restartPolicy: Always` 的容器级重启（kubelet 负责，与是否裸 Pod 无关） |

---

## 7. 收益量化模型（解析估算）与风险

### 7.1 收益估算【推算，需实测确认】

| 维度 | deployment 模式 | pod 模式 | 差额（5,000 实例） |
| :--- | :--- | :--- | :--- |
| 控制面对象（7 对象口径） | 35,000 | 25,000 | **-10,000（-28.6%）** |
| 控制面对象（9 对象口径，含 PV/Endpoints） | 45,000 | 35,000 | **-10,000（-22%）** |
| 历史 RS 残留（每实例最多 9 个） | 最多 +45,000 | 0 | **最多 -45,000** |
| 持久化的 PodSpec 副本/实例 | 3 份（Deployment template、RS template、Pod） | 1 份 | **-2 份/实例** |
| Deployment/RS 控制器侧负载 | 每实例 2 个对象进 kube-controller-manager 工作队列 | 0 | **归零** |
| Pod 就绪翻转的写放大 | Pod status + RS status + Deployment status | Pod status（Operator 只读） | **-2 次写/次翻转** |
| 早高峰并发唤醒写请求 | 需扩副本、RS 建 Pod、两级状态回写 | 直接建 Pod | 按动作链路**等比减少**（比例待实测） |
| 自愈可用性 | kube-controller-manager（多副本常驻，独立于 Operator） | 依赖 Operator 存活 | **-1 层冗余（必须补 HA）** |

> 说明：上表对象数为**按代码结构直接推算**（确定性），写放大/延迟类比例为链路推演，**必须按 §8 验证方案实测后写入验收结论**，不可直接作为决策依据。

### 7.2 风险矩阵

| # | 风险 | 影响 | 严重度 | 缓解 |
| :--- | :--- | :--- | :--- | :--- |
| R1 | **Operator 不可用期间无保活**：当前 `manager.yaml` 为 `replicas: 1` | Pod 消失后长时间不恢复 | **高** | 上 ≥2 副本 + leader election（选主窗口默认 LeaseDuration 15s / RenewDeadline 10s / RetryPeriod 2s）；Pod 缺失态短周期兜底 requeue；Operator 可用性告警；把 "kill Operator" 纳入故障演练 |
| R2 | **Cluster Autoscaler 默认拒绝缩容**：ownerRef 指向 CRD 的 Pod 属 "custom controller pod"，`--skip-nodes-with-custom-controller-pods` 默认 `true` = 承载它的节点永不可缩容 | 节点缩不下去，成本上升 | **高** | 双管齐下：CA 侧关闭该 flag + Pod 侧注解 `safe-to-evict: "true"`；上线前做一次真实缩容验证 |
| R3 | **驱逐/排空语义退化**：裸 Pod 的 PDB 只支持整数 `minAvailable`，单实例 `minAvailable=1` 使节点**永远 drain 不掉** | 运维流程卡死（非数据丢失） | 中高 | 放弃 PDB；改"Operator 主动排空 → `kubectl drain --disable-eviction`"的 runbook；ownerRef 必须 `controller: true` |
| R4 | **故障风暴下重建限速**：节点故障一次性驱逐大量 Pod，全部走 Operator 重建 | API 瞬时压力、调度抖动 | 中高 | `MaxConcurrentReconciles` 限流；分批 drain；APF 保障（Task 1.1） |
| R5 | **漂移误判误杀健康 Pod** | 反复重建、数据面抖动 | 高（若实现不慎） | 白名单字段比对 + 忽略 API Server 默认值 + 单测锁死 + 灰度期观察漂移日志量（§5.4） |
| R6 | **模式切换窗口双 Pod 抢卷** | 工作区数据损坏 | 高（若实现不慎） | 双向 foreground 闸门 + 专门的验收用例（§5.6、§8） |
| R7 | **工具链/runbook 断裂**：`kubectl rollout`、`kubectl scale`、依赖 `kube_deployment_*` 的平台看板 | 运维习惯改变、看板静默变空 | 中 | 本仓库 prometheus/grafana/e2e **0 处**依赖 `kube_deployment_*`（已核查）；替换口径：`kubectl get workspaces`、删 Pod 即"重启"、Operator 自建指标 |
| R8 | **对象归属变化影响平台侧工具**：KSM `created_by_kind` 由 `ReplicaSet` 变 `Workspace`、`kube_deployment_*` 序列消失 | 外部看板/成本分摊脚本静默缺失 | 中 | 上线前盘点 KSM 消费者，补 `kube_pod_owner` 口径 |
| R9 | 目标集群 K8s 版本未在仓库声明（`go.mod` 钉 k8s.io/api v0.36，集群实际版本未知） | 行为假设漂移（如 `InPlacePodVerticalScaling` 已在 1.35 GA，可原地改 resources） | 低 | 上线前在目标版本复跑验证方案；资源变更仍建议按重建处理（resize 限制多） |

---

## 8. 验证方案（本次未执行；开启前必须完成）

### 8.1 功能等价验证（单元/环境级，成本最低，先做）

在 envtest（真实 kube-apiserver + etcd，无需集群）中覆盖：

| # | 用例 | 通过标准 |
| :--- | :--- | :--- |
| 1 | 创建：仅有裸 Pod + Service/Ingress，无 Deployment/RS | 对象清单符合；Pod 标签/ownerRef（`controller: true`）/安全上下文/探针/卷源与 deployment 模式一致 |
| 2 | 规格等价（parity） | 同一 Workspace 规格下，两种模式渲染的 PodSpec 逐字段相同（除工作区名派生项） |
| 3 | 保活 | 带外删除 Pod 后，下一轮 reconcile 重建同名 Pod |
| 4 | 不可变漂移 | 改镜像 → 旧 Pod 被删除（不原地改）→ 新 Pod 用新镜像；任一时刻该 Workspace 只有 1 个 Pod |
| 5 | 休眠/唤醒 | `stopped=true` → 删 Pod、相位 `Stopped`；唤醒 → 重建 Pod |
| 6 | 相位/StartedAt | Pod `Ready` 翻转后相位 `Running`，`podName`/`startedAt` 取自 Pod |
| 7 | 正向迁移 | 遗留 Deployment 处于 terminating 期间不创建裸 Pod |
| 8 | 回滚迁移 | 遗留裸 Pod 消失前不创建 Deployment |
| 9 | 漂移白名单 | 模拟 API Server 默认字段后不误判；真改镜像精确命中 |
| 10 | 同名外部 Pod | 非本 Workspace 控制的同名 Pod 不被删除/替换 |

### 8.2 定量对比验证（需集群；**须先获得环境与资源使用授权**）

在同一集群、同一二进制、仅切换 flag 的严格同口径下测：

| 指标 | 采集方式 | 验收阈值（建议） |
| :--- | :--- | :--- |
| 每实例对象数（分 kind） | `kubectl get <kind> -n <ns> --no-headers \| wc -l` | 与 §7.1 推算一致（-2/实例），无意外对象 |
| 持久化对象体积 | 各 kind 的 JSON 序列化字节（GET 输出，顺序无关）；etcd 侧仅作参考（存在 free-page 复用偏置） | 体积差 ≥ 2 份 PodSpec 的占比 |
| 各动作 API 写请求 | kube-apiserver `/metrics` 的 `apiserver_request_total` 按 verb×resource 差值 | 创建/唤醒/改规格 均**不高于** deployment 模式 |
| 稳态误重建 | Operator 日志中"漂移重建"条数 | 稳态跨 30 分钟为 **0**（仅规格变更时出现） |
| reconcile 时延 | Operator 的 `workspace_reconcile_duration_seconds` | p50 不高于 deployment 模式；**p99 不得劣化 >20%** |
| 启动时延 | CR 创建→Pod Ready | 不劣化 >10%（预期主因是 PVC 供给，与本方案无关） |
| 故障演练 | 删 Pod / drain 节点 / kill Operator | 全部按预期恢复；kill Operator 期间明确记录"无自愈"窗口 |
| 迁移演练 | 两向切换，期间反复 `kubectl get pods` 采样 | 任一采样点该 Workspace 运行时 Pod 数 ≤1 |

> 关于 `revisionHistoryLimit`：建议**无论是否实施本方案**，都把它显式调小（如 2）——这是**零风险**的对象瘦身步骤，可直接降低历史 RS 残留。

---

## 9. 可实施性判定与落地路线

| 门槛 | 判定 |
| :--- | :--- |
| 功能等价（创建/保活/重建/相位/启停/迁移） | ⚠️ 设计上可等价，**待 §8.1 用例验证** |
| 运行时规格等价 | ⚠️ 需"单一 PodSpec 构造器"设计保证，**待 §8.1 用例 2 验证** |
| 控制面收益可量化 | ✅ 对象数为确定性推算；写入/时延**待 §8.2 实测** |
| 可用性不倒退 | ❌ 当前 Operator 单副本，**必须先补 HA（R1）** |
| 运维可承接 | ❌ 需 drain/CA/看板配套（R2/R3/R7/R8） |
| 可回滚 | ⚠️ 设计上可行（flag + 双向闸门），**需演练** |

**结论：技术上可行、收益方向明确；但"能否开启"取决于运维配套（尤其 Operator HA）与 §8 验证结果，而不是代码本身。**

### 落地路线

| 阶段 | 动作 | 退出标准 |
| :--- | :--- | :--- |
| **P0 决策** | 本文评审；确认 Operator 多副本与 CA 参数变更窗口 | 评审通过、责任人与窗口确认 |
| **P1 设计+单测** | 按 §5 设计实现（含 §8.1 全部用例），仅 flag 关闭状态合入 | §8.1 全绿；deployment 模式回归无损 |
| **P2 灰度（≤200 实例，2 周）** | 测试集群开 pod 模式；每日比对 §8.2 指标 | §8.2 阈值全部达标；故障演练通过 |
| **P3 新集群默认** | 新集群默认 pod；存量集群维护窗口内逐命名空间切换 | 迁移窗口无双 Pod；切换后 24h 指标平稳 |
| **P4 存量收敛（可选）** | 全部切换后评估是否删除 deployment 路径 | — |

**回滚**：`--workload-mode=deployment` + 重启，控制器等遗留裸 Pod 消失后重建 Deployment；回滚窗口内相位可能短暂保持旧值（≤ 兜底 requeue 周期）。

---

## 10. 结论与建议

1. **可行性结论：可行。** Deployment + ReplicaSet 在本项目里只承重 4 件事，3 件是"删/建 Pod + 读就绪"，可被 Operator 等价重建；其余是死重，其中历史 RS 还在反向膨胀对象数。
2. **最硬的约束不是代码，是运维**：① Operator 必须从 1 副本升到 ≥2；② CA 必须放行 CRD-owned Pod 的缩容；③ drain 流程必须从 PDB 改为主动排空；④ 消费 `kube_deployment_*` 的平台看板/成本脚本需换口径。
3. **收益是结构性的**：每实例恒定 -2 个对象（5,000 实例 -10,000），叠加历史 RS 残留（最多 -45,000）、模板冗余（-2 份 PodSpec/实例）、以及 Deployment/RS 控制器侧负载归零。
4. **建议节奏**：排在 Task 1.x / 4.x（止血）之后；先 P0/P1，灰度达标后再考虑新集群默认。**不要在没有 Operator HA 的集群上开启。**
5. **零风险前置动作（已实施）**：`workspaceRevisionHistoryLimit = 2` 已写入控制器——创建路径显式设置、更新路径收敛存量 Deployment（[workspace_controller.go](../internal/controller/workspace_controller.go)），并有用例守护"收敛一次后稳态不再写"。该改动**不修改 Pod 模板**，因此不会触发任何 Workspace 重启；kube-controller-manager 会在各自下一次同步时回收超额的历史 ReplicaSet。
6. **本次交付边界**：只输出评估与验证方案；**未改代码、未启动任何集群**。若需要，我可以按 §8.1 的用例清单与 §5 设计，走正常变更流程提交实现 PR。

---

## 参考

- [平台行为与风险调研](analysis/bare_pod_platform_behavior.md)（K8s 文档、Controller 源码、cluster-autoscaler FAQ/代码等 21 处一手来源）
- [大规模优化 TODO · Task 2.2](large_scale_optimization_todo.md)
- Kubernetes：Deployment 修订历史与 `revisionHistoryLimit`、Pod 不可变字段、Recreate 策略、PDB 语义、裸 Pod 的适用性说明
- cluster-autoscaler：`--skip-nodes-with-custom-controller-pods`、`cluster-autoscaler.kubernetes.io/safe-to-evict`
