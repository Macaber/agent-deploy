// egress-sync watches the unified ConfigMap and publishes observed Envoy state.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/example/workspace-operator/internal/egress"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	dir := flag.String("directory", "/var/lib/envoy", "Shared dynamic configuration directory")
	initOnly := flag.Bool("init", false, "Write deny-all bootstrap before Envoy starts")
	readyOnly := flag.Bool("ready", false, "Check local replica readiness")
	flag.Parse()
	if *readyOnly {
		c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
		resp, err := c.Get("http://127.0.0.1:9902/ready")
		if err != nil {
			log.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	if err := run(*dir, *initOnly); err != nil {
		log.Fatal(err)
	}
}

func run(dir string, initOnly bool) error {
	path := filepath.Join(dir, "routes.json")
	if initOnly {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		nodeID := os.Getenv("POD_NAME")
		if nodeID == "" {
			nodeID, _ = os.Hostname()
		}
		if nodeID == "" {
			return fmt.Errorf("cannot determine Envoy node ID")
		}
		b, err := egress.Bootstrap(nodeID, path)
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, "bootstrap.json"), b, 0644); err != nil {
			return err
		}
		return egress.ReplaceRoutes(path, egress.Config{})
	}
	podName, podUID := os.Getenv("POD_NAME"), os.Getenv("POD_UID")
	if podName == "" || podUID == "" {
		return fmt.Errorf("POD_NAME and POD_UID are required")
	}
	ctx := ctrl.SetupSignalHandler()
	rest, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	k, err := kubernetes.NewForConfig(rest)
	if err != nil {
		return err
	}
	ns := egress.Namespace()
	changes := make(chan struct{}, 1)
	signal := func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
	cmClient := k.CoreV1().ConfigMaps(ns)
	informer := cache.NewSharedIndexInformer(&cache.ListWatch{
		ListFunc: func(o metav1.ListOptions) (runtime.Object, error) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", egress.ConfigMapName).String()
			return cmClient.List(ctx, o)
		},
		WatchFunc: func(o metav1.ListOptions) (watch.Interface, error) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", egress.ConfigMapName).String()
			return cmClient.Watch(ctx, o)
		},
	}, &corev1.ConfigMap{}, 0, cache.Indexers{})
	if _, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: func(any) { signal() }, UpdateFunc: func(any, any) { signal() }, DeleteFunc: func(any) { signal() }}); err != nil {
		return err
	}
	go informer.Run(ctx.Done())
	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "configuration not loaded", 503)
			return
		}
		w.WriteHeader(200)
	})
	server := &http.Server{Addr: "127.0.0.1:9902", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Readiness server: %v", err)
		}
	}()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("ConfigMap watch did not synchronize")
	}
	httpClient := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	wanted, written := "", ""
	var lastError string
	apply := func() {
		object, exists, err := informer.GetStore().GetByKey(ns + "/" + egress.ConfigMapName)
		if err != nil || !exists {
			wanted = ""
			lastError = "Unified policy is missing"
			ready.Store(false)
			return
		}
		c, err := egress.Decode([]byte(object.(*corev1.ConfigMap).Data[egress.ConfigKey]))
		if err != nil {
			wanted = ""
			lastError = err.Error()
			ready.Store(false)
			return
		}
		wanted = c.Revision()
		if wanted != written {
			ready.Store(false)
			if err = egress.ReplaceRoutes(path, c); err != nil {
				lastError = err.Error()
				return
			}
			written = wanted
		}
		lastError = ""
	}
	observe := func() {
		actual := ""
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:9901/config_dump?resource=dynamic_route_configs", nil)
		if err != nil {
			lastError = err.Error()
		} else {
			// Full config dump is needed to identify the RoutesConfigDump type.
			req.URL.RawQuery = ""
			resp, e := httpClient.Do(req)
			if e != nil {
				lastError = e.Error()
			} else {
				b, e := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
				_ = resp.Body.Close()
				if e == nil && resp.StatusCode == 200 {
					actual, e = egress.LoadedRevision(b)
				} else if e == nil {
					e = fmt.Errorf("Envoy admin returned %d", resp.StatusCode)
				}
				if e != nil {
					lastError = e.Error()
				}
			}
		}
		state := "syncing"
		if lastError != "" {
			state = "failed"
		} else if wanted != "" && actual == wanted {
			state = "loaded"
		}
		ready.Store(state == "loaded")
		status := map[string]string{"desiredRevision": wanted, "loadedRevision": actual, "state": state, "error": lastError, "observedAt": time.Now().UTC().Format(time.RFC3339), "podUID": podUID}
		b, _ := json.Marshal(status)
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": podUID, "annotations": map[string]string{"workspace.bocom.io/egress-status": string(b)}}})
		patchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := k.CoreV1().Pods(ns).Patch(patchCtx, podName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			log.Printf("Status report failed: %v", err)
		}
	}
	apply()
	observe()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changes:
			apply()
			observe()
		case <-ticker.C:
			apply()
			observe()
		}
	}
}
