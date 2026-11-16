# Workspace 非 Root 化迁移 Runbook（OSS 存储）

> **适用场景**：数据持久化在 `oss://<bucket>/workspaces/<ws>/` 的集群。  
> **核心目标**：workspace Pod 内以**高位专属非 root 用户（推荐 UID/GID `10001`）**运行，消除容器逃逸风险与 root 特权滥用。  
> **核心特性**：
> 1. **家目录与路径零变动**：容器内 `~` 与 `$HOME` 依然是 `/root`，Agent 读写 `/root/.bocomwork`、`/root/.config` 源码与脚本**无需改动任何路径**。
> 2. **数据零迁移**：OSS 对象存储不存 POSIX 属主，通过 ossfs 挂载参数实时合成属主，**无需 initContainer，无需重新搬迁 OSS 数据**。
> 3. **避免 UID 冲突**：不使用易与宿主机默认普通用户（如 `ubuntu`）或 Docker 服务冲突的 `1000`，改用云原生标准的专属高位 UID（`10001`）。
> 4. **渐进式灰度**：基于 Workspace 注解驱动，一次只改一个变量，任意阶段支持秒级回滚。

---

## 0. 关键认知与前置答疑

### 0.1 为什么避开 UID 1000，改用 10001？
* **宿主机冲突隐患**：Ubuntu/Debian 等主流 Linux 宿主机在创建第一个普通登录用户时，默认起始 UID 就是 `1000`；部分环境中的 `docker` 用户组或服务账号也会占用 `1000` 或 `999`。如果以后涉及节点级权限或文件交汇，使用 `1000` 容易发生属主混淆。
* **高位专属 UID 规范**：行业通用规范（如 Google Distroless 的 `10001`，或专用应用 UID 如 `20000`）采用高位 UID 彻底避开系统内置保留段。本 Runbook 统一以 **`10001`** 作为标准示范（集群若有既定规范，只需保证全局统一即可）。

### 0.2 能不能保持 `~` 就是 `/root`？Agent 存的文件需要迁移吗？
* **完全可以！** Linux 下 `~` 的展开取决于 `$HOME` 环境变量和 `/etc/passwd`，没有任何系统规则禁止非 root 用户将 `/root` 设为家目录。
* 只要镜像内执行 `chmod 755 /root && chown -R 10001:10001 /root`，非 root 进程即可自由进出并在 `/root` 下工作。
* **数据完全不需要迁移**：OSS 存储里的文件属主完全由 ossfs 挂载参数（`uid=10001,gid=10001`）在挂载点上**动态合成**。在容器看来，挂载进来的 `/root/.bocomwork` 天然就属于 UID 10001，直接具备读写权限。

### 0.3 为什么不用 initContainer？
* 对象存储本身没有 Linux 权限位，在 initContainer 中跑 `chown -R` 对 OSS 对象是无效的，反而会在启动时因 FUSE 大量写元数据导致容器启动严重超时。
* **改挂载参数是让 OSS 文件呈现给指定 UID 的唯一且最高效的手段。**

### 0.4 为什么存量 PV 必须重建？（已实测验证）
```text
PersistentVolume "ws-x-pv" is invalid: spec.persistentvolumesource:
Forbidden: spec.persistentvolumesource is immutable after creation
```
由于 Kubernetes 的 `csi.volumeAttributes` 属于不可变字段，修改 StorageClass 只对新建的 PV 生效；**存量 PV 需通过“停止 -> 删除 PVC/PV -> 启动触发重建”的方式生效**。因为 PV 回收策略是 `Retain`，数据一直在 OSS 上，重建 PV 不会丢失任何业务数据。

---

## 1. 目标状态架构

```text
┌──────────────────────── Pod (runAsUser=10001, HOME=/root) ────────────────────────┐
│  agent 进程 UID/GID = 10001, HOME=/root, echo ~ 展开为 /root                      │
│  可正常读写 /root/.bocomwork，无任何路径迁移成本                                    │
│  seccomp: RuntimeDefault, allowPrivilegeEscalation: false                         │
└──────────────────────────────────────┬────────────────────────────────────────────┘
                                       │ 挂载点（由 ossfs 合成 uid=10001,gid=10001,umask=022）
                                       ▼
    oss://<bucket>/workspaces/<ws>/ ── workspace/ bocomwork/ config/ share/ state/ cache/
```

