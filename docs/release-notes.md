# 发布说明（Release Notes）

> 版本号以实际交付为准；本文件记录每次升级的**行为变更与操作要求**。

## 下一版本（含 Task 2.1 收敛 + Task 5.2 可观测性）

### 1. 升级影响：所有 Workspace Pod 会被重建一次（重要）

本次升级后，manager 会为每个 Workspace 的 Deployment **pod template** 补上
`app.kubernetes.io/managed-by: workspace-operator` 标签（共享 NetworkPolicy 的
`podSelector` 依据）。该模板字段变化会命中调谐 diff，触发**全量 Workspace 重建**。

由于 Workspace Deployment 使用 `Recreate` 策略（先停后起），每个 Workspace 在
重建期间有**短暂不可用窗口**；叠加 Kata MicroVM 冷启动，窗口比普通容器更长。

**建议操作**

1. 选择低峰期升级，预留"全部 Workspace 逐个重启 + Kata 冷启动"的时长；
2. 升级前可临时调低 manager 的 `--max-concurrent-reconciles`（默认 30，例如
   调至 5～10）以控制重建发起速度，观察重建完成后再调回；
3. 升级期间关注：
   - `kubectl get pods -A -l app=workspace`（重建进度）；
   - `workspace_reconcile_duration_seconds` 的 P95/P99；
   - `controller_runtime_reconcile_errors_total{controller="workspace"}` 是否出现错误风暴；
   - 各 Workspace 的 Deployment rollout 状态。

### 2. 回滚说明

回滚 manager 镜像后，pod template 会**再次**发生 diff（去掉 managed-by 标签），
因此会触发**第二次全量重建**。回滚同样需要预留升级窗口，不要假设回滚是零成本操作。

### 2.1 标签作用域调整（不影响 Pod 重启）

`app.kubernetes.io/managed-by: workspace-operator` 现在只写在 **Pod template** 上
（共享 NetworkPolicy 的 selector 依据）；PVC / Service / Ingress / Deployment 元数据
会渐进移除该标签，下一次调谐自动收敛，**不触发**额外重启。若有用例按该标签检索
子资源，请改用 `app=workspace` + `workspace=<name>` 选择器。

### 3. 可观测性变更

- Manager 的 `/metrics` 改为**明文 HTTP `:8080`**（`--metrics-secure=false`，
  安全取舍见 `deploy/grafana/README.md`），Service 端口名为 `http`；
- 新增 Prometheus 指标与 Grafana 看板（`deploy/grafana/`），抓取前提与最简配置
  见该目录 README；
- **队列延迟指标改名**：删除自建的 `workspace_queue_latency_seconds`，改用
  controller-runtime 内置的 `workqueue_queue_duration_seconds{controller="workspace"}`
  （语义一致、少一把热路径锁）。若存量告警/看板引用了旧指标名，需同步修改；
- **调谐错误指标去重**：删除自建的 `workspace_reconcile_errors_total`，统一使用
  controller-runtime 内置的 `controller_runtime_reconcile_errors_total{controller="workspace"}`；
  存量告警/看板若引用旧名称需同步修改；
- 安装包**不包含** ServiceMonitor（`#- ../prometheus`）与 metrics 入站
  NetworkPolicy（`#- ../network-policy`），理由与启用条件见
  `config/default/kustomization.yaml` 注释。

### 4. 网络策略语义变化（需同步告知使用者）

- per-workspace `spec.networkPolicy` 中 `blockedCIDRs` / `allowedCIDRs` /
  `allowedNamespaces` **已废弃**：仍可提交但不再生效，controller 会发
  `Warning / NetworkPolicyOverridden` 事件提示；集群级放行请改用 manager 的
  `--netpol-allowed-namespaces`（或 `NETPOL_ALLOWED_NAMESPACES`）；
- 仅 `spec.networkPolicy.disabled` 仍然生效（opt-out）；
- 命名空间级共享 NetworkPolicy 的**入站不限端口与协议**（含 ICMP），安全边界是
  **出站隔离**；详见 `docs/api_manual.md` 安全附录。
