# Workspace 非 Root 化迁移 Runbook（OSS 存储）

> **目标**：所有 workspace（存量 + 新建）的 Pod 都以非 root（UID/GID `10001`）运行，且 `drop ALL` capabilities。
> **适用**：数据在 `oss://<bucket>/workspaces/<ws>/`，OSS CSI（ossfs 1.0）。

---

## ⛔ 先读这个：存量 PV / PVC 必须删除重建

> ### 🔴 回答：**是的，所有存量 workspace 的 PVC 和 PV 都必须删除重建，没有别的办法。**
> `csi.volumeAttributes`（其中的 `otherOpts` 决定 ossfs 的 `uid/gid`）在 PV 创建后**不可修改**；改 StorageClass **只影响以后新建的 PV**。
> 不重建的 workspace：operator 检测到 PV 缺少 `uid=10001,gid=10001`，**会保持 root 运行**（并发 Warning Event `RunAsUserSkipped`），不会坏，但也**不会非 root**。
>
> ### 🔴 删除操作的铁律（逐条遵守）
> 1. **只删「已停止、Pod 已消失」的 workspace 的 PVC/PV。** 删除运行中 workspace 的 PVC/PV 会让挂载中的 Pod 读写失败。
> 2. **顺序：先 PVC，后 PV。**（PVC 有保护 finalizer，Pod 未退出会一直 Terminating。）
> 3. **PV 名称必须从 PVC 的 `spec.volumeName` 取，不要手拼**；删前核对 `volumeAttributes.path` 就是 `/workspaces/<该 ws>`，确认没删错别人的卷。
> 4. **绝不要把 PV 的 `persistentVolumeReclaimPolicy` 改成 `Delete`。** 现在是 `Retain`：删除 PV 只删 K8s 对象，**OSS 里的数据不会删**。改成 Delete 会连 OSS 数据一起清掉。
> 5. 一次只试一个 workspace（S2），确认数据完整、能启动，再批量。
> 6. 删除前先备份：`kubectl get pvc,pv ... -o yaml > backup/`。

---

## 1. 方案总览（默认策略）

不再依赖逐个打注解。**operator 带全局默认 UID**（启动参数 `--default-run-as-user=10001`），对每个 workspace 的 Pod 模板：

> **当前提交的配置默认仍是 root**：`config/manager/manager.yaml` 和 `deploy/install.yaml` 里是 `--default-run-as-user=0`，只发布新代码不会有任何行为变化。
> **切换为非 root 的开关就是 §2 第 3 步把这个值从 `0` 改成 `10001`**；改回 `0` 即全局回退。


| 条件 | 结果 |
|---|---|
| PV 是 OSS 且 `otherOpts` 含 `uid=10001` 与 `gid=10001` | **非 root**：`runAsUser/Group=10001`、`runAsNonRoot`、`capabilities.drop=[ALL]`、`allowPrivilegeEscalation=false`、不设 fsGroup |
| PV 不满足（旧 PV / 非 OSS / 还没绑定） | **保持镜像用户（root）**，打日志 + Warning Event `RunAsUserSkipped`，**不报错、不重启** |
| 注解 `ai.example.com/run-as-user: "0"` | 该 workspace **显式退出**，保持 root（单个回滚开关） |
| 注解为其他正整数 | 以该 UID 运行（同样要求 PV 带对应 uid/gid） |
| 注解非法 | 忽略，使用默认值 |
| 查询 PVC/PV 临时失败 | 本轮 reconcile 报错重试，**不会回退成 root**（避免 Pod 模板来回翻转导致重启） |

因此新建的 workspace：SC 带 uid → operator 自动建的 PV 带 uid → 自动非 root。存量 workspace：重建 PV 后，**下次 Pod 模板被 reconcile 修改（唤醒）时**自动非 root。**不需要任何打注解操作**（`"0"` 仅作回滚）。

