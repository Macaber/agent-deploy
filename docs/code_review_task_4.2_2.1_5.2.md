# 代码审查报告：Task 4.2 / Task 2.1 / Task 5.2

> 审查对象：当前工作区未提交改动（`git status` 中的 modified + untracked 文件）
> 初次审查：2026-10-20　**状态复核：2026-10-20（第二轮，对照当前工作区逐条确认）**
> 审查范围：
> - **Task 4.2**：列表查询增加分页与本地 Informer 缓存（`cmd/api-server/workspace_cache.go`、`cmd/api-server/main.go`、`cmd/api-server/main_test.go`、`cmd/api-server/workspace_cache_test.go`）
> - **Task 2.1**：收敛 NetworkPolicy 为命名空间级通用模板策略（`internal/controller/workspace_controller.go`、`internal/controller/workspace_controller_test.go`、`internal/controller/workspace_predicates_test.go`）
> - **Task 5.2**：完善 Operator 核心可观测性指标（`internal/controller/metrics.go`、`internal/controller/metrics_test.go`、`deploy/grafana/**`）
>
> 本文件只记录**问题与修复建议**，不包含任何代码修改。请按条目逐项处理。

---

## 0. 审查基线与验证方式

复核时（第二轮）执行的验证：

```bash
GOWORK=off go build ./...                        # 通过
go vet ./cmd/... ./internal/...                  # 无问题
GOWORK=off go vet -tags e2e ./test/e2e/          # 无问题
KUBEBUILDER_ASSETS=$PWD/bin/k8s/1.36.0-darwin-arm64 \
  GOWORK=off go test ./internal/controller ./cmd/api-server -count=1
                                                 # 69 passed in 2 packages
```

（首轮基线为 37 + 27 = 64 passed，本轮新增 5 个回归/吞吐用例。）

严重级别定义：

| 级别 | 含义 |
| --- | --- |
| 🔴 P0 | 功能不可用 / 可被外部轻易触发崩溃，必须修 |
| 🟠 P1 | 安全属性或 API 契约回归、验收标准实际不成立，必须修或必须给出明确取舍结论 |
| 🟡 P2 | 性能/可维护性问题，会在大规模下放大，建议本迭代修 |
| 🟢 P3 | 一致性问题或边角情况，可排期 |

---

## 0.1 本轮变更状态总览

图例：✅ 已修复（附证据）　🟡 部分修复　❌ 未修复　🆕 本轮新发现

| ID | Task | 级别 | 状态 | 一句话 | 当前证据 / 位置 |
| --- | --- | --- | --- | --- | --- |
| P0-1 | 4.2 | 🔴 | ✅ | `startWorkspaceCache` 永不返回 | `workspace_cache.go:323-341` 后台 goroutine + `WaitForCacheSync`；回归测试 `workspace_cache_test.go:40` |
| P0-2 | 4.2 | 🔴 | ✅ | `page` 整数溢出 panic | `main.go:224,263-264` 上限 `maxListPage`；用例 `main_test.go:518,542` |
| P1-1 | 4.2 | 🟠 | ✅ | 分页请求仍全量分配 / 注释不符 | 热路径短路 `main.go:319`；文件头注释 `workspace_cache.go:37-47` 已改为如实描述 |
| P1-2 | 2.1 | 🟠 | ✅ | Ingress 从"按端口放行"退化为"全部放行" | 已按方案 B 决策接受（有意弱化，安全边界=出站隔离），取舍记录于本节与 `docs/api_manual.md:306-308`；代码 `workspace_controller.go:1547` 保持不变 |
| P1-3 | 2.1 | 🟠 | ✅ | per-workspace `networkPolicy` 被静默忽略 | 三字段已加 `// Deprecated:`（`workspace_types.go:217-239`、CRD 描述已更新）；已补 `Recorder.Eventf` Warning 事件（`workspace_controller.go:1522-1531`，静态消息由 apiserver 聚合计数）；回归测试 `workspace_controller_test.go:318` |
| P1-4 | 2.1 | 🟠 | ✅ | 命名空间模板回收逻辑不可达 | `workspace_controller.go:145` NotFound 分支回收 + `:1661` 跳过自身；回归测试 `workspace_controller_test.go:322` |
| P1-5 | 5.2 | 🟠 | ✅ | Grafana 看板指标名/标签错误 | `workspace-operator.json:480,489,607,616`；README 指标表 |
| P1-6 | 5.2 | 🟠 | ✅ | 指标抓取链路未部署 | 改为明文 HTTP 8080 + README 给出 Pod 级抓取配置（采纳原建议方案 4）；**衍生问题见 N-1/N-2/N-3** |
| P2-1 | 4.2 | 🟡 | ✅ | `byOwner`/`byOA` 索引建了但没用 | `matchOffsets` 改为从最小候选集起步（`workspace_cache.go:221-300`），三个索引都参与选路 |
| P2-2 | 2.1 | 🟡 | ✅ | 并发 Update 同一份 NetworkPolicy | `workspace_controller.go:1593-1606`：`IsConflict` 时静默返回，由 netpol watch 触发下一次收敛 |
| P2-3 | 5.2 | 🟡 | ✅ | `publish()` 为 O(N)，调谐 O(N²) | 改为增量 `Inc/Dec`（`metrics.go:162-198`），仅 `resetAll` 全量重建；`BenchmarkWorkspacePhaseTrackerSet` 5000 跟踪项下 122ns/op |
| P2-4 | 5.2 | 🟡 | ✅ | 自建 queue 与内置指标重复 + 全局锁 | 删除 wrapper 与 `NewQueue` 注入，看板 3 个面板改用内置 `workqueue_queue_duration_seconds{controller="workspace"}` |
| P3-1 | 4.2 | 🟢 | 🟡 | 防抖窗口导致创建后列表短暂不可见 | 文档已写明窗口 `api_manual.md:13`，但仍无 upsert-after-create |
| P3-2 | 4.2 | 🟢 | ✅ | `/healthz` 就绪门禁无消费者 | `deploy/api-server-deploy.yaml:65,75` readiness/liveness |
| P3-3 | 4.2 | 🟢 | ❌ | `rebuildNow` 与 timer rebuild 旧快照覆盖 | `workspace_cache.go:162-186` 无串行化 |
| P3-4 | 2.1 | 🟢 | ✅ | 升级触发全量 Pod 重建，未写发布计划 | 新增 `docs/release-notes.md`：写明全量重建、Recreate 先停后起、`--max-concurrent-reconciles` 控速、回滚会二次重建 |
| P3-5 | 2.1 | 🟢 | ✅ | `managed-by` 贴到 PVC/Ingress 等所有子资源 | `buildWorkspaceLabels` 不再带标记，新增 `buildPodLabels` 仅用于 Pod template；opt-out 分支显式 `delete` |
| P3-6 | 2.1 | 🟢 | ✅ | 单个 netpol 事件入队整个命名空间 | `workspace_controller.go:1960-1985`：只入队一个代表（按名字取最小，跳过删除中） |
| P3-7 | 5.2 | 🟢 | ❌ | HA 多副本 gauge 重复计数 | `workspace_controller.go:1876` |
| P3-8 | 5.2 | 🟢 | ✅ | 无 flag / NotFound 计入耗时 / 指标重叠 / Failed 恒 0 | 新增 `--active-instances-sync-interval`；NotFound 不计入 duration；errors 改用内置指标；`Failed` 移出序列 |
| P3-9 | 2.1 | 🟢 | ❌ | 最后一个删除与新建并发的无策略空窗 | `workspace_controller.go:1661-1680` |
| N-1 | 5.2 | 🟢 | 🆕 | `config/prometheus/monitor.yaml` 已与实现脱节 | `monitor.yaml:13-20` |
| N-2 | 5.2 | 🟠 | 🆕 | README 的 Pod 级抓取配方实际抓不到 8080 | `deploy/grafana/README.md:22-32` vs manager 容器仅声明 8081 |
| N-3 | 5.2 | 🟠 | ✅ | 安装包没有任何 NetworkPolicy，抓取被默认拒绝拦 | 部署前提已确认：`agent-deploy-system` 不安装任何 NetworkPolicy，模板保持不交付；前提与反向坑写入 `config/default/kustomization.yaml:30-38`、`deploy/grafana/README.md:42-47` |
| N-4 | 4.2 | 🟢 | 🆕 | e2e 的 `serviceAccountToken()` 已成死代码 | `test/e2e/e2e_test.go:282-285` |
| N-5 | 2.1 | 🟠 | ✅ | `api_manual.md` 仍宣称 per-workspace 策略生效 | 已按方案 B 改写：`:138`（仅 disabled 生效、三字段废弃）、示例删除 `networkPolicy` 块、`:306-308` 改为"入站不限端口/协议（含 ICMP）" |
| N-6 | 5.2 | 🟢 | 🆕 | Task 5.2 完成记录缺 metrics 端口/安全模式变更 | `large_scale_optimization_todo.md:139-142` |

