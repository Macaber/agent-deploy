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

