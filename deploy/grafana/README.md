# Workspace Operator 可观测性看板（Task 5.2）

## 内容

- `dashboards/workspace-operator.json`：Grafana 看板 **「Workspace Operator - 调谐引擎水位」**。

## 指标清单

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

## 导入看板

Grafana → Dashboards → New → Import → 上传 `dashboards/workspace-operator.json`，
选择 Prometheus 数据源即可（uid 固定为 `workspace-operator`）。

## 大促 / 早高峰观察要点

1. **实例爬坡**：「各 Phase 在线实例水位」应呈现 `Pending/Starting → Running` 的陡峭爬坡；
   `Starting` 长时间堆积说明调度或镜像拉取成为瓶颈，而非调谐引擎。
2. **引擎容量**：「调谐耗时 P95/P99」×「调谐吞吐量」≈ worker 占用。当
   「队列深度」持续增长且「排队延迟 P95」超过 1s 时，调大
   `--max-concurrent-reconciles`（默认 30）并观察 API Server QPS 余量
   （`--kube-api-qps` / `--kube-api-burst`）。
3. **错误风暴**：「调谐错误 (5m)」> 0 时结合「调谐速率与错误率」面板判断：
   错误率上升但吞吐不降 → 限流重试在正常消化；两者同时恶化 → 下游依赖（存储/网络策略）故障。
4. **休眠回收**：大促结束后 `Sleeping`（IdleTimeout 回收）应逐步替换 `Running`，
   队列延迟回落至亚秒级。

## 告警建议（示例 PromQL）

```promql
# 调谐错误持续出现（内置指标，controller 名固定为 workspace）
sum(rate(controller_runtime_reconcile_errors_total{controller="workspace"}[5m])) > 0.1

# 队列积压：排队延迟 P95 超过 2 秒（内置 workqueue 直方图，controller 名固定为 workspace）
histogram_quantile(0.95, sum by (le) (rate(workqueue_queue_duration_seconds{controller="workspace"}[5m]))) > 2

# Pending/Starting 实例长时间无法进入 Running
sum(workspace_active_instances{phase=~"Pending|Starting"}) > 100
```
