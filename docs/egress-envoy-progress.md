# 统一出站白名单（Envoy）：开发记录

当前为未完成实现，不可部署为正式网络安全边界。

## 已实现

- internal/egress：配置模型、严格解码、校验、ConfigMap 持久化、乐观并发控制。
- /api/egress/config GET/PUT：沿用可选登录；不修改普通 Workspace API。
- 现有 Portal 内的出站白名单页，分开维护 IP、域名、基础服务。
- 固定 Envoy bootstrap 与单文件 RDS 生成器；显式 CONNECT 和默认本地拒绝。
- 动态文件同目录原子 rename；读取 admin config_dump 中命名 RDS 的加载版本。
- 单元测试：非法规则、空白名单、鉴权、并发冲突、目标端口、默认拒绝、加载状态。

## 已确认的设计约束

不合并旧实现。旧分支改动保留在 stash；基线 505d437。
所有授权来自统一配置，不设置企业内网默认允许，也不额外增加业务网段授权。
网络基础设施地址的识别仅用于拒绝与保护，不作为额外允许来源。
域名由 Envoy 解析，Operator 不解析业务域名生成 IP 规则。

首次迁移等待自然休眠：运行中旧 Pod 不改模板，不主动唤醒。
休眠至零后准备模板，下次正常唤醒前准备严格策略。
目前控制器按 lastActiveTime + idleTimeout 缩容；不是 Ingress 流量空闲检测。
页面必须显示旧 Pod 待迁移，不能在过渡期声称全部受管 Pod 已生效。
不能简单恢复 managed-by 标签：必须处理旧宽泛策略叠加及保留入站行为。

## 尚未完成（发布阻断项）

1. 同步进程：ConfigMap watch、校验、原子替换、超时和加载错误、各副本心跳。
2. API 状态聚合和页面：保存/同步中/各副本加载/失败，迁移明细。
3. Controller：严格出站规则、配置 watch、自然唤醒迁移、旧策略隔离。
4. Envoy 出站防护：DNS 指向 Workspace、节点、元数据及 loopback 的绕过防护。
   不能仅以域名匹配替代目标地址防护；普通 NetworkPolicy 对 loopback 不足。
5. Envoy 固定镜像 digest、同步器镜像、部署/RBAC、离线分发及许可证/SBOM清单。
6. 真实 Envoy validate、HTTP/CONNECT、流式与长连接、DNS、多副本、故障验证。
7. 隔离 Kind + Calico 验证，以及发布和回退步骤；不使用现场集群测试。
8. 前端浏览器验证；目前仅通过 TypeScript/Vite 构建。

当前 API 返回 saved 仅代表持久化，页面明确尚未确认实际加载。
当前部署文件未增加白名单 RBAC，避免把未完成数据面作为可发布功能。

## 官方资料

- 文件动态更新要求文件替换而非原地编辑：
  https://www.envoyproxy.io/docs/envoy/latest/start/quick-start/configuration-dynamic-filesystem
- DFP confused deputy 与 DNS 指向受保护地址风险：
  https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/dynamic_forward_proxy_filter
- CONNECT 匹配及终止：
  https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/http/upgrades
- 版本候选为 1.40.2，尚未拉取/验证或固定 digest：
  https://www.envoyproxy.io/docs/envoy/v1.40.2/version_history/v1.40/v1.40.2

## 本地验证

Go: go test ./internal/egress ./cmd/api-server
前端: npm --prefix web/portal run build
格式: git diff --check
Docker Desktop 已尝试启动，但 docker version 尚未成功返回；不能声称容器验证通过。

## 审查修复进展（2026-11-05）

已修改并通过定向回归：
- deploy 和 deploy-ack 新增 namespace Role/RoleBinding，支持配置保存和副本状态读取；真实 ServiceAccount 授权验证待完成。
- 停止/自动休眠的缩容路径先于迁移预检，配置缺失不会阻断缩容，且不改变 Pod 模板。
- 迁移要求相同 DNS 目标显式配置 TCP/UDP 53；API/页面返回缺失告警，仍允许保存空业务白名单。
- 迁移要求 Envoy Deployment 存在、非零副本、所有活跃副本已加载当前版本；未就绪不切换网络策略。
- NO_PROXY 补自身 Pod IP（Downward API）和标准集群 Service 域名后缀，仅影响路由选择，不增加网络授权。
- 允许的 HTTP/CONNECT 路由关闭总请求及流空闲超时；保留 10 秒请求头超时。资源限制和长连接压测待完成。

仍然阻断发布：代理目标地址防护、通用 IP 直连客户端兼容性、完整 Envoy/sync 部署与镜像/SBOM、Calico 集成测试、多副本和迁移状态展示。不能把本次修复称为全部审查项已关闭。