| 治理层级 | 改动内容 | 变更位置 |
|---|---|---|
| **镜像层** | 赋予 `/root` 给 10001 权限，保持 `HOME=/root`，预建系统目录 | Agent 基础镜像 Dockerfile |
| **存储层** | SC `otherOpts` 增加 `-o uid=10001,gid=10001,umask=022` | StorageClass 配置 |
| **PV 层** | 存量 PV 逐个平滑重建以加载新参数（OSS 数据不动） | K8s 资源调度 |
| **控制器** | 支持基于注解 `ai.example.com/run-as-user` 动态注入 SecurityContext | `workspace_controller.go`（§3） |

---

## 2. 前置检查（只读）

在正式开始前，在跳板机上确认以下环境信息：

```bash
export NS=bocomwork
export BUCKET='<your-bucket>'
export SC=alicloud-oss
export APP_UID=10001

# 2.1 确认 OSS StorageClass 当前挂载参数
kubectl get sc $SC -o yaml | grep -A 5 "parameters:"

# 2.2 抽查存量 Workspace 的 OSS 挂载路径
export WS=ws-<ID>
kubectl -n $NS get pvc ${WS}-pvc -o jsonpath='{.spec.volumeName}{"\n"}'
kubectl get pv ${WS}-pv -o jsonpath='{.spec.csi.volumeAttributes.path}{"\n"}'
# 期望返回: /workspaces/$WS

# 2.3 记录 OSS 对象数（对比基线，确保无数据变动）
ossutil ls -s "oss://$BUCKET/workspaces/$WS/" | tail -5 | tee /tmp/oss-count-before.txt

# 2.4 查看当前 Pod 内运行用户与 HOME 现状
kubectl -n $NS exec deploy/${WS}-deploy -- sh -c 'id; echo HOME=$HOME; ls -ld /root; ls -ld /root/.bocomwork 2>/dev/null'
```

---

## 3. 控制器改造：注解灰度开关

为确保**平滑灰度、单点可控、支持秒级回滚**，不采用一刀切的全局写死方式，而是引入注解控制：**不加注解保持历史 root 行为，显式加注解才切换非 root**。

### 3.1 代码实现 (`internal/controller/workspace_controller.go`)

在常量定义区新增：
```go
// runAsUserAnnotation opts a single Workspace into running as a non-root UID.
// Absent annotation keeps the historical root behaviour, so the migration can be
// canaried per Workspace and rolled back without redeploying the controller.
const runAsUserAnnotation = "ai.example.com/run-as-user"
```

在 `reconcileDeployment` 方法中，获取 `podSecurityContext, containerSecurityContext := buildWorkspaceSecurityContexts()` 之后加入解析逻辑：

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

*说明：更新路径已具备 `DeepEqual(podSpec.SecurityContext, podSecurityContext)` 自动收敛，存量 Deployment 会在加/减注解时自动同步并重新调度，无需手动修改 Deployment。*

### 3.2 编译与更新部署
```bash
make manifests generate && make test
IMG=<registry>/workspace-operator:<tag> && make docker-build docker-push IMG=$IMG
kubectl -n agent-deploy-system set image deploy/agent-deploy-controller-manager manager=$IMG
kubectl -n agent-deploy-system rollout status deploy/agent-deploy-controller-manager
```

---

## 4. 镜像适配（Agent Image）

保证 `~` 依然指向 `/root`，避免路径迁移与用户混淆。

### Dockerfile 关键调整：
```dockerfile
# 1) 创建高位专属 UID 10001 用户，家目录依然指定为 /root
RUN groupadd -g 10001 agent && \
    useradd -u 10001 -g 10001 -d /root -s /bin/bash agent

# 2) 【核心】开放 /root 权限，使得 10001 用户可自由进出和读写
#    Linux 默认 /root 是 0700，必须赋权否则普通用户无法进入 ~
RUN chmod 755 /root && \
    chown -R 10001:10001 /root

# 3) 预建常用的内部目录（防止运行时由于父目录权限无法创建）
RUN mkdir -p /root/.bocomwork /root/.config /root/.local /root/.cache && \
    chown -R 10001:10001 /root/.bocomwork /root/.config /root/.local /root/.cache

# 4) 显式固定 HOME 环境变量为 /root
ENV HOME=/root
USER 10001:10001
WORKDIR /root
```

**自测要点**：
* 本地构建后运行 `docker run --rm -it <image> bash`
* 执行 `id` 输出应为 `uid=10001(agent) gid=10001(agent)`
* 执行 `echo ~` 和 `echo $HOME` 必须输出 `/root`
* 执行 `touch ~/.bocomwork/test.txt && rm ~/.bocomwork/test.txt` 验证成功

