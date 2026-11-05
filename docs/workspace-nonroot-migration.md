# Workspace 非 Root 化迁移 Runbook（OSS 存储）

> 适用：数据全部在 `oss://<bucket>/workspaces/<ws>/` 的集群。
> 目标：workspace Pod 内的 agent 以 **UID/GID 1000** 运行，镜像、挂载、SecurityContext 三者一致。
> 原则：**一次只改一个变量**（先改挂载属主、再改运行用户），每一阶段都能独立回滚。

---

## 0. 先回答两个前置问题

### 0.1 不用 initContainer 能不能改完权限？——能，而且 initContainer 本来就无效

| 事实 | 含义 |
|---|---|
| OSS 是对象存储，**不保存 POSIX 的 uid/gid/mode** | `chown` / `chmod` 落在 OSS 对象上没有意义，不会持久化 |
| ossfs 在挂载时用挂载参数 **合成** 属主与权限（`uid=` `gid=` `umask=`） | 想“让文件属于 1000”，唯一有效的做法是**改挂载参数** |
| 因此 initContainer 里跑 `chown -R 1000:1000` 是**空操作** | 白跑一遍还可能因为 FUSE 元数据写入拖慢启动 |

**结论：本方案不使用 initContainer 做属主迁移，改为调整 ossfs 挂载参数 + 重建 PV。**
只有当你的集群里**还有** local-path / NFS 的存量数据时，那部分才需要一次性 root `chown`（见 §9 附录 C，OSS 场景跳过）。

### 0.2 为什么必须重建 PV？——已实测

```
$ kubectl apply -f pv.yaml   # 试图修改 csi.volumeAttributes.otherOpts
The PersistentVolume "ws-x-pv" is invalid: spec.persistentvolumesource:
Forbidden: spec.persistentvolumesource is immutable after creation
```
（已在本地 K8s v1.36 API Server 上验证）

`csi.volumeAttributes` 属于 PV 的 `persistentVolumeSource`，**创建后不可变**。所以：

- 改 StorageClass → **只影响此后新创建的 PV**
- 存量 PV → 必须 **删除 PVC + PV，让 controller 用新参数重建**
- 重建**不会丢数据**：数据在 OSS 对象存储里，PV 只是“挂载描述”；且 reclaimPolicy 为 `Retain`

---

## 1. 目标状态

```
┌───────────────────────────── Pod (runAsUser=1000) ─────────────────────────────┐
│  agent 进程 UID/GID = 1000, HOME=/home/agent                                    │
│  seccomp RuntimeDefault, allowPrivilegeEscalation=false, (非 root 后可 drop ALL) │
└───────────────┬────────────────────────────────────────────────────────────────┘
                │ 挂载（由 ossfs 合成 uid=1000,gid=1000,umask=022）
                ▼
   oss://<bucket>/workspaces/<ws>/  ──  workspace/ config/ share/ state/ cache/
```

| 层 | 改动 | 是否改代码 |
|---|---|---|
| 镜像 | 预置 `agent` 用户 UID/GID 1000、`HOME=/home/agent`、`chmod 755 /root` | 改镜像（非本仓库） |
| 存储 | SC `otherOpts` 增加 `-o uid=1000,gid=1000,umask=022` | 不改 |
| PV | 存量逐个重建（参数不可变） | 不改 |
| Pod | `runAsUser/runAsGroup/fsGroup=1000`, `runAsNonRoot=true` | **需改 controller（§3）** |
| 灰度开关 | Workspace 注解 `ai.example.com/run-as-user`，不注解=保持 root | 同上 |

---

## 2. 前置检查（只读，先跑完再动手）

