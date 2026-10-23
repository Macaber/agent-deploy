# Workspace Operator 可观测性看板（Task 5.2）

## 内容

- `dashboards/workspace-operator.json`：Grafana 看板 **「Workspace Operator - 规模与调谐水位」**。

> **接入 Prometheus 的完整操作步骤**（ServiceMonitor / 原生 scrape_configs /
> 集群外 Prometheus、验证与排错）见 **`../prometheus/README.md`** 及同目录的
> `servicemonitor.yaml`、`prometheus-scrape.yaml`、`recording-rules.yaml`。
> 下面这节只保留最简抓取要点。

## 抓取配置

Manager 以 `--metrics-bind-address=:8080 --metrics-secure=false` 启动，`/metrics`
是**明文 HTTP 且不校验 Bearer Token**（集群为可信内部网络）。Service 也已随
`deploy/install.yaml` 部署：

```
agent-deploy-controller-manager-metrics-service.agent-deploy-system:8080  (端口名 http)
```

Manager 容器在 `config/manager/manager.yaml` 中显式声明了两个端口：
`8080 (http)`（metrics）与 `8081 (health)`（健康探针）。`role: pod` 会为
**每个声明的容器端口**各生成一个 target，因此必须按端口名 keep `http`，
否则会出现一个打到 8081、请求 `/metrics` 返回 404 的坏 target。

Prometheus 侧只需一条最简抓取规则，不需要 `bearer_token_file`，也不需要
`tls_config`：

```yaml
- job_name: workspace-operator
  scheme: http
  kubernetes_sd_configs:
    - role: pod
      namespaces: {names: [agent-deploy-system]}
  relabel_configs:
    - source_labels: [__meta_kubernetes_pod_label_control_plane]
      regex: controller-manager
      action: keep
    - source_labels: [__meta_kubernetes_pod_container_port_name]
      regex: http
      action: keep
```

> **抓取前提**：`agent-deploy-system` 命名空间**不安装任何 NetworkPolicy**（部署约定），
> manager 的 8080 入站默认放行——NetworkPolicy 是叠加白名单，没有策略选中 Pod
> 即全放行。`config/network-policy` 组件因此保持不交付（`#- ../network-policy`
> 注释，见该文件注释）。若将来该前提被打破，需启用该组件、给监控命名空间打
> `metrics: enabled` 标签并重建 `deploy/install.yaml`，否则 8080 抓取会被策略拦下。

> 若将来恢复安全模式（去掉 `--metrics-secure=false`，二进制默认即为 true），
> 则抓取侧必须补上 `scheme: https`、`tls_config.insecure_skip_verify: true`
> 与 `bearer_token_file`，并给 Prometheus 的 ServiceAccount 绑定
> `agent-deploy-metrics-reader`（ClusterRole 已随安装包提供）。
>
> `config/prometheus/monitor.yaml` 里的 ServiceMonitor **没有**随
> `deploy/install.yaml` 一起发布（`config/default/kustomization.yaml` 中
> `#- ../prometheus` 保持注释），因此安装包不会因为交付环境缺少
> prometheus-operator CRD 而 apply 失败。

## 指标清单

以下指标由 `internal/controller/metrics.go` 定义，随 Manager 的 `/metrics` 端点暴露。

| 指标 | 类型 | 含义 |
| --- | --- | --- |
| `workspace_reconcile_duration_seconds` | Histogram | 单次 Workspace 调谐耗时（含 PVC / Deployment / Service / Ingress / NetworkPolicy 全流程与状态回写）。NotFound 的删除事件不计入 |
| `workspace_active_instances` | Gauge（`phase` 标签） | 按 `status.phase` 统计的在线实例数，phase 取值：Pending / Starting / Running / Sleeping / Stopped / Unknown（`Failed` 永不赋值，故不发布该序列） |
| `workspace_total_instances` | Gauge | **工作空间总数**（所有 Phase 之和）。恒等于 `sum(workspace_active_instances)`，免去聚合与 `phase` 选择器拼写错误 |
| `workspace_running_instances` | Gauge | **当前处于 Running 的个数**。恒等于 `workspace_active_instances{phase="Running"}` |
| `workspace_running_peak_instances` | Gauge | **同时 Running 的峰值**（高水位，单调递增）。记录每一次相位变化的真实峰值，而不是抓取采样点，因此不会漏掉两个采样点之间的短峰；**进程重启或主备切换后归零**，跨窗口历史峰值请改用 `max_over_time(workspace_running_instances[$__range])` |
| `workspace_running_peak_timestamp_seconds` | Gauge | 当前峰值**第一次出现**的 Unix 时间戳（**单位：秒**，符合 Prometheus 约定）。`max_over_time()` 只给数值不给时间，需要"什么时候达到峰值"时用这个序列。**在 Grafana 里用 `dateTimeAsIso` 单位时必须 `×1000`**——该单位按毫秒解析，直接写会显示成 1970 年 |

