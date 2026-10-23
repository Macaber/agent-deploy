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

#### `targets` 里到底填哪个 IP？

**填任意一个 worker 节点的内网 IP（InternalIP）:30080 即可，不需要是 manager Pod 所在的节点。**

NodePort 服务默认 `externalTrafficPolicy: Cluster`，kube-proxy 会在「被访问的那个节点」
把流量转发到 Pod 实际所在的节点，所以任意节点都是合法入口：

```
Prometheus ──► 节点A:30080 ──(kube-proxy 转发)──► 节点B 上的 manager Pod
```

查节点 IP：

```bash
# 所有节点的内网 IP
kubectl get nodes -o wide
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}'

# manager Pod 在哪个节点（仅供排查，targets 不需要填它）
kubectl -n agent-deploy-system get pod -l control-plane=controller-manager -o wide
```

验证「任意节点都通」（顺便确认安全组开对了）：

```bash
for ip in $(kubectl get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}'); do
  printf '%s: ' "$ip"
  curl -s -o /dev/null -w '%{http_code}\n' --max-time 3 "http://$ip:30080/metrics"
done
# 期望全部 200；有 timeout 的节点说明该节点安全组没放行
```

三个常见错误：

| 写错成 | 结果 |
| --- | --- |
| Pod IP（如 `10.244.x.x`） | 集群外网络不可达，永远 timeout |
| 公网 EIP | 一般不推荐；除非确实放行且节点挂了 EIP |
| 只给「manager 所在节点」开安全组，却填了别的节点 | 换成别的节点就不通；按 worker 节点安全组整体放行更省事 |

> **别长期写死节点 IP**：节点缩容/换机/释放后地址就变了，Targets 会静默变 DOWN。
> 生产建议改用内网 SLB（`metrics-external-service.yaml` 方式 2）拿固定 VIP，
> 或者给节点 IP 配一条内网 DNS 记录、`targets` 写域名。

> 只有把 `externalTrafficPolicy` 改成 `Local` 时，才**必须**指向 manager Pod 所在节点
> （`Local` 不跨节点转发）。确认当前策略：
> `kubectl -n agent-deploy-system get svc agent-deploy-controller-manager-metrics-external -o jsonpath='{.spec.externalTrafficPolicy}'`
> —— 输出为空即 `Cluster`。

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

#### 导入后仍然是空的？按这三项检查

看板要"能看"，需要**三件事同时成立**，缺一个就是空面板：

| # | 前提 | 检查命令 / 方法 | 不满足时的现象 |
| --- | --- | --- | --- |
| 1 | **manager 镜像包含新指标** | `curl -s http://<target>/metrics \| grep workspace_total_instances` | 4 个规模面板（总数/Running/峰值/峰值时间）和折线图**恒为空**，其余调谐类面板正常 |
| 2 | Prometheus 抓取成功 | Prometheus → Status → Targets，`workspace-operator` 为 `UP` | 所有面板都空 |
| 3 | 导入时选对数据源 | 面板无 "Datasource not found" 报错 | 面板报红色数据源错误 |

> **第 1 条最容易漏**：`workspace_total_instances` / `workspace_running_instances` /
> `workspace_running_peak_instances` / `workspace_running_peak_timestamp_seconds` 是
> 新增指标，**只有新版本 manager 才会暴露**。如果集群里还是旧镜像，导入新看板后
> 这 4 个面板会一直是空的——先升级 manager 镜像，再谈看板。
> 判断方法：旧版本只有 `workspace_active_instances` 和 `workspace_reconcile_duration_seconds`。

#### 刚导入时折线图是空的一段，属于正常

指标从**开始被抓取的那一刻**起才有数据：

- 4 个 stat 面板：1～2 个抓取周期（约 30～60 秒）后就有值；
- 折线图默认时间范围是 `now-6h`，左侧 6 小时是空的，需要等数据积累才会画满；
- 想看"当前"而不是趋势，把时间范围调到 `Last 15 minutes`。

#### 多集群：加一套集群要改什么

**1）抓取侧**：在同一个 job 的 `static_configs` 里再加一行，只改 `targets` 和
`cluster`（`instance` 会从 `cluster` 自动派生，不用手改）：

```yaml
    static_configs:
      - targets: ['10.0.0.11:30080']
        labels: {cluster: prod,  env: prod}
      - targets: ['10.0.1.11:30080']   # prod1 的节点 IP + 它自己的 nodePort
        labels: {cluster: prod1, env: prod}
    relabel_configs:
      - source_labels: [cluster]
        target_label: instance
        regex: (.+)
        replacement: workspace-operator-$1
```