```bash
export NS=bocomwork
export BUCKET='<your-bucket>'
export SC=alicloud-oss

# 2.1 确认 OSS StorageClass 参数
kubectl get sc $SC -o yaml | sed -n '/parameters:/,/^[a-z]/p'

# 2.2 确认目标 workspace 用的是 OSS、且路径符合预期
export WS=ws-<ID>
kubectl -n $NS get pvc ${WS}-pvc -o jsonpath='{.spec.volumeName}{"\n"}'
kubectl get pv ${WS}-pv -o jsonpath='{range .spec.csi.volumeAttributes}{@}{"\n"}{end}' \
  | grep -E 'bucket|path|otherOpts'
# 期望 path=/workspaces/$WS

# 2.3 记录 OSS 对象数量（迁移前后对比，用于证明数据未丢）
ossutil ls -s "oss://$BUCKET/workspaces/$WS/" | tail -5 | tee /tmp/oss-count-before.txt

# 2.4 确认镜像里的现状（决定 §4 要改什么）
kubectl -n $NS exec deploy/${WS}-deploy -- sh -c 'id; echo HOME=$HOME; ls -ld /root; ls -ld /root/.config/opencode 2>/dev/null'

# 2.5 确认 controller 是否已支持注解灰度开关（§3 合并后应有）
kubectl -n agent-deploy-system get deploy agent-deploy-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

**出口准则**：2.2 的 `path` 正确、2.3 有对象数、2.4 知道镜像是否已有 UID 1000。

---

## 3. 代码改动：注解灰度开关（必须合并后才能改用户）

> 现状：`buildWorkspaceSecurityContexts()` 写死“保持镜像默认用户（root）”。CRD 没有 securityContext 字段，若直接改死为 1000，无法灰度、也无法单点回滚。
> 因此改成 **注解驱动、默认关闭**：不注解 = 行为与今天完全一致。

**`internal/controller/workspace_controller.go`**

```go
// runAsUserAnnotation opts a single Workspace into running as a non-root UID.
// Absent annotation keeps the historical root behaviour, so the migration can be
// canaried per Workspace and rolled back without redeploying the controller.
const runAsUserAnnotation = "ai.example.com/run-as-user"
```

在 `reconcileDeployment` 中，`podSecurityContext, containerSecurityContext := buildWorkspaceSecurityContexts()` 之后插入：

```go
if raw := ws.Annotations[runAsUserAnnotation]; raw != "" {
	uid, convErr := strconv.ParseInt(raw, 10, 64)
	switch {
	case convErr != nil:
		log.Error(convErr, "Ignoring unparsable run-as-user annotation",
			"value", raw, "annotation", runAsUserAnnotation)
	case uid <= 0:
		log.Info("Ignoring non-positive run-as-user annotation, keeping image user",
			"value", raw, "annotation", runAsUserAnnotation)
	default:
		podSecurityContext.RunAsUser = &uid
		podSecurityContext.RunAsGroup = &uid
		podSecurityContext.FSGroup = &uid
		podSecurityContext.RunAsNonRoot = boolPtr(true)
	}
}
```

说明：
- `strconv` 已在本文件 import，无需新增。
- 更新路径已有 `DeepEqual(podSpec.SecurityContext, podSecurityContext)` 收敛逻辑，**存量 Deployment 会自动跟随注解变化**，无需手工改 Deployment。
- 非 root 之后 `capabilities.add` 里的 DAC 类能力对非 root 进程不生效（内核只对 UID 0 授予），保留无副作用；若后续要上 PSS `restricted`，再按 §8 改成 `drop: ALL`。

**必配单元测试**（`workspace_controller_test.go`）：分别断言“无注解 → `RunAsUser=nil`”、“有注解 1000 → `RunAsUser=1000` 且 `RunAsNonRoot=true`”。

**合并后**：
```bash
make manifests generate && make test
IMG=<registry>/workspace-operator:<tag> && make docker-build docker-push IMG=$IMG
kubectl -n agent-deploy-system set image deploy/agent-deploy-controller-manager \
  manager=$IMG
kubectl -n agent-deploy-system rollout status deploy/agent-deploy-controller-manager
```

> 上线后先确认**无注解的 workspace 行为不变**（镜像/挂载/用户都没动），再做 §5 的灰度。

---

## 4. 镜像改动（agent image，非本仓库）

镜像里必须有 UID/GID 1000 的用户，并且让 controller 写死的 `/root/...` 挂载点可被 1000 访问。

```dockerfile
# 1) 预置用户与家目录
RUN groupadd -g 1000 agent && useradd -u 1000 -g 1000 -m -d /home/agent -s /bin/bash agent

# 2) 【关键】放开 /root 的可穿越位，并预建 4 个挂载点目录（controller 硬编码在 /root 下）
RUN chmod 755 /root && \
    mkdir -p /root/.config/opencode /root/.local/share/opencode \
             /root/.local/state/opencode /root/.cache/opencode && \
    chown -R 1000:1000 /root/.config /root/.local

