# Agent 工作空间存储迁移指南：本地物理盘 (Local Path) 至 NAS/NFS 迁移

本文档为您提供了将平台存储底座从**本地物理磁盘 (Local Path)** 平滑迁移至 **NAS / NFS 网络共享存储**的完整操作指南。

系统架构设计原生支持多 StorageClass 并存与无缝切换。迁移过程可以**按用户逐个渐进式进行**，也可以全量迁移，确保业务风险可控。

---

## 1. 迁移架构与协同工作流

```mermaid
graph TD
    subgraph 迁移前: 本地物理盘架构
        PodA[Agent Pod] -->|独占存储| LocalPV[Local Path 物理盘 /data/local-path-storage]
        PodA -->|共享存储| LocalShare[HostPath 物理盘 /data/bocomwork-share]
    end

    subgraph 数据同步阶段 (rsync)
        LocalPV -->|1. 停机增量同步| NASStorage[NAS 物理存储服务器]
        LocalShare -->|2. 在线增量同步| NASStorage
    end

    subgraph 迁移后: NAS 架构
        PodB[Agent Pod] -->|独占存储| NAS_PV[NFS PVC / standard StorageClass]
        PodB -->|共享存储| NAS_Share[NFS 共享 PVC / bocomwork-share]
    end
```

---

## 2. 准备工作：切换集群默认存储类

若要将后续**新创建的 Workspace** 默认存储重新切回 NAS，只需在 K8s 集群控制端执行以下 StorageClass 默认注解切换命令：

```bash
# 1. 移除 local-path 的默认存储类注解
kubectl annotate storageclass local-path storageclass.kubernetes.io/is-default-class- --overwrite

# 2. 将老的 NAS 存储类 (standard) 设置为集群默认存储类
kubectl annotate storageclass standard storageclass.kubernetes.io/is-default-class="true" --overwrite
```

*验证结果*：运行 `kubectl get sc`，确认 `standard` 带有 `(default)` 标记。

---

## 3. 场景一：公共共享空间迁移 (工具包共享目录)

公共共享空间存储公共工具包（如 `skill`、`bocomwork`、`opencodedir`）。由于其特性为**读多写少**，迁移可以实现秒级切换。

### 步骤 1：同步本地物理盘工具包到 NAS

在物理机上使用 `rsync` 命令将本地工具包同步到 NAS 挂载目录中：

```bash
# 保持文件属主 (UID/GID)、权限、时间戳不变 (-a)
rsync -avzP /data/bocomwork-share/ user@nas-server-ip:/nfs/share/bocomwork-share/
```

### 步骤 2：切换 API 请求中的共享 PVC 名称

修改上层 API 请求 Body 或应用配置，将 `sharedVolumeMounts` 的 `pvcName` 由 `bocomwork-local-share` 改回 NAS 的 `bocomwork-share`：

```json
"sharedVolumeMounts": [
  {
    "pvcName": "bocomwork-share",
    "mountPath": "/opt/bocom-defaults/skill/",
    "subPath": "skill"
  },
  {
    "pvcName": "bocomwork-share",
    "mountPath": "/opt/express/bocomwork",
    "subPath": "bocomwork"
  },
  {
    "pvcName": "bocomwork-share",
    "mountPath": "/opt/bocom-defaults/opencodedir/",
    "subPath": "opencodedir"
  }
]
```

---

## 4. 场景二：个人独占空间迁移 (特定 Workspace 的个人私有数据)

每个 Workspace 拥有专属的个人目录（`/workspace` 和 `/data`）。迁移单个用户的独占空间步骤如下：

### 步骤 1：停止/休眠目标 Workspace
防止在迁移过程中 Pod 继续写入数据造成文件损坏。通过 API 将 Workspace 停止（或设置 `spec.stopped: true`）：

```bash
# 查看目标 Pod 状态，确认已完全停止 (replicas 缩为 0)
kubectl get deploy -n bocomwork ws-user01-deploy
```

### 步骤 2：定位物理机上的本地存储路径
在物理节点上找到该 Workspace 对应 PVC 的物理目录：

```bash
# 格式为: /data/local-path-storage/pvc-<UID>_<NAMESPACE>_<PVC_NAME>
ls -ld /data/local-path-storage/pvc-*_bocomwork_ws-user01-pvc
```

### 步骤 3：数据同步至 NAS
将该目录完整复制到 NAS 服务器对于该 PVC 的分配路径下：

```bash
rsync -avzP /data/local-path-storage/pvc-xxx_bocomwork_ws-user01-pvc/ user@nas-server-ip:/nfs/pv-user01/
```