> 后四个无标签 Gauge 与 `workspace_active_instances` 走**完全相同的代码路径**
> （`workspacePhaseTracker`）更新，因此两者永不漂移；同时它们也不受
> `--active-instances-sync-interval` 全量 resync 影响——resync 只校正当前值，
> **不会**把已经观测到的峰值抹掉。

辅助使用 controller-runtime 内置指标。注意这些指标的**实际名称与标签并不统一**，
不要给它们加 `controller_runtime_` 前缀：

| 内置指标 | 标签 | 说明 |
| --- | --- | --- |
| **`controller_runtime_reconcile_errors_total`** | `controller` | **调谐错误数**（controller 名固定为 `workspace`）。看板/告警用它代替已删除的 `workspace_reconcile_errors_total` |
| `workqueue_depth` | `name`, `controller`, `priority` | 队列深度。priority queue 下带 `priority` 标签，看板用 `sum(...)` 聚合 |
| `workqueue_adds_total` | `name`, `controller` | 累计入队次数，看板用 `rate(...)` |
| **`workqueue_queue_duration_seconds`** | `name`, `controller` | **排队延迟**（进入 ready → 被 worker 取走，不含 rate-limited 重试）。controller 名固定为 `workspace`；看板/告警用它代替已删除的 `workspace_queue_latency_seconds` |
| `workqueue_longest_running_processor_seconds` | `name`, `controller` | 当前最长执行时长 |
| `controller_runtime_max_concurrent_reconciles` | **`controller`**（无 `name`） | worker 并发上限 |

## 看板结构

看板分两行，共 13 个面板。顶部有**「集群」变量**
（`label_values(workspace_total_instances, cluster)`，支持单选 / 多选 / All）：

- 选 **All**：每套集群各一条线、各一个数值，互不干扰；
- 选 **某一套**：只看该集群，数字与门户页面对应；
- 未打 `cluster` 标签的单集群部署仍然兼容（`cluster=~".*"` 匹配空标签）。

所有查询都按 `cluster` 分组（`by (cluster)`），图例形如 `prod · Running`、`prod1 · P95`。

| 行 | 面板 | 说明 |
| --- | --- | --- |
| **工作空间规模** | 工作空间总数 | `max(workspace_total_instances)`，全部 Phase 之和，**全集群所有命名空间**（门户页面默认只看 `bocomwork`，两者数字通常不相等，见下） |
| | Running 实例数 | `max(workspace_running_instances)`，全集群所有命名空间，当前实时水位 |
| | Running 峰值（同时运行数） | `max(workspace_running_peak_instances)`，进程内高水位 |
| | 峰值出现时间 | `max(workspace_running_peak_timestamp_seconds) * 1000`，按 `dateTimeAsIso` 渲染（**必须 ×1000**：指标是秒，Grafana 该单位按毫秒解析；漏乘会显示成 1970 年） |
| | **Running 工作空间数量随时间变化** | **折线图**：Running（绿实线）+ 进程内峰值（橙虚线）+ 总数（蓝线）。图例 `Max` 列即该时间窗口的峰值 |
| | 各 Phase 在线实例水位 | 按 `phase` 堆叠，看爬坡与休眠回收 |
| **调谐引擎** | 调谐吞吐量 / 调谐错误 (5m) | 顶部两个 stat |
| | 调谐耗时、排队延迟、队列深度、速率与错误率、最长执行与并发 | 引擎容量与健康度 |