# 3) HOME 必须指向可写目录（否则 npm/git/opencode 写缓存失败）
ENV HOME=/home/agent
USER 1000:1000
```

要点：
- **`chmod 755 /root` 是必须的**：`/root` 默认 `0700`，非 root 连遍历都不行，4 个挂载点会整体不可访问，opencode 起不来。
- `USER 1000:1000` 只是兜底；真正生效的是 Pod 的 `runAsUser`（两者一致才不会漂移）。
- entrypoint 里若还有 `chown` / `apt-get` / `apk add` / 写 `/usr/local`，必须删掉或挪到构建期。
- 解包带属主的 tar 时加 `--no-same-owner`，否则非 root 会刷 `Cannot change ownership` 警告。

### 4.1 过渡方案 B：暂不重建镜像（可选，仅用于灰度期）

用 CR 字段把挂载点和 HOME 挪出 `/root`，不改镜像：

```yaml
spec:
  runtime:
    env:
      - { name: HOME, value: /home/agent }
    volumeMounts:                 # 注意：一旦非空，默认挂载整体失效，必须列全
      - { mountPath: /workspace,                       subPath: workspace }
      - { mountPath: /home/agent,                      subPath: home }
      - { mountPath: /home/agent/.config/opencode,     subPath: config }
      - { mountPath: /home/agent/.local/share/opencode, subPath: share }
      - { mountPath: /home/agent/.local/state/opencode, subPath: state }
      - { mountPath: /home/agent/.cache/opencode,      subPath: cache }
```
> 前提：镜像里 `/home` 存在且可被 kubelet 创建子目录（kubelet 以 root 创建挂载点，最终属主由 ossfs 合成为 1000）。生产仍建议走 §4 正解。

---

## 5. 挂载参数改动（StorageClass）

```bash
# 5.1 备份
kubectl get sc $SC -o yaml > backup/sc-$SC-$(date +%F-%H%M).yaml

# 5.2 追加 uid/gid/umask（保留原有参数，整串替换 map 值）
kubectl patch sc $SC --type merge -p '{"parameters":{"otherOpts":"-o max_stat_cache_size=100000 -o allow_other -o uid=1000,gid=1000,umask=022"}}'

# 5.3 校验
kubectl get sc $SC -o jsonpath='{.parameters.otherOpts}{"\n"}'
# 期望: -o max_stat_cache_size=100000 -o allow_other -o uid=1000,gid=1000,umask=022
```

说明：
- 该 SC 只影响**之后新建**的 PV；存量 PV 在 §6 重建时才生效。
- `umask=022` → 目录 755、文件 644，属主 1000。单租户 workspace 足够。
- **ossfs 2.0（`fuseType: ossfs2`）不要套用这三个参数**，其参数集不同；本方案仅针对 `fuseType: ossfs`（仓库默认，见 `deploy/oss/storageclass.yaml`）。
- 若同时存在 `deploy-ack/storageclass-ossfs2.yaml`，先确认目标 workspace 用的是哪一个：`kubectl get pv ${WS}-pv -o jsonpath='{.spec.csi.volumeAttributes.fuseType}'`。

---

## 6. 灰度方案（一个变量一个阶段）

| 阶段 | 改什么 | 影响面 | 单次耗时 | 失败即回滚到 |
|---|---|---|---|---|
| **S0** | 备份 + 记录对象数 | 无 | 5 min | — |
| **S1** | 新建 1 个测试 workspace（1000） | 新租户 1 个 | 10 min | 删掉该 workspace |
| **S2** | 存量 1 个：**只重建 PV**，容器仍 root | 1 个存量 | 10 min | §7.2 |
| **S3** | 同上 1 个：**加注解切 1000** | 1 个存量 | 5 min | §7.3（去注解） |
| **S4** | 批量 5 个 → 20 个 → 全量（分租户） | 按批次 | 每批 15 min | 同 S2/S3 |
| **S5** | 观察 24h 后清理、可选收紧 PSS | 全量 | — | 见 §7 |

**为什么 S2 和 S3 要分开**：S2 只验证“新挂载参数下数据还在、读写正常”，此时容器仍是 root（DAC_OVERRIDE 会掩盖权限问题）；S3 再切用户，才能真实验证 1000 的读写。两个变量同时改，出问题无法判断是哪个引起的。

### S0 备份

```bash
mkdir -p backup/$NS/$WS
kubectl -n $NS get workspace $WS -o yaml            > backup/$NS/$WS/workspace.yaml
kubectl -n $NS get deploy  ${WS}-deploy -o yaml     > backup/$NS/$WS/deploy.yaml
kubectl -n $NS get pvc     ${WS}-pvc   -o yaml      > backup/$NS/$WS/pvc.yaml
kubectl get pv             ${WS}-pv    -o yaml      > backup/$NS/$WS/pv.yaml
ossutil ls -s "oss://$BUCKET/workspaces/$WS/"       > backup/$NS/$WS/oss-ls.txt
```
> 所有 PV/PVC 的 reclaimPolicy 都是 `Retain`，且数据在 OSS，**删 PV/PVC 不会删对象**；备份是为了“参数/命名可复原”，不是防数据丢失。

### S1 新租户验证（不动存量）

```bash
# 用新租户 ID 建 workspace（走 portal 或直接 apply），确保命中新 SC 参数
kubectl -n $NS apply -f - <<'EOF'
apiVersion: ai.example.com/v1alpha1
kind: Workspace
metadata:
  name: ws-canary-1000
  namespace: bocomwork
  annotations:
    ai.example.com/run-as-user: "1000"
