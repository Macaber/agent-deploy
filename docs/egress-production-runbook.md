# 全局出站模式：升级、验收与回退

更新：2026-11-11。模式切换允许重建 Workspace Pod；同模式下 IP/域名规则修改热更新。本文替代此前逐次追加的开发进度。服务器集群验收由用户执行，本文不宣称已经投产。

## 行为与边界

| 模式 | 行为 |
| --- | --- |
| legacy（恢复原有默认策略） | 新 Pod 不注入本功能代理，恢复 Workspace 原环境配置；沿用原共享策略及 disabled 语义 |
| allowlist（启用白名单） | 包括历史 disabled 在内的 Workspace 使用严格出站策略和显式代理 |
| unrestricted（全部出站开放） | 对本功能管理的 Workspace 放开全部出站，不注入本功能代理；不修改入站 |

原共享策略放行 TCP/UDP 53、IPv4 0/0（排除 10/8、100.64/10、169.254/16、172.16/12、192.168/16），并保留控制器全局基础服务例外。12.244.* 原本不在排除段中。管理员自建策略及集群更高层限制不被删除；用户原有代理配置保持原样。

切换影响所有 Workspace；模式保存后异步协调，运行中的 Pod 会因模板变化被替换，连接可能中断。休眠 Workspace 下次唤醒使用目标模式。切换期间新旧 Pod 可以并存，各自受对应策略约束，不能把 202 响应当成全部完成。旧模式策略等待旧 Pod（包括终止中）和活动模板退出后清理。

白名单内容在模式切换时保留。普通 IP/域名规则保存不改变 Pod 模板；NetworkPolicy 和 Envoy 路由动态更新。删除规则后已有连接不保证立即断开，使用新连接验收。

## 升级前准备

1. 核对 kubectl 当前 API 地址和节点，备份 Operator/API/代理三个 Deployment、CRD、Workspace、NetworkPolicy、workspace-egress-config、workspace-egress-boundary、workspace-egress-history（后者可能尚不存在）。保留镜像引用和 rollout revision。
2. 核对实际 ServiceAccount、命名空间；下文默认 Operator/代理在 agent-deploy-system，API 在 default，Workspace 在 bocomwork。不同环境先修改补丁和 RBAC subjects。
3. 保留 Operator 的 EGRESS_ENABLED、EGRESS_CONFIG_NAMESPACE、EGRESS_PROTECTED_CIDRS、全局基础服务例外、nodeSelector（现有测试环境 bocomworkuat2）。保留 PVC、入口和其他环境配置。
4. 首次安装还需要部署原有 CRD、API、Operator、代理及存储/RBAC。先启用 API，在网络页面保存 legacy，再启用 Operator EGRESS_ENABLED=true。不要把本节已有环境升级命令当成完整首次安装。
5. 白名单模式需要明确 TCP/UDP 53 DNS 规则、完整保护范围、代理就绪及实际加载版本一致。guard 的 DNS/保护范围变更需要代理重建；日常业务规则不需要。

## 本机构建

PowerShell，目录 D:\E\Code\TS\agent-deploy。每一步成功后再继续。停止占用 node_modules 的项目 Vite 后再 npm ci。

```powershell
Push-Location web\portal
npm ci
npm run build
Pop-Location
docker build --platform linux/amd64 -f Dockerfile -t workspace-operator:egress-modes-20261111 .
docker build --platform linux/amd64 -f Dockerfile.api-server -t api-server:egress-modes-20261111 .
docker save -o deploy\egress-modes-20261111.tar workspace-operator:egress-modes-20261111 api-server:egress-modes-20261111
```

本次未执行上述 Docker 构建，需现场执行。后续重新构建修订版使用新标签，避免缓存混用；勿覆盖 egress-unified-20261110-ip。

传输 tar、deploy/egress-mode-upgrade.patch.yaml、deploy/egress-mode-rbac.yaml。每个可能调度节点执行 `ctr -n k8s.io images import egress-modes-20261111.tar`，或统一推送内网仓库并替换全部镜像引用。原 Envoy、guard 镜像仍需可用。

## 已有环境升级顺序

备份后逐步执行，失败即停止；不要覆盖整份原始 install.yaml 或 api-server-deploy.yaml。

```bash
kubectl apply -f egress-mode-rbac.yaml
kubectl -n agent-deploy-system patch deployment workspace-egress --type=strategic --patch-file egress-mode-upgrade.patch.yaml
kubectl -n agent-deploy-system rollout status deployment/workspace-egress --timeout=180s
kubectl -n agent-deploy-system set image deployment/agent-deploy-controller-manager manager=workspace-operator:egress-modes-20261111
kubectl -n agent-deploy-system rollout status deployment/agent-deploy-controller-manager --timeout=180s
kubectl -n default set image deployment/api-server api-server=api-server:egress-modes-20261111
kubectl -n default rollout status deployment/api-server --timeout=180s
```

