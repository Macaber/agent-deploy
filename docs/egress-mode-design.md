> 历史设计及实施记录。当前操作和验证边界以 docs/egress-production-runbook.md 为准：模式切换允许重建，规则修改热更新，透明拦截方案已取消。

# 全局出站模式：实现约束与验收基线

日期：2026-11-11。本文为实现设计记录，不表示功能已完成。

## 已确认的需求

- 模式全局作用于所有 Workspace，不提供每 Workspace 模式配置。
- 模式为 legacy、allowlist、unrestricted；切换保留白名单数据。
- 用户要求热更新，不以休眠/唤醒作为日常模式切换步骤。
- legacy 必须撤销本次统一出站功能造成的影响，恢复原策略、原代理配置和 disabled 语义。仅替换 NetworkPolicy 不代表恢复完成。
- allowlist 和 unrestricted 覆盖历史 disabled Workspace；入站行为保持原样。

## 原策略的代码基线

internal/controller/workspace_controller.go 中 buildSharedEgressRules：

1. 任意目标 TCP/UDP 53 放行。
2. IPv4 0.0.0.0/0 放行，排除 10.0.0.0/8、100.64.0.0/10、169.254.0.0/16、172.16.0.0/12、192.168.0.0/16。
3. 叠加 GlobalAllowedNamespaces 基础服务例外。
4. workspace-shared-netpol 选择 managed-by=workspace-operator 的 Pod，入站全部放行。
5. 历史 disabled 不携带该 managed-by 标签。不能把恢复默认变成对这些 Pod 新增限制。

12.244.* 不属于上述排除段。恢复原策略不能擅自加入新的基础设施保护范围。管理员自建策略应保留；其实际影响需要现场单独核对。

## 当前架构阻碍

- proxyEnvironment 在 Pod 启动时覆盖大小写 HTTP_PROXY/HTTPS_PROXY/NO_PROXY 等变量。修改 Deployment 不能改变现有应用进程环境；删除变量也不能自动还原用户原有值。
- Envoy Bootstrap 的 DNS 为 V4_ONLY；显式代理的支持范围不是所有协议。
- GuardCommands 由 init 容器执行，限制 Envoy UID 101；业务 ConfigMap 更新不会重写 guard。
- 共享代理发出的流量以代理为源，不能直接用 Workspace 原 NetworkPolicy 的语义约束代理请求。

因此，“NetworkPolicy 全开 + Envoy 通配路由”不满足 unrestricted；“恢复共享策略 + 保留注入代理”不满足 legacy。

## 待确认的迁移边界

是否允许一次性休眠/唤醒，把现有注入显式代理的 Workspace 迁移到新的执行机制。此选择只解决已有进程环境问题，不能单独证明未来热切换可行。新的执行机制仍需单独设计和隔离验证，不得将用户同意一次迁移解释为已同意任意特权组件或生产重建。

## 验收要求

- 三模式六个方向切换；对运行中 Workspace 使用真实客户端验证，不能仅检查 YAML。
- legacy：恢复原直连路径、用户代理配置、基础服务例外、disabled 行为。
- unrestricted：IP/域名、端口、协议均不被本功能新增限制；验证代理路径不会残留额外拒绝。
- allowlist：撤销本功能的全开规则，验证未授权新连接被拒绝、DNS 和授权访问成功。
- 缺失/无效配置、重启恢复、多个命名空间、终止中 Pod、历史配置兼容、并发保存冲突。
- 状态区分目标、协调中、失败、实际生效；保存成功和代理 loaded 不等于全局完成。
- 全局收敛不是跨节点原子切换；已有连接的撤销能力必须按真实数据平面测量并写入手册。
- Kubernetes E2E 仅在隔离 Kind 环境执行；未实测项不写通过。

现有 egress-unified-20261110-ip 镜像不含三模式功能，不能作为新模式发布版本。

## 2026-11-11 实现进度（尚不可投产）

用户已确认接受存量 Workspace 一次休眠/唤醒迁移。日常模式切换仍要求热更新，legacy 要恢复原网络行为。