> `cluster` 是**区分两套集群的唯一标识**，必须唯一。如果漏写或写重，
> 两套集群的序列标签完全相同，Prometheus 会报 duplicate sample，数据会互相覆盖。
> 另外 nodePort 是**每个集群各自分配**的，prod 的 30080 不代表 prod1 也是，要在
> 对应集群里查实际值。

**2）看板侧**：看板已内置「集群」变量（`label_values(workspace_total_instances, cluster)`，
支持单选/多选/全选），所有面板都按 `cluster` 分组：

- 选 **All**：每套集群一条线/一个数值，互不干扰；
- 选 **某一套**：只看该集群，数字与门户页面对应；
- 图例格式为 `{{cluster}} · Running` / `{{cluster}} · P95`，一眼能分辨来源。

**3）告警侧**：多集群时表达式要带维度，否则告警说不清是哪套集群：

```promql
# 每套集群分别判断（返回结果自带 cluster 标签）
max by (cluster) (workspace_running_instances) > 4500

# 不要写 sum(...)：会把两套集群的数字相加，阈值失去意义
```

> 未打 `cluster` 标签的单集群部署仍然兼容：`cluster=~".*"` 会匹配到空标签，
> 面板照常显示。

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

### 创建对外 Service 报 `nodePort: provided port is already allocated`

NodePort 是**全集群唯一**的（跨命名空间共享，默认范围 30000–32767），
换一个集群部署时 `30080` 很可能已被别的 Service（Ingress/LB/其它业务）占用。

两种处理，按需选：

```bash
# a) 推荐：删掉 YAML 里的 nodePort 行，让 k8s 自动分配，再读回实际端口
kubectl -n agent-deploy-system get svc agent-deploy-controller-manager-metrics-external \
  -o jsonpath='{.spec.ports[0].nodePort}{"\n"}'

# b) 自己挑空闲端口：先列全集群已占用的（注意 -A，占用者可能在别的命名空间）
kubectl get svc -A \
  -o jsonpath='{range .items[*]}{range .spec.ports[*]}{.nodePort}{"\n"}{end}{end}' \
  | sort -n | uniq
```

> 自动分配的代价：同一个 Service 不删就稳定，但 delete 重建后端口会变，
> 记得同步改 Prometheus 的 `targets` 与安全组。
> 若该集群有内网负载均衡，直接用 `metrics-external-service.yaml` 里的
> **LoadBalancer** 变体更省事——不占 NodePort、地址稳定、还能挂 ACL 白名单。

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

### 「峰值出现时间」显示 1970 年（如 1970-01-22 01:57:40）

**单位不匹配，不是数据错。** `workspace_running_peak_timestamp_seconds` 是 Unix **秒**
（约 `1.79e9`），而 Grafana 的 `dateTimeAsIso` 单位按**毫秒**解析，把 `1.79e9`
当成 `1.79e6` 秒 → 落在 1970-01-21 附近。

修正：查询里 `×1000`。

```promql
max(workspace_running_peak_timestamp_seconds) * 1000
```

自检方法：把 1970 年显示出来的时间换算回秒再 ×1000，应该正好等于
`workspace_running_peak_timestamp_seconds` 的原始值。例如显示
`1970-01-21 17:57:40 UTC` = `1792660` 秒，× 1000 = `1792660000` ≈ 2026-10-22。

### 「峰值出现时间」早于数据起点（比如 10-23 才接入，峰值是 10-22）

**预期行为，不是数据错。** 峰值和时间戳是 **manager 进程的内存状态**，语义为
「本进程启动以来第一次达到峰值的时刻」，与 Prometheus 从哪天开始抓取无关。
接入抓取的那一刻，Prometheus 只是把内存里早就存在的值抄了一份。

验证：

```bash
kubectl -n agent-deploy-system get pod -l control-plane=controller-manager \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.startTime}{"\n"}{end}'
```

Pod 启动时间早于接入时间，即说明值来自进程内存。

两个推论：

1. **manager 重启（含滚动升级）后峰值与时间戳重置**，从重启后的当前 Running 数重新起算；
2. **删掉/缩容工作空间不会降低峰值**（高水位只升不降），
   `workspace_total_instances` / `workspace_running_instances` 会在一分钟内下降。

要「从接入时刻算起」的峰值，用 `max_over_time(workspace_running_instances[$__range])`
（即看板折线图图例的 `Max` 列）。

> 不要反过来把指标改成毫秒去迁就 Grafana：Prometheus 约定时间戳统一用秒，
> 指标名也带 `_seconds` 后缀，改成毫秒会和其它指标/告警不一致。

