# 把 Workspace Operator 监控接入 Prometheus

本文是**接入操作手册**（纯运维动作，不改代码）。看板本身见 `../grafana/`。

涉及的已部署对象（全部随 `deploy/install.yaml` 交付）：

| 对象 | 值 |
| --- | --- |
| 命名空间 | `agent-deploy-system` |
| Service | `agent-deploy-controller-manager-metrics-service` |
| Service 端口 | 名字 `http`，`8080`（另有一个 `8081/health`，**不要抓**） |
| Pod 标签 | `app.kubernetes.io/name=agent-deploy`, `control-plane=controller-manager` |
| 容器 | `manager`，端口名 `http`(8080) / `health`(8081) |
| 协议 | **明文 HTTP，无需 Bearer Token、无需 TLS**（`--metrics-secure=false`） |

## 第 0 步：先确认指标真的已经暴露

不要跳过这步——先排除"服务通了但指标没开"的情况。

```bash
# 端口转发到本地（不依赖集群内外网络策略）
kubectl -n agent-deploy-system port-forward \
  svc/agent-deploy-controller-manager-metrics-service 8080:8080

# 另开一个终端
curl -s http://127.0.0.1:8080/metrics | grep '^workspace_'
```

预期至少能看到（数字随集群实际变化）：

```
workspace_active_instances{phase="Running"} 7
workspace_active_instances{phase="Sleeping"} 35
workspace_total_instances 42
workspace_running_instances 7
workspace_running_peak_instances 12
workspace_running_peak_timestamp_seconds 1.792628086e+09
workspace_reconcile_duration_seconds_bucket{le="0.1"} 123
```

- **能看到** → 进入方案 A / B / C。
- **看不到 / 返回 404** → 大概率抓到了 `8081`，见文末「排错」。
- **连接被拒** → 检查 manager Pod 是否 Running，以及 `agent-deploy-system` 里是否有 NetworkPolicy（见「排错」）。

## 选哪个方案

| 你的 Prometheus 是 | 用哪个方案 | 文件 |
| --- | --- | --- |
| prometheus-operator / kube-prometheus-stack / ACK 的 ack-prometheus-operator | **方案 A：ServiceMonitor** | `servicemonitor.yaml` |
| 自己写的 `prometheus.yml`（原生 Prometheus） | **方案 B：scrape_configs** | `prometheus-scrape.yaml` |
| 不在这个集群里（外部 Prometheus） | **方案 C：暴露到集群外** | `metrics-external-service.yaml` + `prometheus-scrape-external.yaml` |

## 方案 A：ServiceMonitor（Prometheus Operator）

### A1. 先确认你的 Prometheus 用什么标签选 ServiceMonitor

这一步最容易翻车：`ServiceMonitor` 的 `metadata.labels` 必须命中 Prometheus CR 的
`spec.serviceMonitorSelector`，否则**创建成功但永远不会被抓取**。

```bash
kubectl get prometheus -A \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.namespace}{"\t"}{.spec.serviceMonitorSelector}{"\n"}{end}'
```

典型输出：

```
prometheus-k8s	monitoring	{"matchLabels":{"release":"prometheus"}}
```

把 `servicemonitor.yaml` 里的 `release: prometheus` 改成上面看到的标签值。

> 常见默认值：Helm 安装的 kube-prometheus-stack 是 `release: <Helm release 名>`
> （如 `prometheus` / `kube-prometheus-stack`）；ACK 的 ack-prometheus-operator
> 通常是 `release: ack-prometheus-operator`。**以 A1 命令的实际输出为准**，不要照抄。

### A2. 确认命名空间能被选中

```bash
kubectl get prometheus -n monitoring prometheus-k8s \
  -o jsonpath='{.spec.serviceMonitorNamespaceSelector}{"\n"}'
```

- 输出为空 `{}` → 只能监听 **Prometheus 自己所在命名空间** 里的 ServiceMonitor；
- 输出有 `matchLabels` / `any: true` → 按标签选命名空间。

**最省事的做法**：把 ServiceMonitor 直接 apply 到 Prometheus 所在命名空间
（文件里已写 `namespace: monitoring`，按需改）。

### A3. 应用

```bash
# 改成你的监控命名空间，例如 monitoring / prometheus / kube-prometheus
kubectl apply -n monitoring -f servicemonitor.yaml

# 确认创建成功
kubectl -n monitoring get servicemonitor agent-deploy-controller-manager
```

### A4. 确认 Prometheus 已重新加载

Operator 通常几秒内自动 reload。检查：

```bash
kubectl -n monitoring get prometheus -o jsonpath='{.items[0].status.conditions}{"\n"}'
```