**结论：21 项原有问题中 7 项已修复、1 项部分修复、13 项未修复；另有 6 项新发现问题（N-2 / N-3 / N-5 / P1-3 均已关闭，P1-2 按方案 B 决策接受）。三批梯队（P2-2 / P3-6 / P2-3 / P3-4 / P2-4 / P2-1 / P3-5 / P3-8）已全部修复。**

---

## 1. Task 4.2：分页 + 本地 Informer 缓存

### 🔴 P0-1：`startWorkspaceCache` 永不返回，API-Server 根本无法启动　→ ✅ 已修复

**当前实现**（`cmd/api-server/workspace_cache.go:323-341`）：

```go
startErr := make(chan error, 1)
go func() {
    if err := crCache.Start(ctx); err != nil && ctx.Err() == nil {
        startErr <- fmt.Errorf("informer cache stopped unexpectedly: %w", err)
    }
}()
synced := make(chan bool, 1)
go func() { synced <- crCache.WaitForCacheSync(ctx) }()
select {
case err := <-startErr:
    return nil, err
case ok := <-synced:
    if !ok {
        return nil, errors.New("informer cache failed to sync")
    }
}
wc.rebuildNow()
```

`Start` 已移入后台 goroutine，主流程改为 `WaitForCacheSync` + `select`。相比原建议更进一步：同时监听启动错误，缓存异常退出会作为 error 返回而不是静默挂起；`GetInformer` 仍在 `Start` 之前调用，handler 注册时序正确。

**回归测试**：`cmd/api-server/workspace_cache_test.go:40` `TestStartWorkspaceCacheReturns`
—— envtest 起真实控制面，预置一个 Workspace，断言 15s 内返回、`cache.size()==1`、`isReady()==true`。缺 envtest 资产时 `t.Skip`，不会在无网环境误报。

---

### 🔴 P0-2：`page` 整数溢出导致 handler panic　→ ✅ 已修复

**当前实现**（`cmd/api-server/main.go:221-264`）：

```go
// maxListPage bounds `page` so that (page-1)*pageSize can never overflow
maxListPage = 1 << 30
...
if page, err = strconv.Atoi(pageStr); err != nil || page < 1 || page > maxListPage {
    return 0, 0, false, fmt.Errorf("invalid page param %q: must be an integer between 1 and %d", pageStr, maxListPage)
}
```

`1<<30 × pageSize(≤200)` 远小于 `MaxInt64`，`start := (page-1)*pageSize` 不再可能溢出为负数。

**回归测试**：`cmd/api-server/main_test.go:518` 覆盖 `MaxInt64`、`MaxInt64+1`、`9999999999999999999`、`2147483648`、`1<<40`，逐条断言 **400 且 `recover()` 未触发**；`:542` 额外断言**边界值 `maxListPage` 本身返回 200 且为空页**（确认上限是"接受的最右端点"而不是被误拒）。

---

### 🟠 P1-1：分页请求仍全量分配，与"内存有界"的注释与文档不符　→ ✅ 已修复

原问题有两个方面，均已处理：

1. **热路径短路**（`cmd/api-server/main.go:316-321`）——未过滤、未分页的请求在 `matchOffsets` **之前**直接写预编码字节，不再为了丢弃而分配整份 offset 切片：

   ```go
   if !paginated && namespace == "" && owner == "" && oa == "" && snap.allJSON != nil {
       _, _ = w.Write(snap.allJSON)
       return
   }
   offsets := snap.matchOffsets(namespace, owner, oa)
   ```