spec:
  owner: canary
  runtime:
    image: <your-registry>/opencode:<nonroot-tag>
    cpu: "1"
    memory: "2Gi"
  storage: { size: "1Gi" }
EOF

# 等就绪后逐项验证
kubectl -n $NS wait --for=condition=Available deploy/ws-canary-1000-deploy --timeout=300s
kubectl -n $NS exec deploy/ws-canary-1000-deploy -- id
kubectl -n $NS exec deploy/ws-canary-1000-deploy -- sh -c 'ls -ln /workspace | head; touch /workspace/.probe && rm /workspace/.probe && echo WRITE_OK'
kubectl -n $NS exec deploy/ws-canary-1000-deploy -- sh -c 'mount | grep -i ossfs'
kubectl -n $NS exec deploy/ws-canary-1000-deploy -- sh -c 'git -C /workspace status >/dev/null && echo GIT_OK'
```

判定：`id` 为 1000；`ls -ln` 属主 1000；`WRITE_OK`；ossfs 挂载参数含 `uid=1000`。

### S2 存量：只重建 PV（容器仍 root）

```bash
export WS=ws-<ID>

# 1) 停止（controller 会 scale 到 0）
kubectl -n $NS patch workspace $WS --type merge -p '{"spec":{"stopped":true}}'
kubectl -n $NS wait --for=delete pod -l app=workspace,workspace=$WS --timeout=300s

# 2) 删 PVC + PV（数据在 OSS，不动）
kubectl -n $NS delete pvc ${WS}-pvc
kubectl delete pv ${WS}-pv

# 3) 确认都已消失（避免 controller 抢建旧参数 PV）
for i in $(seq 1 20); do
  kubectl -n $NS get pvc ${WS}-pvc >/dev/null 2>&1 || \
  kubectl get pv ${WS}-pv >/dev/null 2>&1 || break
  sleep 3
done

# 4) 启动，触发重建
kubectl -n $NS patch workspace $WS --type merge -p '{"spec":{"stopped":false}}'

# 5) 等新 PV 出现且参数正确
until kubectl get pv ${WS}-pv -o jsonpath='{.spec.csi.volumeAttributes.otherOpts}' 2>/dev/null | grep -q 'uid=1000'; do
  sleep 3
done
echo "PV_REBUILT_OK"

# 6) 数据与读写验证（此时仍是 root）
kubectl -n $NS exec deploy/${WS}-deploy -- sh -c 'ls -ln /workspace | head; ls /workspace | wc -l'
ossutil ls -s "oss://$BUCKET/workspaces/$WS/" | tail -5
```
**对比 S0 的对象数与目录内容**；不一致就停下来走 §7.2。

> Controller 竞态提示：批量操作时建议按 §6.1 暂停 controller，避免“删掉又被抢建”。

### S3 存量：切 1000（加注解）

```bash
kubectl -n $NS annotate workspace $WS ai.example.com/run-as-user=1000 --overwrite
kubectl -n $NS rollout status deploy/${WS}-deploy --timeout=300s
kubectl -n $NS exec deploy/${WS}-deploy -- id          # 期望 uid=1000
kubectl -n $NS exec deploy/${WS}-deploy -- sh -c 'echo $HOME; ls -ld /root/.config/opencode'
```
验证矩阵见 §8。**注意**：容器重启（`Recreate` 策略）会造成该 workspace 短暂不可用，避开业务高峰。

### 6.1 批量（S4）—— 暂停 controller 的稳妥做法

```bash
# A. 先把要迁的 workspace 全部停止（controller 在线，负责 scale 到 0）
for WS in $(cat ws-list.txt); do
  kubectl -n $NS patch workspace $WS --type merge -p '{"spec":{"stopped":true}}'