### 步骤 4：重建 PVC 绑定至 NAS
1. 删除旧的 `local-path` PVC：
   ```bash
   kubectl delete pvc ws-user01-pvc -n bocomwork
   ```
2. 创建绑定 NAS 的新 PVC（`storageClassName: standard`）：
   ```yaml
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata:
     name: ws-user01-pvc
     namespace: bocomwork
   spec:
     accessModes:
       - ReadWriteOnce
     storageClassName: standard # 指向 NAS
     resources:
       requests:
         storage: 20Gi
   ```

### 步骤 5：重新拉起 / 唤醒 Workspace
在 API 发送唤醒/启动请求（或设置 `spec.stopped: false`）。容器启动后将自动连接并挂载 NAS 上的最新数据。

---

## 5. PV (PersistentVolume) 的处理与生命周期管理

在迁移过程中，老的本地 PV（PersistentVolume）根据**动态分配**和**静态创建**两种类型，处理方式如下：

### (1) 个人独占空间 PV (动态生成的 `local-path` PV)
* **自动回收机制**：`local-path` 动态创建的 PV 的回收策略 (`reclaimPolicy`) 为 `Delete`。
* **处理动作**：当执行 `kubectl delete pvc ws-user01-pvc` 删除旧 PVC 时，K8s 控制器会**自动随之删除对应的底层 `local-path` PV 资源**。
* **数据安全**：因为在删除 PVC 之前，我们已经通过 `rsync` 将物理盘上的数据完整复制到了 NAS，所以老的 PV 被 K8s 自动清理清理掉是完全安全且符合预期的。
* **新 PV 生成**：重建 PVC 指定 `storageClassName: standard` 后，NAS Provisioner 会在 NAS 上**自动动态申请并生成一个新的 NAS PV** 并与新 PVC 自动绑定。

### (2) 共享空间 PV (静态创建的 `bocomwork-local-share-pv`)
* **Retain 保留机制**：静态声明的 `bocomwork-local-share-pv` 回收策略设置为 `Retain`。
* **处理动作**：
  1. 删除 PVC：`kubectl delete pvc bocomwork-local-share -n bocomwork`
  2. 此时，PV 会变为 `Released` 状态（不会自动删除，物理机文件也完好保留）。
  3. 手动清理废弃的旧 PV 资源：
     ```bash
     kubectl delete pv bocomwork-local-share-pv
     ```
  4. 物理机上的 `/data/bocomwork-share` 目录仍会保留在磁盘上，可以手动 `rm -rf` 释放空间，或保留作为本地备份。

---

## 6. 迁移安全与回滚注意事项

1. **权限一致性 (UID/GID)**：
   容器内用户通常为非 root 用户（如 UID 1000）。使用 `rsync` 迁移时必须带 `-a` 参数，确保拷贝后的文件属主和读写权限完全保持不变，避免容器挂载后抛出 `Permission Denied`。
2. **数据一致性校验**：
   大文件数据迁移完成后，建议运行 `rsync -cn-avzP`（校验模式）进行二次 Hash 对比，确保文件无缺失和损坏。
3. **安全回滚方案**：
   在删除本地 `local-path` 目录前，保留物理机 `/data/local-path-storage/` 中的原数据至少 7 天。若 NAS 在早期运行中出现网络波动或故障，可随时将 PVC 重新挂回本地物理盘。


---

## 7. 场景三：本地物理盘 (Local Path) 至 OSS 迁移

当目标存储底座是阿里云 OSS 时，无需 rsync 与 NAS 中转，可直接用 `ossutil` 把每个 Workspace 的 PVC 目录上传到 OSS 对应前缀，实现按 Workspace 粒度的渐进式迁移。

脚本位置：[hack/migrate-localpath-to-oss.sh](../hack/migrate-localpath-to-oss.sh)

### 7.1 路径对应关系

| 项目 | 位置 |
| --- | --- |
| 本地 PVC 目录 | `/data/local-path-storage/pvc-<UID>_bocomwork_ws-<ID>-pvc/` |
| 本地数据子目录 | `workspace/` `bocomwork/` `bocomcode/` |
| OSS 目标前缀 | `oss://<bucket>/workspaces/ws-<ID>/` |
| OSS 子路径 | `oss://<bucket>/workspaces/ws-<ID>/workspace/` ... |

> `ws-<ID>` 的命名必须与 Workspace CR 的 `metadata.name` 一致，因为 Controller 为该 Workspace 自动创建的 OSS PV 使用的 `subPath` 就是 `/workspaces/<workspace-name>`（见 `internal/controller/workspace_controller.go`）。迁移时务必保持 ID 原样，不要自行改写。