---

## 5. StorageClass 挂载参数调整

调整 StorageClass 的 `otherOpts`，增加 OSS 客户端属主映射参数。

```bash
# 5.1 备份现有 StorageClass
kubectl get sc $SC -o yaml > backup/sc-$SC-$(date +%F-%H%M).yaml

# 5.2 Patch 注入 uid=10001,gid=10001,umask=022
kubectl patch sc $SC --type merge -p '{"parameters":{"otherOpts":"-o max_stat_cache_size=100000 -o allow_other -o uid=10001,gid=10001,umask=022"}}'

# 5.3 校验生效
kubectl get sc $SC -o jsonpath='{.parameters.otherOpts}{"\n"}'
# 期望输出: -o max_stat_cache_size=100000 -o allow_other -o uid=10001,gid=10001,umask=022
```

*注意：此变更对新创建的 PV 自动生效。存量 PV 需在下一步中执行重新绑定。*

---

## 6. 分阶段灰度执行

遵循“**一次只动一个变量**”原则：
* **S1**：全新租户验证（端到端打通）
* **S2**：存量单个空间更新 PV 挂载参数（容器暂不切非 root，验证挂载与数据完整性）
* **S3**：存量单个空间打注解切非 root（验证业务逻辑与读写）
* **S4**：批量推广上线

### S1：新建测试工作空间验证（全新环境）

```bash
kubectl -n $NS apply -f - <<'EOF'
apiVersion: ai.example.com/v1alpha1
kind: Workspace
metadata:
  name: ws-canary-10001
  namespace: bocomwork
  annotations:
    ai.example.com/run-as-user: "10001"
spec:
  owner: canary-tester
  runtime:
    image: <your-registry>/bocomwork:<new-10001-tag>
    cpu: "1"
    memory: "2Gi"
  storage:
    size: "1Gi"
EOF

# 验证就绪
kubectl -n $NS wait --for=condition=Available deploy/ws-canary-10001-deploy --timeout=300s

# 验证容器内身份与路径
kubectl -n $NS exec deploy/ws-canary-10001-deploy -- id
# 期望: uid=10001 gid=10001

kubectl -n $NS exec deploy/ws-canary-10001-deploy -- sh -c 'echo ~; echo $HOME'
# 期望均为: /root

kubectl -n $NS exec deploy/ws-canary-10001-deploy -- sh -c 'touch ~/.bocomwork/.write_test && rm ~/.bocomwork/.write_test && echo WRITE_OK'
# 期望输出: WRITE_OK
```

### S2：存量空间重建 PV（保持 root，仅变更 OSS 映射）

选择一个存量 Workspace（如 `ws-test01`）：

```bash
export WS=ws-test01

# 1) 停止工作空间（副本缩为 0，释放 OSS 卷挂载占用）
kubectl -n $NS patch workspace $WS --type merge -p '{"spec":{"stopped":true}}'
kubectl -n $NS wait --for=delete pod -l "app=workspace,workspace=$WS" --timeout=300s

# 2) 删除旧参数的 PVC 和 PV（OSS 数据保留在远端，不受任何影响）
kubectl -n $NS delete pvc ${WS}-pvc
kubectl delete pv ${WS}-pv

# 3) 启动工作空间（触发 Controller 使用新的 SC 参数自动重新创建 PV & PVC）
kubectl -n $NS patch workspace $WS --type merge -p '{"spec":{"stopped":false}}'

# 4) 等待 PV 重建并确认包含 uid=10001
until kubectl get pv ${WS}-pv -o jsonpath='{.spec.csi.volumeAttributes.otherOpts}' 2>/dev/null | grep -q 'uid=10001'; do
  sleep 3
done
echo "PV_REBUILT_WITH_10001_OK"

# 5) 数据核对（此时由于仍是 root 启动，验证数据是否完好无损）
kubectl -n $NS exec deploy/${WS}-deploy -- sh -c 'ls -ln /root/.bocomwork | head -10'
```

### S3：存量空间启用非 root 用户运行