done
kubectl -n $NS wait --for=delete pod -l app=workspace --timeout=600s

# B. 暂停 controller，消除重建竞态
kubectl -n agent-deploy-system scale deploy/agent-deploy-controller-manager --replicas=0

# C. 批量删 PVC/PV
for WS in $(cat ws-list.txt); do
  kubectl -n $NS delete pvc ${WS}-pvc --ignore-not-found
  kubectl delete pv ${WS}-pv --ignore-not-found
done
kubectl -n $NS get pvc -o name | grep -E "$(paste -sd'|' ws-list.txt)" && echo "!! 仍有 PVC，先排查" || echo "PVC_CLEAN"

# D. 恢复 controller → 自动重建 PV/PVC（新参数）并拉起 Pod
kubectl -n agent-deploy-system scale deploy/agent-deploy-controller-manager --replicas=1

# E. 逐个取消停止并验证
for WS in $(cat ws-list.txt); do
  kubectl -n $NS patch workspace $WS --type merge -p '{"spec":{"stopped":false}}'
done
```
**批次建议**：5 → 20 → 全量，批间观察 10 min；OSS FUSE 重建会打一波元数据压力，别一次全量。

---

## 7. 回滚方案（逐阶段，均可独立执行）

### 7.1 回滚 S1（新租户）
```bash
kubectl -n $NS delete workspace ws-canary-1000
# PVC/PV 不会被级联删除（无 ownerReference），按需手工清理：
# kubectl -n $NS delete pvc ws-canary-1000-pvc; kubectl delete pv ws-canary-1000-pv
```

### 7.2 回滚 S2（挂载参数）—— 数据全程不受影响
```bash
# 1) 还原 SC
kubectl apply -f backup/sc-$SC-<时间戳>.yaml
# 2) 停 → 删 PVC/PV → 启（同 §6 S2 步骤 1~5），让 PV 用旧参数重建
```
> 关键前提：**`stopped` 期间不要删 OSS 前缀**。整个流程只动 K8s 对象，OSS 只有一个前缀 `/workspaces/<ws>/`，回滚时被重新挂载。

### 7.3 回滚 S3（运行用户）—— 最快，1 条命令
```bash
kubectl -n $NS annotate workspace $WS ai.example.com/run-as-user- 
# controller 自动收敛回 root（不改镜像、不改 PV、不重启业务数据）
```
> 之所以安全：挂载参数 `uid=1000` 只影响“文件呈现的属主”，**root 进程仍能读写 1000 属主的文件**（有 `DAC_OVERRIDE`），因此“用户回滚”和“挂载参数回滚”**可以解耦**——出问题先做 7.3，30 秒内恢复。
>
> ⚠️ **隐性依赖**：7.3 依赖容器 root 持有 `DAC_OVERRIDE`（当前 `workspaceAddedCapabilities` 里有）。若后续按 §8.1 改成 `drop: ALL` 以满足 PSS `restricted`，则**先改注解回滚会失败**（root 无 DAC_OVERRIDE，对 1000 属主、644 的文件无写权限）。此时回滚顺序必须是：先把 caps 恢复成含 DAC 的那组 → 再去注解。建议把这条写进变更单。

### 7.4 回滚 S4（批量）
```bash
# 对批次内所有 ws 去掉注解即可（§7.3 的循环版）
for WS in $(cat ws-list.txt); do
  kubectl -n $NS annotate workspace $WS ai.example.com/run-as-user- 2>/dev/null || true