## 方案 B：原生 Prometheus（scrape_configs）

把 `prometheus-scrape.yaml` 里的 job 追加到 `prometheus.yml`：

```yaml
scrape_configs:
  - job_name: workspace-operator
    scheme: http
    kubernetes_sd_configs:
      - role: endpoints
        namespaces: {names: [agent-deploy-system]}
    relabel_configs:
      - source_labels: [__meta_kubernetes_service_name]
        regex: agent-deploy-controller-manager-metrics-service
        action: keep
      - source_labels: [__meta_kubernetes_endpoint_port_name]
        regex: http
        action: keep
```

校验并热加载：

```bash
promtool check config /etc/prometheus/prometheus.yml
curl -X POST http://<prometheus>:9090/-/reload    # 需 --web.enable-lifecycle
```

> 如果用的是 kube-prometheus-stack 的 `additionalScrapeConfigs`：只拷贝
> 「`- job_name:` 及其下级」，**不要带 `scrape_configs:` 这一行**。

## 方案 C：Prometheus 在集群外

`agent-deploy-controller-manager-metrics-service` 是 **ClusterIP**，集群外抓不到，
需要把指标暴露到集群网络之外。**不要直接改这个 Service 的 type**——它定义在
`deploy/install.yaml` 里，下次升级 `kubectl apply -f deploy/install.yaml` 会把它
改回 ClusterIP，抓取会在没有任何报错的情况下断掉。

正确做法是**单独建一个专供外部抓取的 Service**（见 `metrics-external-service.yaml`），
与安装包解耦，不想暴露了删掉即可。

### C1. 选择暴露方式

| 方式 | 适用 | 安全等级 | 备注 |
| --- | --- | --- | --- |
| **NodePort** | 任何集群，Prometheus 能通到 worker 节点网段 | 中 | 最通用，需在安全组放行 nodePort |
| **内网 LoadBalancer/SLB + ACL** | 云上（ACK/EKS/GKE） | 高（推荐） | 白名单只放行 Prometheus 出口 IP |
| **Ingress/网关反代** | 已有统一网关 | 高 | 多一跳，需透传 `/metrics`，注意别加鉴权页 |
| ~~直接改官方 Service~~ | —— | —— | **不要**，升级会被覆盖 |

### C2. 建对外 Service（以 NodePort 为例）

```bash
kubectl apply -f metrics-external-service.yaml
kubectl -n agent-deploy-system get svc agent-deploy-controller-manager-metrics-external
# 期望：TYPE=NodePort，PORT(S)=8080:30080/TCP
```

用内网 SLB 的话，把文件里 `type: NodePort` 那段换成下面注释掉的 `LoadBalancer`
段（含云厂商注解与 ACL），端口直接用 8080。

### C3. 放行网络

- **NodePort**：放行 worker 节点安全组入站 `30080/TCP`，源地址限制为 **Prometheus 主机 IP**
  （不要开 `0.0.0.0/0`）。
- **内网 SLB**：在 SLB 的白名单/ACL 里加 Prometheus 出口 IP。

### C4. 集群外 Prometheus 配置

集群外 Prometheus **无法使用 `kubernetes_sd_configs`**（除非额外给它 kubeconfig），
所以用 `static_configs` 写死地址，见 `prometheus-scrape-external.yaml`：

```yaml
scrape_configs:
  - job_name: workspace-operator
    scheme: http
    metrics_path: /metrics
    scrape_interval: 30s
    static_configs:
      - targets: ['10.0.0.11:30080']     # 节点 IP:nodePort，或内网 SLB 地址:8080
        labels:
          cluster: prod                  # 抓多套集群时必须加，否则数据会混在一起
    relabel_configs:
      - source_labels: [__address__]
        target_label: instance
        replacement: workspace-operator-prod
```

改完执行：

```bash
promtool check config /etc/prometheus/prometheus.yml
curl -X POST http://localhost:9090/-/reload      # 需 --web.enable-lifecycle
```

> `externalTrafficPolicy` 保持默认 `Cluster`：任意节点 IP 都能转发，
> 单节点故障不影响抓取。若改成 `Local`，只能抓 manager 所在那个节点。

### C5. 在 Prometheus 主机上先自测

配到 Prometheus 之前，先在 Prometheus 那台机器上验证网络与指标：

```bash
curl -s --max-time 5 http://10.0.0.11:30080/metrics | grep '^workspace_' | head
```

- **通** → 抓取配置基本没问题。
- **timeout** → 安全组/ACL 没放行，或 nodePort 不在放行范围。
- **404** → 端口选错（抓到了 8081），或用了 Ingress 且路径没透传。