> 默认策略只在「会修改 Pod 模板的 reconcile」生效（`workspacePredicate` 忽略注解变化；休眠路径不改模板）。已运行的 Pod 不会被中途重启，等下次启动生效即可。

### 1.1 迁移进度可视化

`Workspace.status.runAsUser` 记录 **Deployment Pod 模板**的 UID（0 = root/镜像用户，>0 = 非 root），`kubectl get ws` 多了 `RunAs` 列：

```bash
export NS=bocomwork
kubectl -n $NS get workspace                      # RunAs 列：0=仍是 root，10001=非 root，空=Deployment 尚未创建
# 统计
kubectl -n $NS get workspace -o json | jq -r '
  .items | group_by(.status.runAsUser // "unset")
  | map({runAs: (.[0].status.runAsUser // "unset"), count: length})'
# 还是 root 的 workspace 清单
kubectl -n $NS get workspace -o json | jq -r '.items[] | select((.status.runAsUser // 0)==0) | .metadata.name'
# 已非 root 的清单
kubectl -n $NS get workspace -o json | jq -r '.items[] | select((.status.runAsUser // 0)>0) | .metadata.name'
```

**注意**：`RunAs` 反映的是 *Pod 模板*。刚重建 PV、还没唤醒的 workspace 仍显示 0；唤醒后变 10001。运行中 Pod 的真实身份以 `kubectl debug`/`id` 为准。

「PV 还没重建」的存量卷（决定谁需要做 S2）：

```bash
kubectl get pv -o json | jq -r '.items[]
  | select(.spec.csi.driver=="ossplugin.csi.alibabacloud.com")
  | select((.spec.csi.volumeAttributes.otherOpts // "") | test("uid=10001") | not)
  | .metadata.name'
# 被跳过的原因
kubectl -n $NS get events --field-selector reason=RunAsUserSkipped
```

---

## 2. 发布顺序（必须按序）

1. **镜像**（`agent-work` 的 `Dockerfile.musl/glibc` + `dist/Dockerfile` 二次构建，见 §4），并让**所有** workspace 先用上新镜像。
   *旧镜像没有 10001 属主的 `/root`、`/app`，如果旧镜像 + 已重建 PV → 非 root 会启动失败。operator 不检查镜像，这一步靠流程保证。*
2. **StorageClass** 追加 uid/gid（§5）。之后新建 workspace 的 PV 才带 uid。
3. **operator 发新版本**（`--default-run-as-user=0`，仍是 root，无行为变化），再**打开开关：把 `0` 改成 `10001`**：
   ```bash
   # 方式 A：改 deploy/install.yaml 中 manager 容器 args 的这一行，然后 kubectl apply -f deploy/install.yaml
   #           - --default-run-as-user=0      →      - --default-run-as-user=10001
   #   同时把 config/manager/manager.yaml 里同一行也改成 10001，否则下次 make build-installer 会改回 0
   # 方式 B：线上直接 patch（args 下标需先确认；本仓库 manager 容器里该参数是第 4 个，即 /args/3）
   kubectl -n agent-deploy-system get deploy agent-deploy-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}'
   kubectl -n agent-deploy-system patch deploy agent-deploy-controller-manager --type json \
     -p '[{"op":"replace","path":"/spec/template/spec/containers/0/args/3","value":"--default-run-as-user=10001"}]'
   kubectl -n agent-deploy-system rollout status deploy/agent-deploy-controller-manager
   ```
   （方式 B 的容器下标 `0` 需先确认是 manager 容器。）2 副本滚动，期间旧 leader 不认识新策略，但没有任何 Pod 模板被改动，无影响。
   此时所有 PV 仍是旧的存量 workspace 保持 root，**不会批量重启**。
4. **逐批重建存量 PV**（§6）。

---

## 3. 代码实现（已落地，未提交）

`internal/controller/workspace_controller.go`