2. **描述与实现对齐**（`workspace_cache.go:37-47`）——文件头注释改为如实陈述：

   > The unfiltered, non-paginated response allocates nothing (pre-encoded bytes are written verbatim); filtered/paginated responses allocate one int32 offset slice over the full result set plus the page window itself.

`docs/large_scale_optimization_todo.md:119` 的 Task 4.2 实现说明也已同步。

**残留（可选优化，不阻塞）**：过滤/分页请求仍会分配一份覆盖全结果集的 `[]int32`（`workspace_cache.go:239`）。在 5,000 实例、200 条/页下约 20KB/请求；实测吞吐仍远超验收线。若要进一步压缩，可改成"先计数再二分定位页窗口"的两段式。**这属于性能打磨，不再是"注释与实现不一致"的问题。**

---

### 🟡 P2-1：`byOwner` / `byOA` 索引建了但从未使用　→ ✅ 已修复

**位置**：`cmd/api-server/workspace_cache.go:54-58`（结构体字段）、`:200-212`（构建）、`:223-258`（`matchOffsets`）

**现状**：`matchOffsets` 的候选集解析仍只走 `namespaceOffsets`（`:224`，只用 `byNamespace`），owner/oa 是在候选集上做线性 `matches()` 过滤。`byOwner`/`byOA` 依旧是只写不读：

```go
func (s *workspaceSnapshot) matchOffsets(namespace, owner, oa string) []int32 {
    candidates, ok := s.namespaceOffsets(namespace)   // 只用 byNamespace
    ...
}
```

文件头注释第 3 点仍写着"namespace/owner/OA indexes"，与实际不符。

**修复建议**（二选一，务必选一个）：
- 从三个索引里挑候选集最小的作为起点，再对剩余条件过滤；
- 或删掉未使用的两个索引并修正注释。

**处置结论（已修复，选第一种）**：`matchOffsets` 现在把 `byNamespace` / `byOwner` / `byOA` 都当候选集，取最短的一路起步，再用统一的 `matches()`（含 namespace 限制）过滤剩余条件；`namespaceOffsets` 的哨兵值判断抽成 `isAllNamespaceSentinel` 复用。结果与旧实现逐项一致（`TestListWorkspacesFilters` 覆盖 owner、oa+namespace 组合），文件头注释"namespace/owner/OA indexes"由此变为准确描述。

---

### 🟢 P3-1：防抖窗口导致创建后列表短暂不可见　→ 🟡 部分修复

**位置**：`cmd/api-server/workspace_cache.go:151-157`（`scheduleRebuild`）、`:348-358`（`cacheRebuildDelay`）

**已做的**：`docs/api_manual.md:13` 现在明确写出：

> `WORKSPACE_CACHE_REBUILD_MS`：Informer 事件到本地索引重建的防抖窗口毫秒数（默认 500，即列表数据最大可见延迟约 0.5 秒，另有 1 分钟周期性 resync 兜底自愈）

**仍缺的**：创建/更新成功后没有 `workspaceCache.upsert(ws)`。Portal 的 `refreshWorkspaceDirectory(true)`（`portal.html:1184,1192,1920,1966,2039`）在创建返回后立即拉列表，仍有概率看不到刚创建的条目。

**修复建议**：在 create/wakeup handler 写 CR 成功后顺手 `workspaceCache.upsert(ws)`（或对该 key 触发一次立即重建），把常见路径的可见延迟降到 0。改前端时机只是回避问题。

---

### 🟢 P3-2：`/healthz` 就绪门禁没有消费者　→ ✅ 已修复

`deploy/api-server-deploy.yaml:63-81` 已补：

- `readinessProbe` → `/healthz:3000`（缓存同步后才放流量）；
- `livenessProbe` → `/health:3000`（不依赖缓存状态，避免缓存重同步或 API 短暂不可用时误杀进程）。

同时补了 `resources.requests/limits`。两个端点确实存在（`cmd/api-server/main.go:65`、`:70`），端口 3000 在容器里已声明（`api-server-deploy.yaml:51-52`）。这版取舍（liveness 不走缓存门禁）比原建议更稳妥。

---

### 🟢 P3-3：`rebuildNow` 与定时器 rebuild 存在旧快照覆盖窗口　→ ❌ 未修复

**位置**：`cmd/api-server/workspace_cache.go:162-186`

```go
func (c *workspaceCache) rebuild() {
    c.mu.Lock()
    c.timer = nil
    items := ... // snapshot pending
    c.mu.Unlock()
    snap := buildSnapshot(items)
    c.snap.Store(snap)      // 并发调用时可能后写入更旧的快照
    ...
}

func (c *workspaceCache) rebuildNow() {
    c.mu.Lock(); c.timer.Stop(); c.timer = nil; c.mu.Unlock()
    c.rebuild()             // 与已触发的 timer rebuild 并发
}
```

两个 `rebuild()` 并发时，先读到 `pending` 的那个可能后执行 `Store`，把旧快照覆盖到新快照上；直到下一个事件或 1 分钟 resync 才纠正。

**修复建议**：加 `building` 标志（在锁内抢占，抢不到就 return），或用单 goroutine + channel 串行化重建，保证快照单调更新。

---

## 2. Task 2.1：NetworkPolicy 收敛为命名空间级模板

### 🟠 P1-2：Ingress 从"按端口放行"退化为"全部放行"　→ ✅ 方案 B 已决策接受

**位置**：`internal/controller/workspace_controller.go:1541-1547`

```go
// Ingress stays open for the selected Pods: the application port differs per
// Workspace and the Ingress controller may SNAT or use host networking, so a
// static template cannot pin a port without breaking traffic. The security
// boundary of the template is egress isolation.
Ingress: []networkingv1.NetworkPolicyIngressRule{{}},
```

注释解释得有道理，**但 `IngressRule` 的 `From` 与 `Ports` 都为空，语义是"所有来源、所有端口、所有协议（含 ICMP）"**，而不是注释暗示的"仅放开业务端口"。`docs/api_manual.md:327` 仍写着"按端口放行……其他端口与 ICMP 不放行"，**实现与文档直接矛盾**（见 N-5）。

