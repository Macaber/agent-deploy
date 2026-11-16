# 下一会话交接与投产准备（2026-11-11）

## 先读与实际工作区

实际仓库 `D:\E\Code\TS\agent-deploy`，分支 `feature-xjw-networkpolicy-envoy`。不要使用 C 盘同名旧副本。先读根目录 AGENTS.md、本文件、docs/egress-envoy-handover.md，检查 git status。不要 reset/clean，保留 .tmp 实验和 stash。

已提交：ae740ca 为原统一出站实现；7e7d64b 为共用镜像、批量录入、IPv4/CIDR 代理支持。已合入当时 origin/main 的 725dcd6 和 d72aa5b。是否有新的远端提交需重新 fetch 核对。没有确认已推送远端。

## 用户最新要求（待实现，不可声称已有）

用户希望首次投产不因漏配规则影响业务：
1. 能恢复原有默认网络策略。
2. 能临时全部出站开放。
3. 有可操作的投产手册，说明构建、导入、升级、生效检查、回退。
用户不坚持特殊值表达方式，明确表示“提供两个按钮也行，主要是能实现功能”。建议页面提供“恢复原有默认策略”“全部出站开放”，并提供“启用白名单”切回。

本次仅完成代码路径梳理与交接文档；三种模式尚未实现。不要把下面设计当作运行代码，更不能把现有 egress-unified-20261110-ip 镜像说成已包含模式切换。

## 建议语义与必须解决的问题

- 显式 mode: legacy / allowlist / unrestricted，避免使用 0.0.0.0/0 等魔法值。已有不含 mode 的配置必须兼容为 allowlist；不能把升级已有白名单变成放开。
- 首次安装未配置可保持原有默认策略；配置删除、读取失败与首次安装应能区分，特别是已有迁移 Pod 时不能静默切到开放。需要持久证据/标记及测试。
- 原有默认策略不是“全部放行”，应复用 buildSharedEgressRules 和原来的 managed-by/disabled 语义，保留控制器全局基础服务例外。
- unrestricted 要定义为 Workspace 全部出站（IP/域名/端口/协议），不改变入站。不能仅在 Envoy 增加通配路由：Envoy 仍有 IPv4、HTTP/CONNECT 和 guard 保护限制。
- 运行中的 Pod 环境变量不能热更新。仅改 NetworkPolicy 或 EGRESS_ENABLED=false 不会清除 HTTP_PROXY，不能称为完整回退。优先设计显式切换、保留当前运行 Pod、显示“等待休眠/唤醒完成切换”；用户通过管理页面休眠，确认旧 Pod 消失，再唤醒。若采用即时重建，必须明确影响并取得相应操作授权。
- 切换期间要处理新旧 Pod 并存、共享策略取并集、migration 标签、已 disabled Workspace、终止中 Pod。不得只改全局共享策略而忽略不同模式的 Pod。
- 恢复默认/开放后，清理我们注入的代理变量，同时保留用户原本的环境配置；不能无差别删除用户自配代理。
- 从开放切回白名单必须收回开放策略，再通过 DNS、保护范围、代理实际加载、叠加策略检查，不得遗留 allow-all。
- API 必须显示目标模式与实际生效/待迁移状态；不能继续把所有模式都依赖代理 loaded，也不能把保存成功说成已切换。
- 模式切换应保留已有白名单内容，支持以后恢复，不覆盖管理员的其他策略。

## 代码入口

- internal/egress/config.go/store.go：配置结构、校验、版本、乐观锁；新增 mode 须影响版本且兼容历史配置。
- internal/egress/policy.go：当前只选 envoy-v1 Pod 的严格策略。
- internal/controller/egress.go：prepareUnifiedEgress、代理注入、迁移门槛和 ConfigMap watch。
- internal/controller/workspace_controller.go：reconcileNetworkPolicy、ensureSharedNetworkPolicy、buildSharedEgressRules、buildPodLabels；停止副本路径会提前返回，不能假定模式协调总会运行。
- internal/egress/envoy.go、cmd/egress-sync/main.go：路由和实际加载报告；现阶段是白名单模式。
- cmd/api-server/egress.go、egress_status.go：配置与状态 API。目前 policyState/migrationState 固定 unverified。
- web/portal/src/components/egress-view.tsx：页面及批量录入。

## 已完成与验证边界

- API 镜像同时包含 /api-server 与 /egress-sync，代理 sidecar/init 使用 command 覆盖；不能只换镜像而漏掉启动命令。
- IPv4 TCP IP/CIDR 支持普通 HTTP 和 CONNECT，严格匹配目标 literal 与端口；UDP/IPv6 仅直连。guard 仍限制受保护目的地址。规则删除不保证断开现有隧道。
- 批量文本支持 Tab/空格；追加后需要保存；HTTP 页面剪贴板 API 不可用时支持手动 Ctrl+C/V。
- 相关 Go 单测/vet、前端构建、批量解析测试通过。完整 make lint-fix/test 曾因缺少依赖失败。真实新版 Envoy/guard 隔离联调尚未完成。
- 用户已于 11 月 11 日确认两个 Docker build 成功：workspace-operator:egress-unified-20261110-ip、api-server:egress-unified-20261110-ip。没有现场部署完成证据；不得当成已投产。
- 最近 npm ci EPERM 已定位为项目 Vite 占用 lightningcss.node，停止该 Vite 后 npm ci/build 成功。
- Docker Desktop 曾无响应，恢复后又有 Docker Hub / gcr.io 拉取 EOF；与集群网络白名单无关。

## 现场问题结论

- 浏览器 503 曾来自错误集群路由、以及 Pod 尚未就绪；后续 502 是入口地址问题。Ingress 已迁到 12.244.142.253，旧 .251 连接失败；修改入口后用户确认页面恢复。不要重新覆盖为 .251。
- 网关 12.244.130.39:80：普通模型调用走旧 Envoy，IP 未授权而返回空 body 的 403，被应用误报鉴权失败。不能据此认定 Key 错误。
- 绕过代理直连已连通，GET 根路径返回 nginx 403；GET Base URL 返回业务 404，这都不等于实际 POST chat/completions 不可用。
- 不要给模型网关附加 Workspace 的 Host ws-9837029.bocomcode；那只是 Ingress 排障头。
- 实际 Base URL 见用户现场配置；模型路径截图含 /ELLM.ELLM-AICODE.V-1.0/bocomwork/v1/chat/completions。严格以现场配置为准，勿记录密钥。

## 下一会话执行顺序

1. 实现并测试模式切换；先把“原默认”“全开”“迁移何时生效”说清楚，不只加两个 UI 按钮。
2. 测试所有模式双向切换、缺失/无效配置、历史配置兼容、开放撤销、disabled/终止中 Pod、多个 Workspace，验证没有多余放行。
3. Docker 隔离验证当前 IP HTTP/CONNECT 和热更新；Kubernetes E2E 仅在隔离 Kind，不连真实集群跑测试。
4. 更新投产手册，给新模式版本使用新镜像标签；不要覆盖已构建的 20261110-ip 标签导致节点缓存混用。
5. 提交代码及文档；根据用户授权再推送，明确本地 commit 不等于远端备份。