> **口径提醒：看板是「全集群」，门户是「单命名空间」**
>
> 所有规模指标都由 manager 的**全集群** `List` 得出（`syncActiveInstances` 没有加
> `InNamespace`），而门户/API 页面默认只查 `bocomwork` 一个命名空间
> （`web/portal/src/store.ts` 里 `namespace: 'bocomwork'`，请求
> `GET /api/workspaces?namespace=bocomwork`）。
>
> 所以「看板总数 41 / 页面总数 35」这种差异**通常不是算错**，而是另外 6 个
> 分布在同一集群的其它命名空间（测试、default 等）。核对方法：
>
> ```bash
> kubectl get workspaces.ai.example.com -A --no-headers | wc -l        # 对比看板
> kubectl get workspaces.ai.example.com -n bocomwork --no-headers | wc -l  # 对比页面
> ```
>
> 指标目前**没有 `namespace` 标签**，因此看板无法按命名空间拆分或过滤，
> 也就无法与门户数字直接对齐。需要按命名空间看，就得给指标加维度（见下）。

> **峰值看两个口径**：
> - 面板「Running 峰值（同时运行数）」是**进程内真实峰值**，来自控制器每次相位变化的观测，
>   比抓取采样更准（30s 抓取 + 1m 内的短峰容易漏采），但**重启后归零**；
> - 折线图图例的 `Max` 列是**所选时间窗口内采样点的最大值**（`max_over_time` 口径），
>   跨重启仍然成立，适合复盘"昨天晚上高峰跑到多少"。
> 需要长期留存峰值时，用 Recording Rule 固化 `max_over_time`，或直接看进程内峰值并接受重启归零。

> **为什么「峰值出现时间」会早于面板的数据起点？**
>
> 这是预期行为，不是数据错乱。`workspace_running_peak_timestamp_seconds` 是 manager
> 进程的**内存状态**，含义是「本进程启动以来第一次达到峰值的时刻」，
> 与你的 Prometheus 什么时候开始抓取无关：
>
> ```
> 10-22 manager 启动 ──► 观测到峰值 12，把 (12, 10-22 17:06) 记在内存
> 10-23 15:00 接入抓取 ──► Prometheus 首次抓取，直接读到已有的 (12, 10-22 17:06)
> ```
>
> 验证：`kubectl -n agent-deploy-system get pod -l control-plane=controller-manager -o jsonpath='{.items[*].status.startTime}'`
> 输出的时间早于你接入抓取的时间，就说明值来自进程内存。
>
> 两个推论：
> 1. **manager 重启后峰值与时间戳都会重置**（滚动升级也算），重新从「当前 Running 数」起算；
> 2. **删掉/缩容工作空间不会让峰值下降**——它是高水位，只升不降；`workspace_total_instances`
>    和 `workspace_running_instances` 会在一分钟内降下来（全量 resync 兜底），但峰值要等重启。
>
> 想要「从接入那一刻算起」的峰值，用 `max_over_time(workspace_running_instances[$__range])`，
> 也就是折线图图例的 `Max` 列。

## 导入看板

Grafana → Dashboards → New → Import → 上传 `dashboards/workspace-operator.json`，
选择 Prometheus 数据源即可（uid 固定为 `workspace-operator`）。

## 引擎类面板怎么读

| 面板 | 回答什么问题 | 数据来源 | 异常信号 |
| --- | --- | --- | --- |
| **调谐耗时 P50/P95/P99** | 一次调谐端到端要多久 | `workspace_reconcile_duration_seconds`（含创建整个子资源栈，**不含**排队等待） | P99 持续 > 1s 关注、> 5s 告警；P99 与吞吐同时上升＝容量不足 |
| **队列深度与入队速率** | 有多少活压着、活从哪来 | `sum(workqueue_depth{name="workspace"})`、`rate(workqueue_adds_total{name="workspace"})` | 深度在 0 附近抖动正常；**持续爬升**才是积压 |
| **调谐速率与错误率** | 每秒消化多少、失败多少 | `rate(workspace_reconcile_duration_seconds_count)`、`rate(controller_runtime_reconcile_errors_total{controller="workspace"})` | 两条线应基本重合；错误率抬升但吞吐不降＝限流重试在兜底；同时恶化＝下游故障 |
| **最长调谐执行与 Worker 并发** | 有没有卡住的调谐、并发打满没有 | `workqueue_longest_running_processor_seconds`、`controller_runtime_max_concurrent_reconciles`（默认 30） | 该值持续贴近或超过 P99 且不回落＝有调谐卡死 |