**修复建议**（仍需在 PR 里给出明确取舍结论）：

- **方案 A（推荐，改动极小且几乎零风险）**：固定 `Protocol: TCP`、不限定端口：

  ```go
  Ingress: []networkingv1.NetworkPolicyIngressRule{{
      Ports: []networkingv1.NetworkPolicyPort{{
          Protocol: ptr.To(corev1.ProtocolTCP),
      }},
  }},
  ```

  这样保留"仅 TCP"的约束，ICMP/UDP 仍被阻断，与文档的"ICMP 不放行"一致；同时不会因端口漂移挡住流量。

- **方案 B**：确实要完全放开入站，则在 PR 描述、`docs/api_manual.md` 与安全评审记录中明写这是**有意的安全属性弱化**及理由。

不要保留"代码全放开、文档说按端口"的状态。

**处置结论（已关闭，方案 B）**：产品/安全侧决定接受完全放开的入站，理由与代码注释一致——各 Workspace 业务端口不同，Ingress controller 可能 SNAT 或 hostNetwork，静态模板按端口收口会误伤流量；模板的安全边界是出站隔离（默认封禁私网/CGNAT/链路本地，集群级 `--netpol-allowed-namespaces` 放行例外）。**有意的安全属性弱化已记录于**：本节、`docs/api_manual.md` 安全附录（入站不限端口与协议，含 ICMP）、以及 CRD 字段描述（`allowedCIDRs` 等已标 Deprecated）。

---

### 🟠 P1-3：per-workspace `networkPolicy` 被静默忽略，API 契约被破坏　→ ✅ 已修复

**位置**
- `internal/controller/workspace_controller.go:1516-1522`（仍只有一条 `log.Info`）
- `api/v1alpha1/workspace_types.go:211-232`（`blockedCIDRs` / `allowedCIDRs` / `allowedNamespaces` **仍无 `// Deprecated:`**）
- `docs/api_manual.md:138,155,327-328`（仍按"生效"描述，见 N-5）

**现状**：只有集群侧日志能看出被忽略；用户侧完全无感知：

```go
if ws.Spec.NetworkPolicy != nil && (...非空...) {
    log.Info("Ignoring per-workspace NetworkPolicy customization because a single shared namespace-level NetworkPolicy template is used; configure controller-wide egress exceptions with --netpol-allowed-namespaces instead",
        "workspace", ws.Name, "namespace", ws.Namespace)
}
```

影响不变：用户配 `allowedCIDRs: ["10.10.20.5/32"]` 访问内部 LLM 网关，升级后断网，且 `kubectl describe workspace` 看不到任何线索。

**修复建议**（Recorder 已存在，`workspace_controller.go:82`，`:701` 起已有用法，直接照抄即可）：

1. 发 Warning Event：

   ```go
   if r.Recorder != nil {
       r.Recorder.Eventf(ws, corev1.EventTypeWarning, "NetworkPolicyOverridden",
           "spec.networkPolicy.{blockedCIDRs,allowedCIDRs,allowedNamespaces} 已被忽略：当前使用命名空间级共享模板，"+
           "请改用 controller 的 --netpol-allowed-namespaces")
   }
   ```

   注意：Event 每个调谐都会打，建议用 `Eventf` 的聚合（同名 reason/message 会被 apiserver 计数合并），或在 WS 上加 annotation 只发一次。

2. 给三个字段加 `// Deprecated:`，`make manifests` 后 CRD 描述里会体现，`kubectl explain` 可见。

**已完成（方案 B）**：上述两点均已落地——`api/v1alpha1/workspace_types.go:217-239` 三个字段均带 `// Deprecated:`，CRD 描述已随 `make manifests` 更新；`docs/api_manual.md` 同步改写（见 N-5）；`reconcileNetworkPolicy` 已发 `Warning / NetworkPolicyOverridden` 事件（`workspace_controller.go:1522-1531`，消息静态以便 apiserver 按 reason+message 聚合，避免每次调谐刷屏），回归测试 `workspace_controller_test.go:318` 断言事件确实发出。

---

### 🟠 P1-4：命名空间模板回收逻辑不可达，等于不回收　→ ✅ 已修复

**当前实现**
- `internal/controller/workspace_controller.go:145` —— NotFound 分支现在真正调用了回收：

  ```go
  if err := r.maybeDeleteSharedNetworkPolicy(ctx, req.Namespace, req.NamespacedName); err != nil {
      log.Error(err, "Failed to clean up shared NetworkPolicy template", "namespace", req.Namespace)
      return ctrl.Result{}, err
  }
  ```

- `:1661` —— 签名改为 `maybeDeleteSharedNetworkPolicy(ctx, namespace string, gone types.NamespacedName) error`，并显式跳过刚删除的那个 Workspace，规避"缓存里还能看到自己"导致的永不回收：

  ```go
  if item.DeletionTimestamp.IsZero() && item.Name != gone.Name {
      return nil
  }
  ```

- `:185` —— `DeletionTimestamp` 分支同样带着 `req.NamespacedName` 调用，两条路径行为一致。

**回归测试**：`internal/controller/workspace_controller_test.go:322`
`"should delete the shared NetworkPolicy after the last Workspace is deleted"`
—— 用独立 `gc-namespace`（避免 shared `default` 命名空间里 fixture 的干扰），先断言 `ContainsFinalizer == false`（证明走的是真实删除路径），再真删 CR + 调 Reconcile，`Eventually` 断言 netpol `IsNotFound`。**原先"手工 AddFinalizer 制造线上不存在的状态"的测试已替换**。

---

### 🟡 P2-2：5000 个 Workspace 并发 Update 同一份 NetworkPolicy　→ ✅ 已修复

**位置**：`internal/controller/workspace_controller.go:1584-1589`

```go
if needsUpdate {
    log.Info("Updating shared NetworkPolicy template due to changes", ...)
    return r.Update(ctx, netpol)     // 仍无冲突容忍
}
```

稳态下 `needsUpdate=false` 没问题；一旦模板漂移（人工改动、升级补 label），30 个并发 worker 会同时 `Update` 同一对象 → 大量 409 → `workspace_reconcile_errors_total` 上涨 + 指数退避。