- `Reconciler.DefaultRunAsUser` ← `--default-run-as-user`（0 = 关闭默认）。
- `resolveRunAsUser`：默认值/注解解析 + `volumeBlocksRunAsUser`（PVC 存在且已绑定 → PV 是 OSS CSI → `otherOpts` 含匹配 `uid=`/`gid=`）。
- `applyNonRoot`：`RunAsUser=RunAsGroup=uid`、`RunAsNonRoot=true`、`Capabilities{Drop:[ALL]}`（不设 Add，**不设 FSGroup**：OSS 卷 RWX 且 CSIDriver `fsGroupPolicy=ReadWriteOnceWithFSType`，fsGroup 本就不会生效）。
- 兜底建 PV（SC 无 `otherOpts` 时）也会追加 `uid/gid/umask`；SC 显式配置时以 SC 为准。
- `status.runAsUser` + `RunAs` printcolumn；缓存里的 Deployment 保留 Pod 级 SecurityContext 用于该字段（其余仍剥离）。
- 单元测试：`internal/controller/workspace_runasuser_test.go`（注解解析、applyNonRoot、otherOpts 解析、PV 守卫全分支）。

---

## 4. 镜像（已改）

- `agent-work/Dockerfile.musl`、`Dockerfile.glibc`：创建 10001 用户（**不写 `USER`**）、预建 `/root/*` 目录、`chmod 755 /root`、`chown -R 10001 /root`、`chown 10001 /app`、`HOME=/root`。
- `dist/Dockerfile`：`COPY --chown=10001:10001` dist/、opencode、APPEND_SYSTEM.md，`chown /root/.pi/agent`。
- `dist/move.sh`：`cp -a` 改为 `cp`（非 root 无法保留属主，busybox 可能报错）。
- `dist/workspace.json`：NAS 共享卷 `readOnly: true`。

> **不要在镜像写 `USER 10001`**：是否非 root 只由 operator 决定。固化 USER 会绕过「PV 未重建就保持 root」的保护，旧 PV 下直接写入失败。

自测：`docker run --rm --user 10001:10001 <image> sh -c 'id; echo ~; touch ~/.config/.t /app/.t'`，并完整跑 `apikey.sh && move.sh && start-prod.sh`（`move.sh` 要覆盖「有新 tar / 新 opencode」分支）。

---

## 5. StorageClass

`deploy-ack/storageclass.yaml`、`deploy/oss/storageclass.yaml` 已加 `-o uid=10001,gid=10001,umask=022`。线上执行：

```bash
export SC=alicloud-oss
kubectl get sc $SC -o yaml > backup/sc-$SC-$(date +%F-%H%M).yaml
kubectl get sc $SC -o jsonpath='{.parameters.otherOpts}{"\n"}'   # 先看线上现值！patch 会整体覆盖
kubectl patch sc $SC --type merge -p '{"parameters":{"otherOpts":"-o max_stat_cache_size=100000 -o allow_other -o uid=10001,gid=10001,umask=022"}}'
```

> K8s 中 StorageClass 的 `parameters` **创建后不可变**，上面的 `patch` 大概率会被拒绝。此时需要 `kubectl delete sc` 后用新 yaml `kubectl apply`（SC 删除不影响已存在的 PV/PVC，只影响新建）。操作前务必备份。

此变更**只对新建的 PV 生效**，这也是 §6 要删除重建的原因。

---

## 6. 存量 workspace 重建 PV（夜间窗口）

生产在夜间操作，此时 running 的 workspace 很少，策略：**先停 → 删 → 启动**。休眠/已停止的 workspace 没有 Pod，可直接进入删除步骤。

### S1：新建 workspace 验证
SC 与 operator 已就绪后新建一个 workspace，应 `RunAs=10001`，且 `kubectl debug` 进入后 `id` 为 10001、能写 `~/.bocomwork`。

### S2：单个存量 workspace