已实现：Config.Mode 三种显式值、旧配置默认 allowlist、显式 allowlist 与旧配置哈希一致、不同模式版本不同、非法模式拒绝。当前 API 对 legacy/unrestricted 返回 422；Envoy 路由和严格策略生成器也拒绝这两个模式。此保护用于避免尚未实现的数据平面虚报生效，不是模式切换功能交付。

已通过：internal/egress 与 cmd/api-server 单测，internal/controller 的 TestEgress 回归测试；cmd/egress-sync 无测试文件。

make lint-fix 在 WSL 中失败：go 不在该 shell PATH。由于命令使用 &&，make test 未执行。Windows Go 1.27.3 可以直接运行上述定向测试。

未完成：可热旁路且保留域名授权能力的执行机制、控制器全局协调、持久迁移证据、实际状态 API、页面按钮、六方向隔离 E2E 和发布清单。一次迁移并不能自动使现有显式代理支持透明旁路；不应向用户承诺仅迁移即可解决。

### 继续排查结果

已定位 WSL Go 为 /usr/local/go/bin/go，通过为 make 显式设置 PATH 修复工具发现问题。lint-fix 随后在下载 golangci-lint v2.12.2 时因 proxy.golang.org 连接超时失败；不是模式代码编译错误。make test 已启动，最终结果需读取运行会话，不可先写通过。

新增 API 回归测试 TestUnsupportedModeDoesNotOverwriteSavedRules 已通过：legacy/unrestricted 返回 422，原规则和 resourceVersion 均保持不变。

架构候选为可动态旁路的透明流量拦截组件，需要网络管理权限。已向用户询问部署环境是否允许先做隔离原型，尚未部署或声明等价。透明 TLS 的 SNI 识别与显式 CONNECT 授权不同，ECH/无 SNI、用户自有代理及保护范围都必须验证；DNS 映射 IP 放行也不能替代原域名授权语义。

参考：Envoy TLS inspector https://www.envoyproxy.io/docs/envoy/v1.31.5/configuration/listeners/listener_filters/tls_inspector ，Cilium DNS 策略 https://docs.cilium.io/en/latest/security/policy/layer3/ 。这些资料仅用于评估，不表示将替换现场 Calico。

## 最新需求与控制器进度（覆盖前述透明拦截方案）

用户明确：模式切换允许重建 Pod；白名单中的 IP/域名规则更新必须热更新。不再推进透明流量拦截组件。

已实现控制器开发分支：legacy/unrestricted 不要求代理就绪；新 Pod 使用 Workspace runtime.env 原配置，只有 allowlist 注入代理。unrestricted 使用独立 unrestricted-v1 Pod 标签和仅出站 allow-all 策略，旧 envoy-v1 Pod 不被该 allow-all 选中。控制器使用同一配置快照构建策略和 Pod 模板，避免一次协调过程中并发保存产生错配。模式切换期间 Envoy 继续生成保留的白名单路由以服务旧 Pod。

新增 TestNonProxyModesDoNotRequireProxyAndScopeAllowAll，通过非代理模式无代理依赖、allow-all 标签隔离及仅出站范围验证。定向 controller/egress/API 回归通过。

尚未完成：API 422 保护解除、页面模式切换与重建确认、全局实际收敛状态、旧模式策略清理、首次未配置与配置丢失的持久区分，以及六方向真实隔离测试。当前不作为可发布功能。禁止手工写 ConfigMap 绕过 API 保护用于生产。

### 模式入口接通进度

API 已改为仅未确认模式变更时返回 422；确认后允许保存三模式。页面已有全局按钮和重建提示，首次未配置可保存 legacy。旧模式策略按 Pod 和活动 Deployment 模板引用清理，终止中 Pod 仍计入。API 六方向保留规则、清理测试及前端构建通过。全局状态与缺失配置持久区分仍待实现；当前配置缺失阻止协调，不会自动恢复 legacy。投产验收尚未完成。