**修复建议**（任一即可）：
- 改用 `r.Patch(ctx, netpol, client.MergeFrom(before))`；
- 或 `if apierrors.IsConflict(err) { return nil }`（下一个 Workspace 的调谐会再收敛）；
- 更彻底：只让一个代表 Workspace 负责收敛模板（与 P3-6 的方案合并），见 P3-6。

**处置结论（已修复）**：采用第二条——`IsConflict` 时记录日志并返回 nil，不污染 `workspace_reconcile_errors_total`、不触发指数退避；模板变更/删除由 NetworkPolicy watch 重新入队，下一次收敛即完成。与 P3-6（单代表入队）叠加后，冲突窗口从"N 个 worker 同时抢"收敛为"一个 worker 顺序重试"。

---

### 🟢 P3-4：升级会触发全量 Pod 重建　→ ✅ 已修复（文档层面）

**位置**：`internal/controller/workspace_controller.go:2047-2056`

新增的 `app.kubernetes.io/managed-by: workspace-operator` 写进 `Deployment.spec.template.labels`，对存量 Workspace 全部命中 Task 1.2 的 diff → 一次性全量 Recreate。实现本身合理，但**发布计划/变更说明里仍没有这一条**（已 grep `docs/`，仅 TODO 文档的功能描述，无升级影响说明）。配合 Kata 启动耗时会是一次明显抖动。

**修复建议**：在发布说明中写明"本次升级会重建所有 Workspace Pod"，并给出按命名空间灰度或分批重启的操作建议。

**处置结论（已修复）**：新增 `docs/release-notes.md`，写明：全量重建及其原因（pod template 新增 managed-by 标签）、`Recreate` 先停后起的不可用窗口、Kata 冷启动叠加、用 `--max-concurrent-reconciles` 控速的灰度手段、升级期观察项，以及**回滚会触发第二次全量重建**的代价；同版本的可观测性与 NetworkPolicy 语义变化一并记录。

---

### 🟢 P3-5：`managed-by` 标签贴到了 PVC/Ingress 等所有子资源　→ ✅ 已修复

**位置**：`internal/controller/workspace_controller.go:2047-2056`（`buildWorkspaceLabels`）

`buildWorkspaceLabels` 仍被 PVC（`:600`、`:623`）、Deployment（`:1080`）、Service（`:1310`）、Ingress（`:1396`）共用，因此 PVC / Ingress / Service 也带上了 `app.kubernetes.io/managed-by: workspace-operator`。语义上只有 Pod template 需要它（它是共享 NetPol 的 `podSelector` 依据）。

附带问题仍存在：opt-out 只是"代码侧不再加"；用户若在 CR `labels` 里写了同名 key，`maps.Copy(labels, ws.Labels)` 会把它保留回 Pod template，从而绕过 `networkPolicy.disabled` 的退出纳管。

**修复建议**：
- 给 Pod template 单独一个 builder（在 `buildWorkspaceLabels` 结果上叠加 managed-by）；
- opt-out 分支显式 `delete(labels, managedByLabelKey)`，不要依赖"没加"。

**处置结论（已修复）**：两条都落地——`buildWorkspaceLabels` 只产出 app/workspace/oa 等共享标签（PVC/Service/Ingress/Deployment 元数据用），新增 `buildPodLabels` 叠加 managed-by 并只用于 Deployment 的 `spec.template.labels`（创建与收敛两条路径都改）；opt-out 分支显式 `delete(labels, managedByLabelKey)`，因此用户在 CR `labels` 里写同名 key 也无法把标签带回来。回归测试覆盖：子资源无该标签、禁用时不带、用户伪造也不带。

---

### 🟢 P3-6：单个 netpol 事件入队整个命名空间　→ ✅ 已修复

**位置**：`internal/controller/workspace_controller.go:1953-1966`（`mapNetworkPolicyToWorkspaces`）

模板每次变更/删除仍把该命名空间下**所有** Workspace 入队（`requests` 容量就是 `len(wsList.Items)`）。5k 规模下会被并发度 30 限住，但队列深度与排队延迟曲线会出尖刺，并与 P2-2 的冲突问题叠加。

**修复建议**：按名字排序取第一个（或取 `spec.stopped=false` 的第一个）作为唯一代表入队。模板是命名空间级的，一个 Workspace 调谐足够收敛；配合 P2-2 的冲突容忍即可彻底消除冲突风暴。

**处置结论（已修复）**：`mapNetworkPolicyToWorkspaces` 改为跳过 `DeletionTimestamp` 非零的项后，按名字取 `slices.Min` 的代表（确定性选择，多副本行为一致），只返回一个请求；无存活 Workspace 时返回 nil。

---

### 🟢 P3-9：最后一个删除与新建并发时的空窗　→ ❌ 未修复

`maybeDeleteSharedNetworkPolicy`（`:1661-1680`）只要看不到存活 Workspace 就删除；新建 Workspace 的调谐可能还没跑到 `ensureSharedNetworkPolicy`，出现短暂"无策略"窗口（默认拒绝语义下，这个窗口里 Pod 网络会短暂放开）。频率极低，记录在案即可；若要收口，可在删除前做一次短延时二次确认，或让新建路径先建模板再加 label。

---

## 3. Task 5.2：Operator 可观测性指标

### 🟠 P1-5：Grafana 看板 3 处指标名/标签错误，面板必然是空白　→ ✅ 已修复

**当前表达式**（`deploy/grafana/dashboards/workspace-operator.json`）：

| 行 | 表达式 | 对照 controller-runtime v0.24.1 |
| --- | --- | --- |
| `:480` | `sum(workqueue_depth{name="workspace"})` | ✅（priority queue 下按 `name` 聚合掉 `priority`） |
| `:489` | `sum(rate(workqueue_adds_total{name="workspace"}[$__rate_interval]))` | ✅ |
| `:607` | `max(workqueue_longest_running_processor_seconds{name="workspace"})` | ✅（原本就正确，保留） |
| `:616` | `max(controller_runtime_max_concurrent_reconciles{controller="workspace"})` | ✅（标签是 `controller`，已修正） |

`deploy/grafana/README.md` 的指标表也重写为"**实际名称与标签并不统一，不要加 `controller_runtime_` 前缀**"，并单独标注 `controller_runtime_max_concurrent_reconciles` 只有 `controller`、没有 `name`。