```bash
# 打上注解切换到 UID 10001
kubectl -n $NS annotate workspace $WS ai.example.com/run-as-user="10001" --overwrite

# 等待 Deployment 滚动更新就绪
kubectl -n $NS rollout status deploy/${WS}-deploy --timeout=300s

# 进入终态验证
kubectl -n $NS exec deploy/${WS}-deploy -- id
# 期望: uid=10001 gid=10001

kubectl -n $NS exec deploy/${WS}-deploy -- sh -c 'echo ~; echo $HOME'
# 期望: /root /root

kubectl -n $NS exec deploy/${WS}-deploy -- sh -c 'ls -ld ~/.bocomwork; touch ~/.bocomwork/.canary && rm ~/.bocomwork/.canary && echo PASS'
# 期望: PASS
```

### S4：批量执行上线（使用脚本安全受控执行）

针对批量迁移，利用暂停 Controller 的方式规避删除与重建时的调度竞态：

```bash
# 准备目标清单
cat <<EOF > ws-batch-list.txt
ws-user01
ws-user02
ws-user03
EOF

# A. 批量停止
for W in $(cat ws-batch-list.txt); do
  kubectl -n $NS patch workspace $W --type merge -p '{"spec":{"stopped":true}}'
done
kubectl -n $NS wait --for=delete pod -l app=workspace --timeout=600s

# B. 临时暂停 Operator，消除重建竞态
kubectl -n agent-deploy-system scale deploy/agent-deploy-controller-manager --replicas=0

# C. 批量清除旧 PV/PVC
for W in $(cat ws-batch-list.txt); do
  kubectl -n $NS delete pvc ${W}-pvc --ignore-not-found
  kubectl delete pv ${W}-pv --ignore-not-found
  # 同步打上注解
  kubectl -n $NS annotate workspace $W ai.example.com/run-as-user="10001" --overwrite
done

# D. 恢复 Operator 自动重建并批量启动
kubectl -n agent-deploy-system scale deploy/agent-deploy-controller-manager --replicas=1

for W in $(cat ws-batch-list.txt); do
  kubectl -n $NS patch workspace $W --type merge -p '{"spec":{"stopped":false}}'
done
```

---

## 7. 秒级回滚方案

如果在非 root 化过程中遇到业务报错或不兼容，各阶段均提供独立、确定的回滚指令：

### 7.1 单个 Workspace 紧急回滚为 root 运行（**耗时 < 30 秒**）
只需移除注解，Controller 调谐器会自动将 Pod 恢复为原有的 root 启动模式：
```bash
kubectl -n $NS annotate workspace $WS ai.example.com/run-as-user-
```
> **原理保底**：因为 OSS 挂载设置了 `uid=10001`，当回滚为 root (uid=0) 启动时，由于 root 原生具备 `CAP_DAC_OVERRIDE` 能力，依然能无障碍读写 10001 属主的文件，因此**无需回退存储参数即可立即恢复业务**。

### 7.2 挂载参数彻底回退
如果需要将 StorageClass 和 PV 完全恢复为旧版无 uid/gid 模式：
```bash
# 1. 恢复 StorageClass
kubectl apply -f backup/sc-$SC-<timestamp>.yaml

# 2. 对指定工作空间重复 S2 的“停 -> 删 PVC/PV -> 启动”操作，PV 自动按原参数重建
```

---

## 8. 终态验收清单

| 验收项 | 检查命令 | 预期输出 | 说明 |
|---|---|---|---|
| **生效 UID** | `kubectl exec -it <pod> -- id` | `uid=10001 gid=10001` | 已脱离 root 特权 |
| **家目录展开** | `kubectl exec -it <pod> -- sh -c 'echo ~'` | `/root` | `~` 完全保持历史路径 |
| **环境变量** | `kubectl exec -it <pod> -- sh -c 'echo $HOME'` | `/root` | 保证各类 cli 依赖正常寻找配置 |
| **业务数据可写** | `kubectl exec -it <pod> -- touch ~/.bocomwork/.p && rm ~/.bocomwork/.p` | 返回成功 | OSS 卷属主正确合成映射 |
| **配置目录可写** | `kubectl exec -it <pod> -- touch ~/.config/.p && rm ~/.config/.p` | 返回成功 | 镜像层授权生效 |
| **防提权** | `kubectl exec -it <pod> -- cat /proc/self/status \| grep NoNewPrivs` | `NoNewPrivs: 1` | `allowPrivilegeEscalation: false` |
| **容器能力** | `kubectl exec -it <pod> -- capsh --print` | 无 `CAP_SYS_ADMIN` 等危险项 | 最小特权集生效 |
| **对象完整性** | `ossutil ls -s "oss://$BUCKET/workspaces/$WS/"` | 对象数与迁移前完全一致 | OSS 存储数据未受损 |
