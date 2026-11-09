package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func Namespace() string {
	if s := os.Getenv("EGRESS_CONFIG_NAMESPACE"); s != "" {
		return s
	}
	return "agent-deploy-system"
}

// Decode rejects unknown fields and trailing documents, including invalid manual edits.
func Decode(data []byte) (Config, error) {
	var c Config
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("expected one policy document")
	}
	return c, c.Validate()
}

func Read(ctx context.Context, k client.Client, namespace string) (Config, string, error) {
	cm := &corev1.ConfigMap{}
	if err := k.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ConfigMapName}, cm); err != nil {
		return Config{}, "", err
	}
	c, err := Decode([]byte(cm.Data[ConfigKey]))
	return c, cm.ResourceVersion, err
}

// Save uses optimistic concurrency. A stale editor must reload instead of silently
// overwriting another administrator's changes. Empty version means create only.
func Save(ctx context.Context, k client.Client, namespace, version string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: namespace, ResourceVersion: version}, Data: map[string]string{ConfigKey: string(b)}}
	if version == "" {
		return k.Create(ctx, cm)
	}
	// Preserve unrelated metadata and data keys while retaining the client's version.
	existing := &corev1.ConfigMap{}
	if err := k.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ConfigMapName}, existing); err != nil {
		return err
	}
	existing.ResourceVersion = version
	if existing.Data == nil {
		existing.Data = map[string]string{}
	}
	existing.Data[ConfigKey] = string(b)
	return k.Update(ctx, existing)
}