---

### 🟠 P1-6：指标抓取链路未部署，验收标准不成立　→ ✅ 已修复（采纳"方案 4：文档化替代抓取方式"）

**策略变更（而非按原建议发 ServiceMonitor）**：改为**明文 HTTP 8080 + 文档化抓取配置**，明确不交付 prometheus-operator CRD 依赖。

已落地的改动：

| 文件 | 变更 |
| --- | --- |
| `config/default/manager_metrics_patch.yaml` | 新增 `--metrics-bind-address=:8080`、`--metrics-secure=false`（含安全取舍说明） |
| `config/default/metrics_service.yaml` | 端口 `https`/8443 → `http`/8080 |
| `config/network-policy/allow-metrics-traffic.yaml` | 8443 → 8080（含注释） |
| `config/manager/kustomization.yaml` | 接入上述 patch |
| `deploy/install.yaml:882-883`、`:845-852` | args 与 Service 端口同步（`name: http`、`port: 8080`） |
| `deploy/grafana/README.md` | 重写：写明明文 HTTP、无 Bearer/无 TLS、给出 `kubernetes_sd_configs` 抓取示例、明确"ServiceMonitor **没有**随安装包发布"、标注恢复安全模式时需补什么 |
| `test/e2e/e2e_test.go:226` | 改为 `curl http://<svc>:8080/metrics`，不再取 SA token、不再 `-k` |

这条验收标准（"Prometheus 能抓到指标"）现在由**部署文档**兑现，且 e2e 用真实 HTTP 抓取做了冒烟。

**但该策略引入了 3 个新的落地缺口（N-1 / N-2 / N-3），必须一并处理，否则文档里的抓取配方仍然跑不通。**

---

### 🟡 P2-3：`workspacePhases.publish()` 是 O(N)，让调谐循环变成 O(N²)　→ ✅ 已修复

**位置**：`internal/controller/metrics.go:128-152`（`set`/`unset`/`resetAll`）→ `publish():162-176`

三个入口仍是无条件全量重算：

```go
func (t *workspacePhaseTracker) set(key types.NamespacedName, phase aiv1alpha1.WorkspacePhase) {
    t.mu.Lock(); defer t.mu.Unlock()
    t.phases[key] = phase
    t.publish()                 // 遍历全部记录 + Set 7 个 gauge
}
```

5000 个 Workspace 跑一轮调谐 = 2500 万次 map 迭代，且 30 个 worker 在**调谐收尾处**被同一把全局锁串行化。这与"万级优化"的目标方向相反。

**修复建议**：改为增量维护（无变化直接 return；有变化则旧 phase `Dec()`、新 phase `Inc()`），`publish()` 只保留给注册与 `resetAll()` 这类低频路径。可加 `BenchmarkPhaseTrackerSet`（5000 条）验证 O(1)。

**处置结论（已修复）**：`set`/`unset` 改为对旧/新 phase 直接 `Dec()`/`Inc()`，phase 未变化时直接返回；`resetAll` 保留全量重算（周期 resync 与测试），`publish()` 移除、注册时改用 `zeroWorkspacePhaseSeries()`。新增 `BenchmarkWorkspacePhaseTrackerSet`：5000 个跟踪项下 **122 ns/op**，与 N 无关。

---

### 🟡 P2-4：自建 queue latency 与内置指标重复，且给热路径加了全局锁　→ ✅ 已修复

**位置**：`internal/controller/metrics.go:181-280`（`newInstrumentedWorkspaceQueue` / `instrumentedWorkspaceQueue`），接线仍在 `internal/controller/workspace_controller.go:1847`：

```go
controllerOptions.NewQueue = newInstrumentedWorkspaceQueue
```

`workqueue_queue_duration_seconds{name,controller}` 的语义与自定义 `workspace_queue_latency_seconds` **完全一致**（进入 ready 树 → 被 worker 取走，均不含 rate-limited 重试），且内置指标自带 `controller` label、无额外锁。而包装器让每次 `Add`/`Get` 都过一遍 `q.mu`（`:232-280`），在事件风暴下是真实争用点。

**修复建议**：优先删掉包装器与 `NewQueue` 注入，看板/告警改查内置指标（P1-5 已改用内置指标，删掉不影响看板）。若确需业务口径，至少用分片锁/`sync.Map` 并给 histogram 加 `controller` label。

**处置结论（已修复，选删除）**：`metrics.go` 删除 `workspaceQueueLatency`、`newInstrumentedWorkspaceQueue`、`instrumentedWorkspaceQueue` 与 `nonNegativeDuration`；`workspace_controller.go` 删除 `controllerOptions.NewQueue` 注入，恢复 controller-runtime 默认队列（内置指标自带 `controller="workspace"` 标签、无需额外锁）。看板 3 个 P50/P95/P99 面板与 README 指标表/告警 PromQL 同步改为 `workqueue_queue_duration_seconds{controller="workspace"}`；对应的 4 个 wrapper 单测删除。

---

### 🟢 P3-7：HA 多副本时 gauge 重复计数　→ ❌ 未修复

`workspace_active_instances` 只在 leader 的 `Start()`（`workspace_controller.go:1876`）里 resync。非 leader 也暴露 `/metrics`。当前 `replicas: 1`（`config/manager/manager.yaml:24`、`deploy/install.yaml:867`）所以不发作，但 `--leader-elect` 已开启（`install.yaml:884`）；leader 切换后旧 leader 保留最后值，`sum()` 会翻倍。建议给 gauge 加 `leader` 维度，或在失权时清零。

---

### 🟢 P3-8：其他小项　→ ✅ 已修复