### 7.2 每个 Workspace 执行的动作

```bash
cd /data/local-path-storage/pvc-<UID>_bocomwork_ws-<ID>-pvc
ossutil cp -r -f workspace  oss://<bucket>/workspaces/ws-<ID>/workspace/
ossutil cp -r -f bocomwork oss://<bucket>/workspaces/ws-<ID>/bocomwork/
ossutil cp -r -f bocomcode oss://<bucket>/workspaces/ws-<ID>/bocomcode/
```

**同步语义（重要）**：

脚本对每个 Workspace 先探测 `oss://<bucket>/workspaces/ws-<ID>/`，再决定是否上传：

| 远端状态 | 脚本行为 |
| --- | --- |
| **已有数据**（存在非目录对象） | **整个 Workspace 跳过**：不上传、不覆盖、不删除，日志记 `SKIP` |
| 为空 / 前缀不存在 | 正常上传，OSS 侧没有可被覆盖的数据 |
| 只剩目录占位对象（`Key` 以 `/` 结尾的 0 字节对象） | 视为空，正常上传 |
| 无法确认（`ossutil ls` 报权限、网络等异常） | 该 Workspace 判 `FAIL` 并终止，**绝不盲目上传** |

要点：

* **永远不会覆盖或删除 OSS 上已有的数据**，这是默认且唯一的行为（除非显式加 `--force`）。
* 需要重新覆盖某个 Workspace（例如上一次上传中途失败、数据不完整）时，用：
  ```bash
  bash hack/migrate-localpath-to-oss.sh -id <ID> --force
  ```
  `--force` 会跳过探测、直接覆盖上传该 Workspace 的三个子目录。
* 探测只看"是否有非目录对象"，因此**误判风险很低**；但请注意：若某个 Workspace 曾在 OSS 上被上传过一部分，之后会被整体跳过，需用 `--force` 补齐。
* 切勿给 `ossutil cp` 传 `-u/--update`：该参数会跳过"远端不旧于本地"的文件，与 `--force` 覆盖语义相反。

### 7.3 用法

> **可以直接在数据目录里执行**，不需要记生产路径：
>
> ```bash
> cd /opt/local-path-provisioner        # 生产实际的数据目录
> bash /path/to/migrate-localpath-to-oss.sh --all --yes
> ```
>
> 未指定 `--root` 时脚本按以下顺序自动定位：当前目录含 `pvc-*_<ns>_ws-<ID>-pvc` → 用当前目录；否则用 `/data/local-path-storage`；都没有则报错并提示用 `--root`。
> `--root` 同时支持相对路径（如 `--root .`、`--root ./nodes/node1`），脚本会自己转成绝对路径。

```bash
# 先确认 ossutil 已配置 AK（建议使用仅对该 Bucket 有读写权限的 RAM 子账号）
ossutil config

# 0) 列出本地有哪些 Workspace（不需要 ossutil，不产生任何写操作）
bash hack/migrate-localpath-to-oss.sh --list

# 1) 演练：只打印将要执行的命令，不删除、不上传、不写日志
bash hack/migrate-localpath-to-oss.sh -id 9660946,08,01 --dry-run

# 2) 迁移指定 Workspace（本地不存在的 ID 自动跳过，不做任何操作）
bash hack/migrate-localpath-to-oss.sh -id 9660946,08,01

# 3) 分批迁移：每次 5 个，按 ID 排序，配合 --skip-done 断点续跑
bash hack/migrate-localpath-to-oss.sh --all --limit 5 --skip-done --yes

# 4) 全量迁移（远端已有数据的 ws 会自动跳过）
bash hack/migrate-localpath-to-oss.sh --all --yes

# 5) 某个 ws 上次上传不完整，需要覆盖重传
bash hack/migrate-localpath-to-oss.sh -id 9660946 --force --yes

# 6) ID 写在文件里，批量迁移
bash hack/migrate-localpath-to-oss.sh -f ids.txt --yes
```

ID 文件（`ids.txt`）格式很宽松：

```text
# 以 # 开头是注释，整行忽略
9660946
08
01        # 行尾注释也可以
ws-0001-pvc

13, 24, 51   # 一行写多个，逗号或空格分隔
```

* 空行、多余空格、Tab 分隔、全角逗号、Windows 换行（CRLF）、UTF-8 BOM、重复 ID 都会被自动处理；
* `ws-` 前缀、`-pvc` 后缀可写可不写；
* 也可以直接把 `ls` 的结果贴进去：`pvc-06aacd22-..._bocomwork_ws-9660946-pvc`，
  甚至整条路径 `/data/local-path-storage/pvc-..._ws-08-pvc/` 都能识别出 ID；