**串起来看（容量三角）**：

```
需要的 worker 数 ≈ 吞吐(次/秒) × 平均耗时(秒)        # Little's law
```

例：50 次/秒 × 0.2 秒 = 10 个 worker，上限 30 时余量充足。
若「队列深度」持续上涨 **且**「排队延迟 P95」上涨 → worker 不够或下游变慢，
先调大 `--max-concurrent-reconciles`（默认 30）并确认 API QPS 余量
（`--kube-api-qps` 默认 300 / burst 600）；若「错误率」也在涨，是下游故障，扩容无用。

> 两个易错读法：
> - 「最长调谐执行」是**此刻正在跑的那一个**已耗时多久（会回落），不是历史最大值；
> - 它与「Worker 并发」共用一张图但**单位不同**（秒 vs 个，右轴是并发数），
>   不要拿两个数值直接相减/相除。

## 大促 / 早高峰观察要点

1. **规模与占比**：先看「工作空间总数」与「Running 实例数」，两条线的差值就是待机/停止的存量。
   Running 占比长期偏低说明 `idleTimeout` 或手动停止回收过猛；占比接近 100% 说明没有弹性余量。
2. **实例爬坡**：「Running 工作空间数量随时间变化」与「各 Phase 在线实例水位」应呈现
   `Pending/Starting → Running` 的陡峭爬坡；`Starting` 长时间堆积说明调度或镜像拉取成为瓶颈，
   而非调谐引擎。
3. **峰值水位**：早高峰看「Running 峰值（同时运行数）」抬到多少，据此定容量与告警阈值；
   「峰值出现时间」用于复盘峰值时段是否与业务高峰对齐。
4. **引擎容量**：「调谐耗时 P95/P99」×「调谐吞吐量」≈ worker 占用。当
   「队列深度」持续增长且「排队延迟 P95」超过 1s 时，调大
   `--max-concurrent-reconciles`（默认 30）并观察 API Server QPS 余量
   （`--kube-api-qps` / `--kube-api-burst`）。
5. **错误风暴**：「调谐错误 (5m)」> 0 时结合「调谐速率与错误率」面板判断：
   错误率上升但吞吐不降 → 限流重试在正常消化；两者同时恶化 → 下游依赖（存储/网络策略）故障。
6. **休眠回收**：大促结束后 `Sleeping`（IdleTimeout 回收）应逐步替换 `Running`，
   队列延迟回落至亚秒级。

## 告警建议（示例 PromQL）

```promql
# 调谐错误持续出现（内置指标，controller 名固定为 workspace）
sum(rate(controller_runtime_reconcile_errors_total{controller="workspace"}[5m])) > 0.1

# 队列积压：排队延迟 P95 超过 2 秒（内置 workqueue 直方图，controller 名固定为 workspace）
histogram_quantile(0.95, sum by (le) (rate(workqueue_queue_duration_seconds{controller="workspace"}[5m]))) > 2

# Pending/Starting 实例长时间无法进入 Running
sum(workspace_active_instances{phase=~"Pending|Starting"}) > 100

# 同时 Running 数量超过容量红线
max(workspace_running_instances) > 4500

# 同时 Running 的短时峰值超过红线（1 小时内任意时刻，不受抓取间隔影响）
max(workspace_running_peak_instances) > 4500

# 容量逼近：Running 占全部工作空间的比例超过 90%（需要扩容或调小 idleTimeout）
max(workspace_running_instances) / clamp_min(max(workspace_total_instances), 1) > 0.9

# 窗口口径的峰值（跨 manager 重启仍有效，适合每日容量复盘报表）
max(max_over_time(workspace_running_instances[24h]))
```

