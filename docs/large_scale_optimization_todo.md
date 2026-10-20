# 万级 Workspace 高并发架构演进与优化待办清单 (Scalability TODO)

> **目标**：支撑 **3,000 ~ 5,000+** 个 Workspace 实例稳定在线运行，解决 Controller 调谐延迟、Ingress Reload 洪暴、K8s 对象膨胀、存储 I/O 锁死及 API-Server 阻塞等系统性瓶颈。

---

## 一、现状与核心瓶颈总结

| 维度 | 当前实现缺陷 | 万级并发下危害 |
| :--- | :--- | :--- |
| **控制器 (Operator)** | 单 Worker 串行调谐；每次调谐无条件执行 4 次 `r.Update`；无索引全局扫描 PodList | 调谐严重滞后排队（单轮排队数分钟）；API Server 遭遇写放大与 APF 限流；级联触发事件风暴 |
| **网络与路由** | 每个 Workspace 独立维护 1 个 Ingress | 5,000 个 Ingress 导致 Nginx 配置文件膨胀数十 MB，频繁变更引发无休止的 `nginx -s reload`，长连接（WebIDE WebSocket）频繁断连 |
| **K8s 控制面** | 1 个 Workspace 对应 7~9 个关联资源（Deployment, RS, Pod, Svc, Ingress, NetPol, PVC 等） | 集群对象数膨胀至 3.5 万 ~ 4.5 万个，etcd 内存剧增，CoreDNS 与 Kube-proxy iptables 链饱和 |
| **底层存储** | 单点 NFS 服务端或单 Bucket OSSFS 直挂 | 5,000 个活跃开发容器高频文件监控（inotify/git/编译）压垮 NFS 锁与 RPC；打穿 OSS 单 Bucket QPS 导致 503 SlowDown |
| **API-Server** | `GET /api/workspaces` 内存全量 List；创建/唤醒时 90 秒同步阻塞轮询 + Ingress 探活 | 管理后台高频查询导致 API-Server 内存 OOM；早高峰数百人并发唤醒产生海量轮询探活流量，雪崩故障 |

---

## 二、待办任务清单 (按优先级划分)

### 🔴 阶段一：紧急核心性能与稳定性攻坚 (P0 - 阻断级问题)