代理补丁保留 initialize-config/sync 的 `/egress-sync` command。代理滚动可能中断现有代理连接。Operator 开启接管且已有白名单配置时，升级可促使旧 Workspace 模板迁移，因此按维护窗口执行。

本次不修改 CRD schema；不要为本功能删除或回退 CRD。增量 RBAC 不覆盖现场已有角色，依赖既有 API 配置创建权限和 Workspace 读取权限。

## 页面操作与生效判断

- 首次未配置：保存默认 legacy。已有配置没有 mode 的，按 allowlist 兼容，不能视为默认开放。
- 修改规则：保存或放弃后再切换模式。点击三个模式按钮时确认 Pod 重建提示；API 模式变化要求 confirmModeChange=true 和当前 resourceVersion。
- 页面 pending 表示模板、Pod 或 rollout 尚未一致，会列出待完成资源。observed 表示已观测模式一致，不代表 CNI/网络实际验证通过。
- policyState 保持 unverified，实际流量效果由下节验证；legacy/unrestricted 不要求代理 loaded。allowlist 需所有代理副本的 desired/loaded revision 一致。
- 模式切换不要求管理员直接删除 Pod，不要暂停 Operator；等待控制器更新模板。如果调度或卷挂载阻碍替换，查 Deployment/Pod events。

## 服务器验收清单（用户执行）

先记录测试 Workspace 的 Pod 名称、UID、环境配置和策略；覆盖普通 Workspace、历史 disabled、休眠 Workspace，并至少验证两个 Workspace。

1. 六方向切换：legacy→allowlist→unrestricted→legacy→unrestricted→allowlist→legacy。每步等待旧 Pod 消失、目标 Pod Ready、页面待完成列表清空，再测试网络。
2. allowlist：授权 IP 直连成功，授权域名代理成功，未授权 IP/域名和错误端口的新连接失败；验证 DNS、真实模型 POST/SSE/取消。目标业务 403/404 不等于网络阻断。
3. 热更新：保持 allowlist，新增和删除 IP/域名；确认 Pod UID 不变，新连接按新规则允许/拒绝，代理 loadedRevision 更新。
4. unrestricted：验证原未授权 IP/域名、多端口和所需协议直连；核对未注入本功能代理。用户自有代理若存在，仍按用户配置运行。
5. legacy：核对原共享策略、基础服务例外、disabled 行为及用户环境变量恢复；待旧 Pod 全部退出后检查不存在本功能残留 allow-all 策略。
6. 异常：并发保存返回 409；切换前缺少确认返回 422；配置无效不放开。配置删除实验只在备份充分的测试环境执行，随后恢复备份，不要在生产试删。

```bash
kubectl -n bocomwork get deployment,pods -o wide
kubectl -n bocomwork get pods --show-labels
kubectl -n bocomwork get networkpolicy -o yaml
kubectl -n agent-deploy-system logs deployment/agent-deploy-controller-manager -c manager --since=10m --tail=150
kubectl -n bocomwork get events --sort-by=.metadata.creationTimestamp
```

curl 必须在 Workspace 容器内执行。分别使用 `curl --noproxy '*' ...` 验证直连，以及 `curl --noproxy '' --proxy http://workspace-egress.agent-deploy-system.svc:3128 ...` 验证显式代理。替换为现场实际地址，不在文档记录密钥。检查无 temporary-egress-debug 等额外全开策略；本功能不会自动删除管理员策略。

## 配置故障与回退

workspace-egress-history 为独立初始化标记。配置被删且标记存在时明确报错，不回到首次安装；初始化保存中断也报错。恢复备份的 workspace-egress-config，不要删除标记规避保护。两份 ConfigMap 都被人为删除不在该标记能识别的范围内；首次未配置也不会自动创建开放策略。

应急网络回退优先在新版页面选择 legacy 或 unrestricted，等待 Pod 替换并验证实际访问。直接 EGRESS_ENABLED=false 不会清除运行中代理环境，不能当成回退。

镜像回退和模式切换不同：如果要回退至不支持 mode 的旧程序，先在新版切换 allowlist 并等待全部完成，备份配置后仅移除 policy.json 的 mode 字段（保留全部规则），再按备份 revision 对三个 Deployment 执行 rollout undo 并 rollout status。旧程序无法读取含 mode 字段的配置；不要在 legacy/unrestricted Pod 仍存活时直接回退旧 Operator。若新版已无法工作，先恢复已备份的一整套兼容配置和镜像，按现场变更窗口处理，不能承诺无中断。

## 本地验证与交付边界

已运行相关 Go 单测、控制器定向回归、go vet、前端 TypeScript/Vite 构建和批量录入测试。覆盖 API 六方向切换保留规则、确认保护、配置丢失、初始化中断、终止中 Pod 及策略清理。

完整 make lint-fix 曾因 proxy.golang.org 下载超时失败；不将其列为通过。真实 CNI/服务器流量、镜像构建导入和业务客户端验收由用户按上文执行。本地测试不等于现场投产成功。