### C6. 安全提醒（重要）

当前 `/metrics` 是**明文 HTTP、无任何鉴权**（`--metrics-secure=false`）。一旦暴露到
集群外，任何能连通该端口的进程都能读到工作空间数量、调谐耗时等内部信息。
因此：

1. **至少**用安全组/ACL 把来源限制到 Prometheus 主机；
2. 若所在网络不可信，把 manager 的 `--metrics-secure` 改回 `true`（二进制默认值），
   抓取侧改成 HTTPS + Bearer Token，具体步骤见
   `prometheus-scrape-external.yaml` 末尾的「加固方式」注释；
3. 优先走内网 SLB，**不要**把 metrics 端口挂到公网 Ingress 上。

### C7. 临时排查用的端口转发

不通的时候用来快速区分「应用问题」还是「网络问题」：

```bash
kubectl -n agent-deploy-system port-forward \
  svc/agent-deploy-controller-manager-metrics-service 8080:8080
curl -s http://127.0.0.1:8080/metrics | grep '^workspace_'
```

能通说明指标本身没问题，问题在暴露方式或安全组。


## 验证接入成功

### 1. Prometheus UI

打开 `Status → Targets`，应出现 `workspace-operator`，状态 `UP`。
如果 `DOWN` 且报 `context deadline exceeded` / `connection refused`，看「排错」。

### 2. PromQL 自检（在 Prometheus 查询框里跑）

```promql
# 目标在线
up{job="workspace-operator"} == 1

# 六个 phase 序列都在（Pending/Starting/Running/Sleeping/Stopped/Unknown）
count(workspace_active_instances)

# 总数与 Running
workspace_total_instances
workspace_running_instances
workspace_running_peak_instances

# 一分钟内确实有新样本（说明活着的，不是残留）
count_over_time(workspace_total_instances[1m]) > 2
```

### 3. 导入看板

Grafana → Dashboards → New → Import → 上传
`../grafana/dashboards/workspace-operator.json`，数据源选刚接入的 Prometheus。
看板 uid 固定为 `workspace-operator`，重复导入会覆盖（不会产生副本）。

## 排错

### 指标 404 / 抓不到数据

`manager` 容器声明了 **两个** 端口：`http`(8080，有 `/metrics`) 和 `health`(8081，只有
`/healthz`、`/readyz`)。`role: pod` 会给每个端口生成一个 target，按端口名
`http` 过滤即可；`ServiceMonitor` 里 `port` 也必须写 `http`。

```bash
kubectl -n agent-deploy-system get svc agent-deploy-controller-manager-metrics-service -o yaml | grep -A6 ports
```

### 抓取被 NetworkPolicy 拦截

```bash
kubectl -n agent-deploy-system get networkpolicy
```

部署约定是 **`agent-deploy-system` 不装任何 NetworkPolicy**（无策略即全放行）。
若实际存在策略，需要给监控命名空间加一条入站放行：

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-metrics-scrape
  namespace: agent-deploy-system
spec:
  podSelector:
    matchLabels:
      control-plane: controller-manager
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
      ports:
        - protocol: TCP
          port: 8080
```

### ServiceMonitor 建了但 Targets 里没有

按顺序查三件事：`metadata.labels` 是否命中 `serviceMonitorSelector`；
命名空间是否被 `serviceMonitorNamespaceSelector` 允许；
Prometheus 的 ServiceAccount 是否有该命名空间的 `services/endpoints` 读权限
（kube-prometheus-stack 默认有）。

### 指标值翻倍 / `rate()` 是真实值的 2 倍

同一实例被抓了两次。常见原因：**同时启用了 `role: endpoints` 和 `role: pod`
两个 job**，或 ServiceMonitor 与其他 scrape 配置重叠。二选一。
（看板里已用 `max()` 而非 `sum()` 聚合，能容忍多副本，但重复抓取会让
`rate()`/`histogram_quantile()` 失真。）

### 多副本（HA）时数字不对

`replicas: 1` 时无需关心。若将来扩到多副本：只有 Leader 会更新这些 Gauge，
Standby 暴露的是 0；**看板用 `max()` 聚合**已能取到 Leader 的真实值，
但告警表达式请统一写 `max(...)`，不要写 `sum(...)`。

### 峰值重启后变小

`workspace_running_peak_instances` 是**进程内**高水位，滚动升级/主备切换后归零。
跨重启的长期峰值用 `recording-rules.yaml` 里的 `max_over_time` 规则固化，
或用看板折线图图例的 `Max` 列（窗口口径）。