done
# 如需连挂载参数一起回退：§7.2 + 批量重建（controller 暂停法同 §6.1）
```

### 7.5 回滚决策树

```
Pod 起不来 / agent 不可用
├─ id 显示 uid=0 且 ls -ln 属主 1000   → 只是注解未生效，查 controller 镜像/注解拼写
├─ id 显示 1000，但 Permission denied
│   ├─ 路径在 /root 下（.config/.local）→ /root 没放开 o+x → 回滚 7.3，改镜像 §4
│   ├─ 写 /workspace 失败，属主 root    → PV 未重建成功 → 7.2
│   └─ 写 /workspace 失败，属主 1000    → 看 ossfs 是否 uid=1000，或 umask 过严
├─ 数据“看起来变少/为空”               → 先查 mount 到的 path 是否 /workspaces/<ws>
└─ 需要立刻恢复业务                    → 7.3（30 秒）→ 再从容排查
```

---

## 8. 验证矩阵

| # | 检查 | 命令 | 期望 |
|---|---|---|---|
| 1 | 用户 | `id` | `uid=1000 gid=1000` |
| 2 | 家目录 | `echo $HOME` | `/home/agent`（或方案 B 的路径） |
| 3 | 挂载参数 | `mount \| grep ossfs` | 含 `uid=1000,gid=1000,umask=022` |
| 4 | 属主 | `ls -ln /workspace` | 1000 |
| 5 | 写工作区 | `touch /workspace/.p && rm /workspace/.p` | 成功 |
| 6 | git 写 | `git -C /workspace status`（空仓用 `git init` 验证） | 成功 |
| 7 | 包管理 | `npm install` / `pip install --user` | 成功（走 `$HOME` 缓存） |
| 8 | opencode 配置 | `ls /root/.config/opencode` 或 `$HOME/.config/opencode` | 可读写 |
| 9 | 探针 | `kubectl get pod` | Running，无重启 |
| 10 | 安全 | `cat /proc/self/status \| grep NoNewPrivs` | `1` |
| 11 | 能力 | `capsh --print` | 无 `sys_admin`；非 root 下 add 列表无效属正常 |
| 12 | 数据完整 | `ossutil ls -s oss://$BUCKET/workspaces/$WS/` | 对象数与 S0 一致 |
| 13 | 共享只读卷 | `ls /data/oss-shared`（如挂载） | 可读 |

### 8.1 可选：收紧到 PSS `restricted`

非 root 之后 `drop: ALL` 重新变得安全（非 root 不需要任何 capability），此时才能满足 `restricted`：

```go
// buildWorkspaceSecurityContexts 中，非 root 分支改为：
Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
```
并同时：
- `runAsNonRoot: true`、`seccompProfile: RuntimeDefault`（已具备）
- 命名空间打标：
```bash
kubectl label ns $NS pod-security.kubernetes.io/enforce=restricted --overwrite
```
⚠️ 必须**先**改完容器 securityContext **再**打标，否则 Pod 直接被准入拒绝。打标前用 dry-run 验证：
```bash
kubectl label ns $NS pod-security.kubernetes.io/enforce=restricted --dry-run=server --overwrite
```

---

## 9. 已知限制（务必让业务方知晓）

1. **OSS 上没有真正的文件权限**：`umask` 对所有文件一视同仁（644/755），**无法做到“某个文件 0600 只有 owner 可读”**。切非 root 并不能阻止 agent 读同目录里的 `.env`/token —— 这类敏感信息应走 Secret/环境变量，不要指望文件权限。
2. **ossfs 2.0 不适用本方案的 `uid/gid/umask` 写法**，且官方不建议用于随机写负载（见 `deploy-ack/storageclass-ossfs2.yaml` 注释）。
3. **`fsGroup` 对 OSS FUSE 基本不起作用**，属主只认挂载参数；不要依赖它。
4. **PV 重建会造成该 workspace 一次重启**（`Recreate` 策略），期间不可用，避开高峰。
5. **initContainers**：若 `spec.runtime.initContainers` 里的镜像以 root 运行，Pod 级 `runAsNonRoot: true` 会让它直接被拒；迁移期需移除这些 initContainer，或给它们单独放行（见下方）。
6. **postStartScript** 以容器用户（1000）执行，此前依赖 root 的脚本（改属主/装包/写系统目录）会失败，需要改写。
7. 若存在**混用存储**（OSS + local-path/NFS）：非 OSS 部分仍需真实 `chown`，见附录 C。

> 关于 5 的临时解：需要 root initContainer 时，Pod 级不能设 `runAsNonRoot: true`；只能改用“容器级 `runAsUser: 0` + Pod 级 `runAsNonRoot: false`”，并在迁移完成后移除。**不建议**长期保留。

---

## 10. 附录

### 附录 A：一次完整的单 workspace 迁移（可直接整段执行）