- [x] **Task 1.1：控制器开启高并发调谐与客户端限流调优**
  - **位置**：[`cmd/main.go`](file:///Users/yfsun/mywork/myproject/agent-deploy/cmd/main.go)、[`internal/controller/workspace_controller.go`](file:///Users/yfsun/mywork/myproject/agent-deploy/internal/controller/workspace_controller.go)
  - **内容**：
    - 在 `restConfig` 中将默认 QPS 提升至 `300~500`，Burst 提升至 `600~1000`（支持命令行参数 `--kube-api-qps` 与 `--kube-api-burst` 自定义）。
    - 在 `SetupWithManager` 中通过 `WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent})` 开启并发调谐（支持 `--max-concurrent-reconciles` 传参，默认 30 并发）。
  - **验收标准**：5,000 个 Workspace 并发触发调谐时无 Client-side Throttling 日志，队列平均延迟降至毫秒级。

- [x] **Task 1.2：实施精准 Diff 检查，杜绝无意义 Update**
  - **位置**：`internal/controller/workspace_controller.go` (`reconcileDeployment`, `reconcileService`, `reconcileIngress`, `reconcileNetworkPolicy`)
  - **内容**：
    - 针对所有子资源更新前增加 Diff 比较（对比 Spec、Labels、Annotations、Probes、VolumeMounts 等），仅在实际发生实质变更时才调用 `r.Update()`。
  - **验收标准**：稳态下（无状态变化的已就绪 Workspace）调谐期间向 API Server 发起的写请求为 0。

- [x] **Task 1.3：引入 Predicates 机制过滤无关事件**
  - **位置**：`internal/controller/workspace_controller.go`
  - **内容**：
    - 自定义 `workspacePredicate`，拦截控制器自更新 Status（Phase, PodName, Endpoint 等）引发的死循环事件，精准放行 Spec/Labels/Finalizers/LastActiveTime/DeletionTimestamp 变动。
    - 自定义 `deploymentPredicate`，仅在 ReadyReplicas 变动、Generation 变动或删除时触发 Workspace 调谐。
    - 为 Service、Ingress、NetworkPolicy 挂载 `predicate.GenerationChangedPredicate{}`。
  - **验收标准**：更新 Status 不再触发级联 Reconcile 循环。

- [ ] **Task 1.4：路由架构改造：废除独立 Ingress，收敛为泛域名 / 动态网关**
  - **位置**：`internal/controller/workspace_controller.go`、`config/samples/workspace-unified-api-ingress.yaml`
  - **内容**：
    - 废除为每个 Workspace 单独创建 Ingress 的模式（停止 `reconcileIngress` 生成个体 Ingress）。
    - 部署集群级单一统一网关（泛域名通配 Ingress `*.workspace.domain`，或 Envoy/OpenResty/Gateway API）。
    - 网关基于请求 Host 规则或 Cookie/Header 中的身份信息进行动态反向代理。
  - **验收标准**：创建/删除/休眠 Workspace 过程中，Ingress Controller reload 频率降为 0。

- [x] **Task 1.5：引入 FieldIndexer 消除 PodList 线性扫描**
  - **位置**：`internal/controller/workspace_controller.go`
  - **内容**：
    - 注册 Pod 的 `metadata.workspace` 字段索引（O(1) 索引定位）。
    - 在 `Reconcile` 中通过 `client.MatchingFields{"metadata.workspace": ws.Name}` 精准获取 Workspace 关联 Pod，并保留针对测试环境的优雅降级。
  - **验收标准**：数千 Pod 下单次调谐 CPU 占用降低 80% 以上。

---

### 🟡 阶段二：资源模型轻量化与控制面减负 (P1)

- [ ] **Task 2.1：收敛 NetworkPolicy 为命名空间级通用模板策略**
  - **位置**：`internal/controller/workspace_controller.go` (`reconcileNetworkPolicy`)
  - **内容**：
    - 停止每个 Workspace 生成独立的 `ws-xxx-netpol`。
    - 改为在租户命名空间下创建全局静态 NetPol 模板（使用 `podSelector: matchLabels: app.kubernetes.io/managed-by: workspace-operator` 统一纳管所有容器）。
  - **验收标准**：全集群 NetworkPolicy 数量从 5,000+ 缩减至个位数，节点内核 iptables/ipset 规则数减少 99%。

- [ ] **Task 2.2：评估直接编排 Pod（跳过 Deployment + ReplicaSet）**
  - **位置**：`internal/controller/workspace_controller.go`
  - **内容**：
    - 评估单实例交互式开发工作空间直接使用裸 Pod（搭配 Operator 自行保活与状态同步）的可行性。
    - 若可行，可省去 5,000 个 Deployment + 5,000 个 ReplicaSet 的对象管理开销。
  - **验收标准**：输出架构评估方案与原型验证对比。

- [ ] **Task 2.3：宿主机网络转发确认与优化**
  - **内容**：
    - 确保集群 Kube-proxy 运行在 `IPVS` 模式（严禁在数千 Service 下使用传统 iptables 模式）。
    - 评估落地 Cilium eBPF Host-Routing 方案，彻底绕过内核连接跟踪与 iptables。

---

### 🟡 阶段三：底层存储并发升级与分层加速 (P1)

- [ ] **Task 3.1：废弃单机 NFS，迁移至分布式共享文件存储**
  - **位置**：`deploy/nfs/`、`deploy-ack/storageclass.yaml`
  - **内容**：
    - 废弃单节点 `nfs-client-provisioner`。
    - 生产迁移至云厂商全托管分布式存储（如阿里云 NAS 极速型 / CPFS）或开源 CephFS / GlusterFS。
  - **验收标准**：支撑 5,000 客户端并发读写无 RPC 挂起与元数据锁死。

- [ ] **Task 3.2：引入 JuiceFS 优化 OSS 挂载性能**
  - **位置**：`deploy/oss/`、`deploy-ack/storageclass-ossfs2.yaml`
  - **内容**：
    - 针对 OSS 存储方案，使用 **JuiceFS**（Redis/TiKV 存储元数据，OSS 存储数据块）替代传统单 Pod FUSE 直挂。
    - 开启工作空间节点本地 Cache（SSD 缓存加速工作区依赖如 node_modules、pip cache）。
  - **验收标准**：规避 OSS 原生小文件元数据瓶颈，杜绝 OSS Bucket 503 SlowDown 告警。

---

### 🟡 阶段四：API-Server 网关异步化与性能增强 (P1)

- [ ] **Task 4.1：工作空间启动/唤醒接口异步化 (202 Accepted + 状态通知)**
  - **位置**：[`cmd/api-server/main.go`](file:///Users/yfsun/mywork/myproject/agent-deploy/cmd/api-server/main.go)
  - **内容**：
    - 废除当前在 HTTP Request 内部使用 `pollWorkspaceRunning` 同步阻塞 90 秒的逻辑。
    - 接口创建/唤醒 CR 成功后直接返回 `202 Accepted`。
    - 前端通过 WebSocket 或 SSE（Server-Sent Events）监听工作空间状态流变。
  - **验收标准**：API-Server 启动/唤醒接口 P99 响应时间从 10~30 秒降至 50 毫秒以内。

- [ ] **Task 4.2：列表查询增加分页与本地 Informer 缓存**
  - **位置**：[`cmd/api-server/main.go`](file:///Users/yfsun/mywork/myproject/agent-deploy/cmd/api-server/main.go#L160-L196) (`listWorkspacesHandler`)
  - **内容**：
    - 增加分页支持（`page`, `pageSize`）与标签筛选（Owner/Namespace）。
    - API-Server 接入本地 Informer 内存缓存（或 Redis 同步缓存），严禁直接直连 K8s API Server 进行无分页全量 List。
  - **验收标准**：`GET /api/workspaces` 在 5,000 实例规模下吞吐量达到 1,000+ QPS，内存稳定不漂移。

---

### 🟢 阶段五：大规模仿真压测与可观测性建设 (P2)

- [ ] **Task 5.1：搭建万级轻量仿真压测环境（基于 KWOK）**
  - **内容**：
    - 使用 [KWOK (Kubernetes WithOut Kubelet)](https://kwok.sigs.k8s.io/) 模拟 100+ 虚拟节点和 5,000+ 虚拟工作空间 Pod。
    - 验证控制器在 5,000 实例并发创建、休眠、删除全生命周期下的 CPU/内存曲线与调谐延迟。

- [ ] **Task 5.2：完善 Operator 核心可观测性指标**
  - **位置**：`internal/controller/workspace_controller.go`
  - **内容**：
    - 暴露并打通 Prometheus 指标：
      - `workspace_reconcile_duration_seconds`（调谐耗时直方图）
      - `workspace_reconcile_errors_total`（调谐失败计数）
      - `workspace_queue_latency_seconds`（工作队列排队延迟）
      - `workspace_active_instances`（按 Phase 统计在线实例数）
  - **验收标准**：Grafana 看板清晰呈现大促/早高峰期间调谐引擎水位。