* 支持多个文件：`-f a.txt -f b.txt`，也支持与 `-id` 混用（取并集）；
* `-f -` 表示从标准输入读取，便于管道：`kubectl ... | bash migrate-localpath-to-oss.sh -f -`。

### 7.4 参数速查

| 参数 | 说明 |
| --- | --- |
| `--all` | 迁移本地全部 Workspace |
| `-id LIST` | 只迁移指定 ID，逗号或空格分隔；**不存在的 ID 直接跳过、不做任何操作** |
| `-f, --file FILE` | 从文件读取 ID（每行一个，也支持逗号/空格分隔、`#` 注释、CRLF；`-` 表示 stdin）；可与 `-id` 混用 |
| `--limit N` | 最多迁移 N 个（按 ID 排序），用于分批灰度 |
| `--list` | 仅列出本地已有的 ID 及目录 |
| `--root DIR` | local-path 数据根目录；支持相对路径。省略时自动识别（当前目录 → `/data/local-path-storage`） |
| `--bucket` / `--prefix` | OSS 目标，默认 `oss://euvduatc-oss` + `workspaces` |
| `--dirs LIST` | 需要迁移的子目录，默认 `workspace,bocomwork,bocomcode` |
| `--force` | 忽略远端已有数据，强制覆盖上传（默认：远端有数据就跳过） |
| `--ossutil-arg ARG` | 透传 ossutil 参数（如 `--ossutil-arg=--jobs=10` 提升并发） |
| `--dry-run` | 演练模式 |
| `--skip-done` | 跳过日志中已成功的工作空间 |
| `--retries N` | ossutil 失败重试次数，默认 3 |
| `-y/--yes` | 跳过交互确认（cron 等非交互场景必须加） |

### 7.5 日志与断点续跑

日志默认写在**脚本所在目录**的 `logs/oss-migrate.log`；如果该目录不可写（例如脚本放在 `/usr/local/bin`），会自动回退到 `~/.oss-migrate/oss-migrate.log` 并给出提示。用 `--log` 可显式指定，指定后若不可写会直接报错（不会悄悄换位置）。

成功的工作空间会写入 TSV 日志（默认 `hack/logs/oss-migrate.log`），列结构：

```
time  status  workspace  local_dir  remote_prefix  dirs  size  files  note
```

* 只有 `status=OK` 才代表该 Workspace 全部子目录上传成功；`FAIL` / `SKIP` 不会被 `--skip-done` 排除，方便重跑补齐。
* 因为远端已有数据就跳过，重跑整批是安全的幂等操作；`--skip-done` 只是省掉一次远端探测请求。
* 中途中断后，用 `--all --skip-done --yes` 即可从断点继续。
* 脚本结尾会汇总成功/跳过/失败数量，并针对失败项直接给出重试命令。

### 7.6 迁移前检查清单

1. **停掉目标 Workspace**：`kubectl get deploy -n bocomwork ws-<ID>-deploy`，确认副本已缩为 0，避免迁移过程中容器继续写入（覆盖上传期间本地文件仍在变化会导致远端内容不一致）。
2. **确认 Bucket 与 Region 匹配**：`oss://<bucket>` 必须与 `deploy/oss/storageclass.yaml` 中配置的 `bucket`、`url` 一致，否则新 PV 挂载后看不到数据。
3. **确认 ossutil 的 AK 权限**：至少需要该前缀的 `GetObject` / `PutObject` / `ListObjects` / `DeleteObject`。
4. **先只迁 1 个 Workspace 验证**：用 `-id <ID>` 单跑一个，启动 Workspace 确认数据完整后，再逐步放量或 `--all`。
5. **保留本地数据**：确认 OSS 侧数据可用前，不要删除 `/data/local-path-storage/` 下的原目录。

### 7.7 迁移后切换

把默认 StorageClass 指向 OSS 后，新建 Workspace 会自动使用 OSS PV；已迁移的 Workspace 需重建 PVC 才能换到 OSS 底座：

```bash
# 1) 删除旧 local-path PVC（reclaimPolicy=Delete，本地 PV 与物理目录会一并清理，请确认数据已迁移）
kubectl delete pvc ws-<ID>-pvc -n bocomwork

# 2) 重新拉起 Workspace，Controller 会按 StorageClass 自动创建 OSS PV + PVC
kubectl patch workspace <workspace-name> -n bocomwork --type=merge -p '{"spec":{"stopped":false}}'
```

> 注意：删除 PVC 会触发 `local-path` 清理本地目录，务必在确认 OSS 数据完整之后再执行第 1 步。