1. **`ActiveInstancesSyncInterval` 没有对应 flag**：`cmd/main.go:90-94` 已为 `--max-concurrent-reconciles`、`--netpol-allowed-namespaces` 暴露参数，建议同样加 `--active-instances-sync-interval`。
2. **`Reconcile` 包装器把 NotFound 快速返回也计入 duration 直方图**（`workspace_controller.go:115-126`）：删除事件的空调谐会污染 P50/P99 分辨率。建议跳过 `IsNotFound` 路径（同时不计入 `workspace_reconcile_errors_total`）。
3. **`workspace_reconcile_errors_total` 与内置 `controller_runtime_reconcile_errors_total{controller="workspace"}` 重复**，考虑二选一。
4. **`Failed` phase 恒为 0**：`allWorkspaceMetricPhases`（`metrics.go:83-90`）包含 `WorkspaceFailed`，但全仓只有**读**（`workspace_controller.go:365`、`cmd/api-server/main.go:694`），没有任何**赋值**。看板会多一条永远为 0 的线。要么补上 Failed 的赋值，要么从列表移除。

**处置结论（四项全部修复）**：
1. 新增 `--active-instances-sync-interval`（`flag.DurationVar`，0 表示使用内置默认 1m），并在 `WorkspaceReconciler` 构造处接线；
2. `reconcile` 对 NotFound 返回内部哨兵 `errWorkspaceGone`，`Reconcile` 包装器识别后直接返回且**不 Observe 直方图**（删除事件的空调谐不再污染 P50/P99）；
3. 删除自建 `workspace_reconcile_errors_total`，看板/README/告警统一改用内置 `controller_runtime_reconcile_errors_total{controller="workspace"}`；
4. `Failed` 移出 `allWorkspaceMetricPhases`（controller 从不赋值，发布恒 0 序列会误导），看板移除该颜色 override，README 注明原因。

---

## 4. 本轮新发现的问题

### 🟠 N-2：`deploy/grafana/README.md` 的 Pod 级抓取配方实际抓不到 8080

**位置**：`deploy/grafana/README.md:22-32`

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
```

**问题**：manager 容器的 `ports` 只有 `8081/health`（`config/manager/manager.yaml:68-71`，`deploy/install.yaml:890-892`），**没有声明 8080**。`role: pod` 的默认 target 地址取自容器的第一个声明端口，因此这个 job 会去打 `<podIP>:8081`，而 8081 是 health probe 端口，`/metrics` 在那里 404。同时没有任何 `__meta_kubernetes_pod_container_port_number` 的 relabel，无法把端口改到 8080。

**修复建议**（任一）：
- **推荐**：改用 Service 发现，直接命中已存在的 `agent-deploy-controller-manager-metrics-service`（`install.yaml:845-855`，端口名 `http`、8080）：
  ```yaml
  kubernetes_sd_configs:
    - role: endpoints
      namespaces: {names: [agent-deploy-system]}
  relabel_configs:
    - source_labels: [__meta_kubernetes_endpoint_port_name]
      regex: http
      action: keep
  ```
- 或在 `config/manager/manager.yaml` 的容器里补 `- containerPort: 8080 name: metrics`（`deploy/install.yaml` 由 `make build-installer` 重生成），并把 README 配方改成按 `container`/`port_name` 过滤。

两种都能修，但**必须与文档二选一保持一致**，否则 README 的抓取示例是错的。

---

### 🟠 N-3：安装包里没有任何 NetworkPolicy，抓取可能被默认拒绝拦掉

**位置**：`config/default/kustomization.yaml:34`（`#- ../network-policy` 仍注释）、`deploy/install.yaml`（`kind: NetworkPolicy` 出现 0 次）

**问题**：`config/network-policy/allow-metrics-traffic.yaml` 已改到 8080，但它没有被 Kustomize 引入，也就没有进安装包。若目标集群对 `agent-deploy-system` 施加了默认拒绝（或装了默认 deny-all 的 CNI 策略），Prometheus 侧即使按 N-2 修好配方，也连不上 8080。

**处置结论（已关闭）**：部署前提已确认——**不会有任何 NetworkPolicy 装入 `agent-deploy-system`**，故本项不按"启用 `- ../network-policy`"处理，策略模板保持不交付（NetworkPolicy 是叠加白名单，无策略选中即全放行，抓取当前是通的）。同时把前提与**反向坑**写入会被误改的位置：`config/default/kustomization.yaml` 注释明确说明"不要直接放开注释——模板只放行打了 `metrics: enabled` 标签的命名空间，放开而不打标反而会打断现有抓取"，`deploy/grafana/README.md` 抓取配置一节同步记载。若前提未来被打破，按注释里的步骤启用组件 + 打标 + 重建 `deploy/install.yaml`。

---

### 🟢 N-1：`config/prometheus/monitor.yaml` 已与实现脱节

**位置**：`config/prometheus/monitor.yaml:13-20`、`config/prometheus/monitor_tls_patch.yaml`

Monitor 仍是旧的 secure 配置：

```yaml
- path: /metrics
  port: https          # ← Service 端口现在叫 http
  scheme: https        # ← Manager 现在是明文 HTTP
  bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token   # ← 已不校验 token
  tlsConfig:
    insecureSkipVerify: true
```

同时 `config/prometheus/kustomization.yaml` 的 cert-manager patch、`monitor_tls_patch.yaml` 也都假设存在 TLS 服务端与 `metrics-server-cert` Secret。

**当前不会发作**，因为 `config/default/kustomization.yaml:27` 的 `#- ../prometheus` 保持注释。但这是一颗定时炸弹：任何人按 README 的旧印象启用它，都会得到一个 `port "https" not found` 的 ServiceMonitor（端口名现在是 `http`）。

**修复建议**：把 `monitor.yaml` 改成明文口径（`port: http`、`scheme: http`，删掉 `bearerTokenFile` 与 `tlsConfig`），把 TLS 版本整体挪到 `monitor_tls_patch.yaml` 并在文件头注明"仅在恢复 `--metrics-secure=true` 时使用"。否则就删掉这两个文件，避免误导。

---

### 🟢 N-4：e2e 的 `serviceAccountToken()` 已成死代码

**位置**：`test/e2e/e2e_test.go:282-285`

`serviceAccountToken()` 只被定义、无任何调用（`grep -rn serviceAccountToken test/` 仅命中定义处）。`go vet` 不会报未使用的函数，所以 CI 是绿的。metrics 抓取已改为明文 HTTP 后，取 token 的整套逻辑（TokenRequest 文件、`kubectl create -f`、解析 base64）都不需要了。

**修复建议**：删除该函数及其遗留的 `/tmp` 临时文件逻辑，减少 e2e 里与 metrics 无关的噪音。