```bash
export WS=ws-xxx
# 0) 备份
mkdir -p backup
kubectl -n $NS get pvc ${WS}-pvc -o yaml > backup/${WS}-pvc.yaml
PV=$(kubectl -n $NS get pvc ${WS}-pvc -o jsonpath='{.spec.volumeName}')
kubectl get pv $PV -o yaml > backup/${WS}-pv.yaml
kubectl get pv $PV -o jsonpath='{.spec.persistentVolumeReclaimPolicy} {.spec.csi.volumeAttributes.path}{"\n"}'
# 必须输出: Retain /workspaces/<WS>   —— 不是就停手

# 1) 停止并确认 Pod 已消失
kubectl -n $NS patch workspace $WS --type merge -p '{"spec":{"stopped":true}}'
kubectl -n $NS wait --for=delete pod -l "app=workspace,workspace=$WS" --timeout=300s

# 2) ⛔ 删除（先 PVC 后 PV）。仅删 K8s 对象，OSS 数据保留
kubectl -n $NS delete pvc ${WS}-pvc
kubectl delete pv $PV

# 3) 启动：operator 按新 SC 重建 PV/PVC
kubectl -n $NS patch workspace $WS --type merge -p '{"spec":{"stopped":false}}'
kubectl get pv ${WS}-pv -o jsonpath='{.spec.csi.volumeAttributes.otherOpts}{"\n"}'   # 应含 uid=10001,gid=10001

# 4) 验证
kubectl -n $NS get workspace $WS           # RunAs=10001
kubectl -n $NS get events --field-selector involvedObject.name=$WS | grep RunAsUserSkipped   # 不应有
```

> 若 operator 在你删除 PVC/PV 后、启动前就把它们按新 SC 重建了，是正常且期望的行为（新 PV 带 uid）。
> 步骤 3 后 `RunAs` 仍为 0：看 Warning Event，多半是 SC 还没改 / PV 没重建 / 镜像未更新。

### S3：批量（夜间）

```bash
# ws-batch-list.txt：每行一个已停止或休眠的 workspace（可先用 §1.1 的 root 清单）
for W in $(cat ws-batch-list.txt); do
  kubectl -n $NS patch workspace $W --type merge -p '{"spec":{"stopped":true}}'
done
for W in $(cat ws-batch-list.txt); do
  kubectl -n $NS wait --for=delete pod -l "app=workspace,workspace=$W" --timeout=600s
done
# ⛔ 删除段：确认上面全部 Pod 已消失再执行
for W in $(cat ws-batch-list.txt); do
  PV=$(kubectl -n $NS get pvc ${W}-pvc -o jsonpath='{.spec.volumeName}' 2>/dev/null)
  kubectl -n $NS delete pvc ${W}-pvc --ignore-not-found
  [ -n "$PV" ] && kubectl delete pv "$PV" --ignore-not-found
done
# 启动（也可用 cmd/api-server 的批量重启管理页分批拉起，避免集中冷启动）
for W in $(cat ws-batch-list.txt); do
  kubectl -n $NS patch workspace $W --type merge -p '{"spec":{"stopped":false}}'
done
```

不需要缩容 operator：workspace 已停止，operator 重建 PV 用的就是新 SC。**休眠(Sleeping)的 workspace 没有 Pod，可不唤醒**，只做删除，等用户下次使用时自动非 root（但进度表里会一直显示 0，直到唤醒）。需要尽快把进度推满，用批量重启工具分批拉起。

---

## 7. 回滚

- **单个 workspace**：`kubectl -n $NS annotate workspace $WS ai.example.com/run-as-user="0" --overwrite`，下次启动（停→启）生效，回到 root。root 具备 `DAC_OVERRIDE`，可读写 uid=10001 属主的文件，**不需回退存储**。
- **全局**：operator 去掉 `--default-run-as-user`（或设 0）后，下一次模板被 reconcile 时回到 root；同样无需动 PV。
- **彻底回退挂载参数**：恢复 SC 备份，再对目标 workspace 重复 S2（⛔ 同样的删除铁律）。

---