```bash
#!/usr/bin/env bash
set -euo pipefail
NS=bocomwork; SC=alicloud-oss; WS="$1"; BUCKET="${BUCKET:?set BUCKET}"
B=backup/$NS/$WS; mkdir -p "$B"

kubectl -n "$NS" get workspace "$WS" -o yaml > "$B/workspace.yaml"
kubectl -n "$NS" get pvc "${WS}-pvc" -o yaml   > "$B/pvc.yaml"
kubectl get pv "${WS}-pv" -o yaml              > "$B/pv.yaml"
ossutil ls -s "oss://$BUCKET/workspaces/$WS/"  > "$B/oss-before.txt"

kubectl -n "$NS" patch workspace "$WS" --type merge -p '{"spec":{"stopped":true}}'
kubectl -n "$NS" wait --for=delete pod -l "app=workspace,workspace=$WS" --timeout=300s

kubectl -n "$NS" delete pvc "${WS}-pvc"
kubectl delete pv "${WS}-pv"
for i in $(seq 1 20); do
  kubectl -n "$NS" get pvc "${WS}-pvc" >/dev/null 2>&1 || kubectl get pv "${WS}-pv" >/dev/null 2>&1 || break
  sleep 3
done

kubectl -n "$NS" patch workspace "$WS" --type merge -p '{"spec":{"stopped":false}}'
until kubectl get pv "${WS}-pv" -o jsonpath='{.spec.csi.volumeAttributes.otherOpts}' 2>/dev/null | grep -q 'uid=1000'; do sleep 3; done

kubectl -n "$NS" annotate workspace "$WS" ai.example.com/run-as-user=1000 --overwrite
kubectl -n "$NS" rollout status "deploy/${WS}-deploy" --timeout=300s

kubectl -n "$NS" exec "deploy/${WS}-deploy" -- id
kubectl -n "$NS" exec "deploy/${WS}-deploy" -- sh -c 'ls -ln /workspace | head'
ossutil ls -s "oss://$BUCKET/workspaces/$WS/" > "$B/oss-after.txt"
diff <(tail -3 "$B/oss-before.txt") <(tail -3 "$B/oss-after.txt") && echo "OBJECT_COUNT_UNCHANGED"
```

### 附录 B：先跑 dry-run 看清单

```bash
# 需要 jq；若集群跳板机没有 jq，用下面的 jsonpath 版本
kubectl get pv -o json | jq -r '
  .items[] | select(.spec.csi.driver=="ossplugin.csi.alibabacloud.com") |
  [.metadata.name,
   (.spec.csi.volumeAttributes.path // ""),
   (if (.spec.csi.volumeAttributes.otherOpts // "") | contains("uid=1000") then "OK" else "TODO" end)]
  | @tsv' | column -t | sort -k3

# 无 jq 版本：输出 "PV名 <TAB> OSS路径 <TAB> otherOpts"（需 awk）
kubectl get pv -o jsonpath='{range .items[*]}{.spec.csi.driver}{"\t"}{.metadata.name}{"\t"}{.spec.csi.volumeAttributes.path}{"\t"}{.spec.csi.volumeAttributes.otherOpts}{"\n"}{end}' \
  | awk -F'\t' '$1 ~ /ossplugin/ {print $2"\t"$3"\t"$4}' \
  | awk -F'\t' '{print $0 "\t" ($3 ~ /uid=1000/ ? "OK" : "TODO")}'
```

### 附录 C：非 OSS（local-path / NFS）残留数据的属主迁移

仅当还有这类数据时使用；OSS 场景**跳过**。

```yaml
# 一次性 initContainer，纯 OSS 环境不要启用
initContainers:
- name: fix-ownership
  image: <same-image>
  command: ["/bin/sh","-c"]
  args:
  - |
    find /workspace -xdev \( -uid 0 -o -uid 65534 \) -exec chown 1000:1000 {} + 2>/dev/null
  securityContext:
    runAsUser: 0          # 容器级覆盖；此时 Pod 级不能有 runAsNonRoot:true
  volumeMounts:
  - { name: workspace-data, mountPath: /workspace, subPath: workspace }
```

### 附录 D：术语对照

| 名词 | 含义 |
|---|---|
| `uid=` / `gid=`（ossfs） | 挂载时把所有文件**呈现**为该属主，不写入 OSS |
| `umask=`（ossfs） | 呈现的权限位：`umask=022` → 目录 755 / 文件 644 |
| `volumeAttributes` | PV 的 CSI 挂载参数，**创建后不可变**，改它必须重建 PV |
| `otherOpts` | StorageClass 参数，controller 建 PV 时透传为 ossfs 挂载选项 |