---

### 🟠 N-5：`docs/api_manual.md` 仍宣称 per-workspace 策略生效

**位置**：`docs/api_manual.md:138`、`:155`、`:327-328`

- `:138` —— `networkPolicy` 字段仍描述为"**工作空间专属**网络安全策略配置……包含 `blockedCIDRs`（追加禁止网段）、`allowedCIDRs`（精准白名单放行）……"，读者会认为它生效；
- `:155` —— 请求体示例里 `"networkPolicy": {...}` 仍在；
- `:327` —— "**入站（Ingress）按端口放行**：……其他端口与 ICMP 不放行"，与 `Ingress: [{}]` 直接矛盾。

这是 P1-2 / P1-3 的文档侧后果。**代码与文档必须同时改**，否则安全评审结论与用户自建配置都会基于错误前提。

**处置结论（已关闭，方案 B）**：`docs/api_manual.md` 已按最终选定的方案 B 改写——`:138` 明确"仅 `disabled` 仍生效；`blockedCIDRs`/`allowedCIDRs`/`allowedNamespaces` 已废弃（仍可提交但会被忽略），统一放行用 `--netpol-allowed-namespaces`"；请求体示例删除 `networkPolicy` 块；安全附录改为"入站不限制端口与协议（含 ICMP），安全边界为出站隔离"，并注明这是 Task 2.1 收敛后的有意取舍。API 侧三个字段同步加上 `// Deprecated:`，CRD 描述随 `make manifests` 更新（`kubectl explain` 可见）。

---

### 🟢 N-6：Task 5.2 完成记录缺 metrics 端口/安全模式变更

**位置**：`docs/large_scale_optimization_todo.md:139-142`

完成记录只写了指标实现与看板位置，没有提 `--metrics-bind-address=:8080 --metrics-secure=false` 这一**部署契约变更**。运维照旧文档去 8443 抓 TLS 指标会直接失败。

**修复建议**：在 5.2 的"完成记录"补一句：Manager 默认在 `:8080` 暴露**明文** `/metrics`（`--metrics-secure=false`，安全取舍见 `deploy/grafana/README.md`），Service 端口名 `http`；恢复安全模式的步骤同 README。

---

## 5. 剩余工作与建议顺序

**会直接影响验收，建议本轮内完成**

> N-2、N-3 已在本轮处理完毕：N-2 修复（manager 容器声明 `8080/http`，README 改为端口名 keep）；N-3 按部署前提关闭（`agent-deploy-system` 不安装 NetworkPolicy，前提与反向坑已写入 kustomization 注释与 README），不再列入剩余工作。
> P1-2 / P1-3 / N-5 亦已关闭：P1-2 按方案 B 决策接受，P1-3 补了 Deprecated + Warning Event + 回归测试，N-5 文档已按方案 B 改写。
> 第一梯队已落地：P2-2（冲突容忍）、P3-6（单代表入队）、P2-3（增量 gauge，122ns/op）、P3-4（`docs/release-notes.md`）。
> 第二梯队已落地：P2-4（删除 queue 包装器，看板/README 改用内置指标）、P2-1（三个索引参与最小候选集选路）。

**本轮无遗留的"会直接影响验收"项。**

**可排期**

1. P3-3（重建串行化）、P3-1（upsert-after-create）；
2. N-1（monitor.yaml 对齐或删除）、N-4（删死代码）、N-6（补文档）；
3. P3-7/P3-9（HA gauge、删除/新建空窗）。

---

## 6. 完成标准（Definition of Done）

已满足（复核时验证）：

- [x] `GOWORK=off go build ./...` 通过；
- [x] `go vet ./cmd/... ./internal/...` 与 `go vet -tags e2e ./test/e2e/` 均无问题；
- [x] `go test ./internal/controller ./cmd/api-server -count=1` → **69 passed in 2 packages**；
- [x] `startWorkspaceCache` 在 15s 内返回且缓存已填充（`workspace_cache_test.go:40`）；
- [x] `page` 超大值返回 400 且不 panic（`main_test.go:518,542`）；
- [x] 删除最后一个 Workspace 后 `workspace-shared-netpol` 被回收，走真实删除路径（`workspace_controller_test.go:322`）；
- [x] API-Server Deployment 有 readiness/liveness 探针，`/healthz` 就绪门禁有消费者；
- [x] 看板内 `workqueue_*` / `controller_runtime_max_concurrent_reconciles` 的指标名与标签正确；
- [x] Manager 指标以明文 HTTP 8080 暴露，e2e 能抓通，README 记录了替代抓取方式。

尚未满足：

- [x] **README 的 Pod 级抓取配方能被真实执行**（manager 容器已声明 `8080/http`，配方按端口名 keep，见 N-2）；
- [x] 安装包或文档明确处理 metrics 入站的 NetworkPolicy（见 N-3）——结论：`agent-deploy-system` 不安装任何 NetworkPolicy，模板保持不交付，前提与反向坑已写入 `config/default/kustomization.yaml` 与 `deploy/grafana/README.md`；
- [ ] `config/prometheus/monitor.yaml` 与明文 HTTP 口径一致，或已删除（见 N-1）；
- [x] `docs/api_manual.md` 与实现一致：NetworkPolicy 入站行为、per-workspace 字段已失效（见 N-5、P1-2、P1-3）——已按方案 B 改写，API 三字段已标 Deprecated；
- [x] per-workspace `networkPolicy` 被忽略时发出 `Warning/NetworkPolicyOverridden` 事件，用户 `kubectl describe workspace` 可见（见 P1-3，测试 `workspace_controller_test.go:318`）；
- [ ] `docs/large_scale_optimization_todo.md` 5.2 完成记录包含 metrics 端口/安全模式变更（见 N-6）；
- [x] 发布说明写明"本次升级会重建所有 Workspace Pod"（见 P3-4，`docs/release-notes.md`）；
- [x] `workspace_active_instances` 的 per-reconcile 开销不再随实例数线性增长（见 P2-3，增量 `Inc/Dec`，benchmark 122ns/op）；
- [x] 无自建 queue 包装器，且排队延迟改用内置 `workqueue_queue_duration_seconds{controller="workspace"}`（见 P2-4）；