## 8. 迁移后行为变化（有影响的操作，全部在这里）

| 变化 | 影响 / 做法 |
|---|---|
| `kubectl exec` 进容器现在是 **10001**，不再是 root | 需要 root 排查用 `kubectl debug -it <pod> --target=<container> --image=<img> -- sh`（临时容器仍受集群策略约束）。`exec` 里 `apk add` / `apt install` / 改 `/etc`、`/usr` 均失败 |
| `npm i -g`、`pip install`（全局）失败 | 全局目录 root 属主。可用 `pip install --user`（落到 `~/.local`，不在 OSS 卷上，**重启丢失**）。需要固定工具请进镜像 |
| `postStartScript` 以 10001 执行 | 脚本里写 `/etc`、`/usr`、装包、绑 <1024 端口会失败 |
| `capabilities` 全部丢弃（`drop: ALL`） | 不能 `chown` 到其他用户、`setuid/setgid`、`ping`(raw socket)、绑特权端口。应用端口 3001/25808 不受影响 |
| `allowPrivilegeEscalation=false`（NoNewPrivs） | `sudo`、setuid 程序不可用 |
| 文件属主 | OSS 卷内文件一律显示 `10001:10001`、权限由 `umask=022` 合成（目录 755/文件 644）。`chown/chmod` 在 OSS 卷上无效或被忽略 |
| 回滚到 root 后 | OSS 卷依旧 10001 属主，root 靠 `DAC_OVERRIDE` 仍可读写；但镜像层 `/root` 下新建文件会是 root 属主 |
| `/root`、`/app` 之外的镜像目录不可写 | 只有 `/root`（整树）、`/app` 目录本身、`/tmp`、OSS 卷可写。agent 写其他路径会报权限错误 |
| 热更新（`move.sh`） | 只在 `/app/out`、`/app/packages`、`/usr/local/bin/opencode` 上操作，均已归 10001。若以后新增热更新目标路径，需同步在镜像里 `chown` |
| NAS 共享卷 `/opt/bocom-defaults` 改为只读 | 不能在 workspace 内往里写。发布新版本需在 NAS 侧（别处）替换文件；`move.sh` 只读取（先 copy 到别的路径，不在共享卷上原地改） |
| 旧镜像 + 已重建（带 uid）的 PV | 非 root 会因 `/root`/`/app` 属主是 root 而启动失败。**务必先完成镜像升级**（§2） |
| 新建 workspace 使用了非 OSS 存储 | 保持 root（守卫），Event 会提示。目前仅 OSS 受支持 |
| `kubectl get ws` 新增 `RunAs` 列 | CRD 新增 `status.runAsUser`，需要 `make manifests` 后 apply 新 CRD |
| operator 内存缓存 | Deployment 缓存多保留一个小字段（Pod SecurityContext），可忽略 |

---

## 9. 终态验收

| 项 | 命令 | 预期 |
|---|---|---|
| 进度 | `kubectl -n $NS get workspace` | `RunAs=10001` |
| 无 skip 事件 | `kubectl -n $NS get events --field-selector reason=RunAsUserSkipped` | 空 |
| UID | `kubectl -n $NS exec deploy/${WS}-deploy -- id` | `uid=10001 gid=10001` |
| HOME | `... -- sh -c 'echo ~'` | `/root` |
| OSS 可写 | `... -- sh -c 'touch ~/.bocomwork/.p && rm ~/.bocomwork/.p'` | 成功 |
| 配置目录可写 | `... -- sh -c 'touch ~/.config/.p && rm ~/.config/.p'` | 成功 |
| NoNewPrivs | `... -- grep NoNewPrivs /proc/self/status` | `NoNewPrivs: 1` |
| 能力 | `... -- grep Cap /proc/self/status` | `CapEff/CapPrm/CapBnd` 全 0 |
| OSS 数据 | `ossutil ls -s oss://$BUCKET/workspaces/$WS/` | 与迁移前一致 |
