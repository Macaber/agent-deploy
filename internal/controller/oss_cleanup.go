package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

// ossCSIDriver 平台使用的阿里云 OSS CSI 驱动名
const ossCSIDriver = "ossplugin.csi.alibabacloud.com"

// cleanupPV 删除 workspace 的同名 PV。
// OSS 卷的 reclaimPolicy 为 Retain 且 CSI DeleteVolume 为空操作，删除 PV 对象前
// 需要显式清空 OSS 上该 workspace 的数据目录，与 NAS/Local 方案删除 PVC 时
// 由 provisioner 清空存储数据的语义保持一致。
// Local/NFS 卷由各自 provisioner 在 PVC Delete 回收时清理数据，这里无需处理。
func (r *WorkspaceReconciler) cleanupPV(ctx context.Context, ws *aiv1alpha1.Workspace) error {
	log := logf.FromContext(ctx)
	pvName := ws.Name + "-pv"
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == ossCSIDriver {
		if err := r.cleanupOSSData(ctx, ws, pv); err != nil {
			// 数据清理失败不阻塞 workspace 删除（与 PVC 已删而 provisioner 清理失败的行为一致），
			// 记录错误日志供人工介入
			log.Error(err, "Failed to delete OSS data, PV will still be removed", "pvName", pvName)
		}
	}

	if err := r.Delete(ctx, pv); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// workspaceOSSPrefix 由 workspace 名称推导其在 OSS 上的数据目录前缀。
// 与创建时的 subPath 约定一致：/workspaces/<name> → OSS key 前缀 workspaces/<name>。
// 名称不合法时返回空串，调用方必须拒绝执行删除。
func workspaceOSSPrefix(wsName string) string {
	name := strings.TrimSpace(wsName)
	if name == "" || strings.Contains(name, "/") {
		return ""
	}
	return "workspaces/" + name
}

// filterWorkspaceKeys 只保留属于该 workspace 数据目录的对象：
//   - key 恰好等于 prefix（目录占位对象）
//   - key 以 prefix+"/" 开头（目录下的文件）
//
// 防止误删其他 workspace 的数据，例如删除 ws-aikc 时不会误伤 ws-aikc-dev
// 或 ws-aikc-tmp 等同名前缀的对象。
func filterWorkspaceKeys(keys []string, prefix string) []string {
	filtered := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == prefix || strings.HasPrefix(key, prefix+"/") {
			filtered = append(filtered, key)
		}
	}
	return filtered
}

// cleanupOSSData 清空 workspace 在 OSS 上对应的数据目录。
// 安全保证：删除前缀完全由 workspace 名称推导（workspaces/<name>），
// 不采用 PV 上的 path 属性，且按目录边界过滤，绝不删除其他路径的内容。
func (r *WorkspaceReconciler) cleanupOSSData(ctx context.Context, ws *aiv1alpha1.Workspace, pv *corev1.PersistentVolume) error {
	log := logf.FromContext(ctx)

	prefix := workspaceOSSPrefix(ws.Name)
	if prefix == "" {
		return fmt.Errorf("invalid workspace name %q, refuse to delete OSS data", ws.Name)
	}

	attrs := pv.Spec.CSI.VolumeAttributes
	bucket := attrs["bucket"]
	url := attrs["url"]
	if bucket == "" || url == "" {
		return fmt.Errorf("PV %q missing bucket/url volume attributes, skip OSS data deletion", pv.Name)
	}
	if pathAttr := strings.Trim(strings.TrimSpace(attrs["path"]), "/"); pathAttr != "" && pathAttr != prefix {
		// path 属性与命名约定不一致时，仍只删除由 workspace 名称推导的前缀（安全优先）
		log.Info("PV path attribute differs from naming convention, deleting only the workspace-derived prefix",
			"pvPath", pathAttr, "deletePrefix", prefix)
	}

	// 凭证引用：优先 controllerPublishSecretRef（新 PV 均有），回退 nodePublishSecretRef，最后用默认值
	secretName, secretNamespace := "oss-secret", "default"
	if ref := pv.Spec.CSI.ControllerPublishSecretRef; ref != nil && ref.Name != "" {
		secretName, secretNamespace = ref.Name, ref.Namespace
	} else if ref := pv.Spec.CSI.NodePublishSecretRef; ref != nil && ref.Name != "" {
		secretName, secretNamespace = ref.Name, ref.Namespace
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: secretNamespace, Name: secretName}, secret); err != nil {
		return fmt.Errorf("get secret %s/%s: %w", secretNamespace, secretName, err)
	}
	akID := strings.TrimSpace(string(secret.Data["akId"]))
	akSecret := strings.TrimSpace(string(secret.Data["akSecret"]))
	if akID == "" || akSecret == "" {
		return fmt.Errorf("secret %s/%s missing akId/akSecret keys", secretNamespace, secretName)
	}

	// 专有云/内网 OSS 走 http + path-style；旧 PV 的 url 可能不带协议，默认补 http://
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + url
	}

	client, err := oss.New(url, akID, akSecret, oss.ForcePathStyle(true))
	if err != nil {
		return fmt.Errorf("create OSS client: %w", err)
	}
	b, err := client.Bucket(bucket)
	if err != nil {
		return fmt.Errorf("get bucket %q: %w", bucket, err)
	}

	marker := ""
	deleted := 0
	skipped := 0
	for {
		lsRes, err := b.ListObjects(oss.Marker(marker), oss.Prefix(prefix), oss.MaxKeys(1000))
		if err != nil {
			return fmt.Errorf("list objects under %q: %w", prefix, err)
		}
		if len(lsRes.Objects) > 0 {
			allKeys := make([]string, 0, len(lsRes.Objects))
			for _, obj := range lsRes.Objects {
				allKeys = append(allKeys, obj.Key)
			}
			keys := filterWorkspaceKeys(allKeys, prefix)
			skipped += len(allKeys) - len(keys)
			if len(keys) > 0 {
				if _, err := b.DeleteObjects(keys); err != nil {
					return fmt.Errorf("delete objects under %q: %w", prefix, err)
				}
				deleted += len(keys)
			}
		}
		if !lsRes.IsTruncated {
			break
		}
		marker = lsRes.NextMarker
	}
	log.Info("Deleted OSS workspace data", "bucket", bucket, "prefix", prefix, "objects", deleted, "skipped", skipped)
	return nil
}
