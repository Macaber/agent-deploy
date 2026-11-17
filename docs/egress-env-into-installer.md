# 下次投产：把 EGRESS 环境变量三件套纳入 install.yaml（执行指令书）

> **本文档写给执行者（人或 AI 模型）**：用户会说"读这个文档并执行"。按步骤做，每步带验证，不要跳步。
> 背景速览：2026-11-20 投产时，EGRESS 三件套只靠 `kubectl set env`（手册步骤 10）注入线上，install.yaml 文件里没有——谁将来 apply 一次 install.yaml，env 整份对齐就会**静默删掉这三件套**（EGRESS_ENABLED 消失 = 接管关闭，无告警）。本任务一次性治本：把三件套写进 kustomize 源头，让生成的 install.yaml 与线上终态一致。

## 前置检查（起点必须干净）

2026-11-17 曾加过又全部回滚，先确认没残留：

```bash
cd D:\E\Code\TS\agent-deploy
grep -c "EGRESS_\|__CLUSTER" config/manager/manager.yaml deploy/install.yaml deploy/api-server-deploy.yaml
# 三处都必须是 0；不是 0 → 先读文件判断是残留还是别人改过，勿直接覆盖
```

## 步骤 1：先查线上当前值，再改源头 config/manager/manager.yaml

**① 查线上 EGRESS_ENABLED 当前值（决定文件里写什么）**：

```bash
kubectl -n agent-deploy-system get deployment agent-deploy-controller-manager -o yaml | grep -A1 "name: EGRESS_ENABLED"
```

| 线上当前值 | 文件里写 | 场景 |
| --- | --- | --- |
| `value: "true"` | **"true"** | **正常情况**：11-20 已投产开接管，后续每次投产都是这个值 |
| `value: "false"` 或查不到 | "false" | 仅首次/接管被主动关闭的环境 |

⚠️ **值与线上一致是硬性要求**：文件写 false 而线上是 true → 3.5 apply **当场静默关闭接管**（allowlist 模式下窗口期新建 Workspace Pod 无代理注入但被旧策略拦截 = 出网全断）；反之文件 true 而线上未就绪 → Operator 对账报错。

**② 在 env 段的 `NETPOL_ALLOWED_NAMESPACES` 块之后、`resources:` 之前插入**（`<ENABLED>` 用 ① 查到的值替换）：

```yaml
        # Unified egress management (see docs/egress-production-runbook.md).
        # EGRESS_ENABLED MUST mirror the live cluster value whenever this file
        # is generated/applied: mismatched value silently flips takeover at
        # apply time (no warning). True requires proxy + initial config in
        # place (runbook step 5/6 done in a prior rollout).
        - name: EGRESS_ENABLED
          value: "<ENABLED>"
        # Namespace holding workspace-egress-config/history/boundary ConfigMaps
        # shared by operator, api-server and the proxy sync container.
        - name: EGRESS_CONFIG_NAMESPACE
          value: "agent-deploy-system"
        # Infrastructure inventory (Pod/Service/node CIDRs, comma-separated),
        # identical per cluster to workspace-egress-boundary's value. PLACEHOLDER:
        # after `make build-installer`, sed-replace per cluster with the exact
        # value from egress-install.<cluster>.yaml before applying.
        - name: EGRESS_PROTECTED_CIDRS
          value: "__CLUSTER_PROTECTED_CIDRS__"
```

## 步骤 2：重新生成 install.yaml

```cmd
:: 在 cmd 里跑（本机 bash 无 make，bin/kustomize 是 Linux 二进制跑不了）
cd /d D:\E\Code\TS\agent-deploy
GOWORK=off make build-installer IMG=workspace-operator:<本次tag>
```

验证：`deploy/install.yaml` 的 env 段出现三件套，镜像 tag 正确，无 WARNING 头残留（生成是覆盖式）。

## 步骤 3：按集群填占位符（每个集群的发布目录各做一次）

占位符的值 = 该集群 `egress-install.<集群名>.yaml` 里 `EGRESS_PROTECTED_CIDRS` 的值（ConfigMap data 段，2026-11-17 定稿版在 L129 附近）——**逐字符一致，不要重新拼**：

```bash
sed -i 's|__CLUSTER_PROTECTED_CIDRS__|<从egress-install.<集群>.yaml复制的完整串>|' install.yaml
grep "__CLUSTER" install.yaml    # 应无输出
```

若两集群的 egress-install 文件也更新过节点清单（扩容等），以最新版为准；若那时已重拼，逐字对照 boundary 值即可。

## 步骤 4：同步更新投产手册（docs/egress-production-runbook.md）

1. **步骤 3.5 的 diff 门禁**：预期从"只允许镜像行"扩为"镜像行 + EGRESS_ENABLED / EGRESS_CONFIG_NAMESPACE / EGRESS_PROTECTED_CIDRS 三行**新增**（文件本次新加，预期内）"，其余行仍一票否决；并在门禁前加 ⓪ 填占位符（同步骤 3）。
2. **交付物清单** install.yaml 行：注明"含占位符，apply 前按 3.5 ⓪ 填；EGRESS_ENABLED 已按生成时线上值对齐"。
3. **步骤 10** 加一句：install.yaml 文件已含 EGRESS 三件套；本步 set env 仅在文件值与预期不符时产生变化（值一致则为空操作确认）。

## 步骤 5：收尾验证

```bash
python -c "import yaml; docs=list(yaml.safe_load_all(open('deploy/install.yaml',encoding='utf-8'))); print(len(docs),'docs OK')"
grep -n "EGRESS_ENABLED\|EGRESS_CONFIG_NAMESPACE\|EGRESS_PROTECTED_CIDRS" deploy/install.yaml
```

解析通过 + 三件套在位 → 完成。提交 git（用户同意后）。

## 长期红线（写给未来的所有人/模型）

**任何时点 re-apply install.yaml 前，必须核对线上 `EGRESS_ENABLED` 当前值并把它写进文件**：
- 线上是 true、文件是 false → apply 会**静默关闭接管**（白名单失效回 legacy，无告警）
- `kubectl diff -f install.yaml` 出现 `EGRESS_ENABLED -true/+false` 行时必须停手对齐
- 保护清单变更的日常维护见 runbook 8.2（set env + edit cm 两处同步 + rollout restart）

## 历史背景（可跳过）

- 2026-11-17 曾完整做过一遍（源头+生成物+手册），因投产手册封板全部回滚；本文件即当时方案的原样恢复指令。
- `set env` 改的是 Deployment spec（etcd 持久化，Pod 重建继承），"环境变量不持久化"是误解——真正的风险只来自 apply 无该 env 的旧文件。
