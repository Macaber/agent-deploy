package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

func main() {
	log.Println("Initializing local API Gateway / Mock Server...")

	// Fetch standard kubeconfig context
	config, err := ctrl.GetConfig()
	if err != nil {
		log.Fatalf("Failed to load kubeconfig: %v", err)
	}

	// Register our custom Workspace schema in client scheme
	scheme := runtime.NewScheme()
	if err := aiv1alpha1.AddToScheme(scheme); err != nil {
		log.Fatalf("Failed to add scheme: %v", err)
	}

	// Create K8s client
	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("Failed to create K8s client: %v", err)
	}

	http.HandleFunc("/", serveHome)
	http.HandleFunc("/api/workspaces", createWorkspaceHandler(k8sClient))
	http.HandleFunc("/api/workspaces/stop", stopWorkspaceHandler(k8sClient))
	http.HandleFunc("/api/workspaces/wakeup", wakeupWorkspaceHandler(k8sClient))

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	log.Printf("API Server is running on http://localhost:%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server exited with error: %v", err)
	}
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tmpl, err := template.New("index").Parse(htmlTemplate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpl.Execute(w, nil)
}

func createWorkspaceHandler(c client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			userID := r.URL.Query().Get("userId")
			if userID == "" {
				namespace := r.URL.Query().Get("namespace")
				if namespace == "" {
					namespace = "default"
				}
				ctx := r.Context()
				wsList := &aiv1alpha1.WorkspaceList{}
				if err := c.List(ctx, wsList, client.InNamespace(namespace)); err != nil {
					log.Printf("Failed to list Workspaces: %v", err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}

				type WorkspaceItem struct {
					UserID string `json:"userId"`
					Name   string `json:"name"`
					Phase  string `json:"phase"`
					URL    string `json:"url"`
					Image  string `json:"image"`
					CPU    string `json:"cpu"`
					Memory string `json:"memory"`
				}

				items := []WorkspaceItem{}
				for _, ws := range wsList.Items {
					url := getWorkspaceURL(ws.Spec.Owner, ws.Name, ws.Status.Endpoint)
					items = append(items, WorkspaceItem{
						UserID: ws.Spec.Owner,
						Name:   ws.Name,
						Phase:  string(ws.Status.Phase),
						URL:    url,
						Image:  ws.Spec.Runtime.Image,
						CPU:    ws.Spec.Runtime.CPU,
						Memory: ws.Spec.Runtime.Memory,
					})
				}

				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(items)
				return
			}

			ctx := r.Context()
			wsName := fmt.Sprintf("ws-%s", userID)
			namespace := r.URL.Query().Get("namespace")
			if namespace == "" {
				namespace = "default"
			}

			ws := &aiv1alpha1.Workspace{}
			err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, ws)
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				if apierrors.IsNotFound(err) {
					json.NewEncoder(w).Encode(map[string]any{
						"exists": false,
						"phase":  "",
						"url":    "",
					})
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			url := getWorkspaceURL(userID, wsName, ws.Status.Endpoint)

			json.NewEncoder(w).Encode(map[string]any{
				"exists": true,
				"phase":  ws.Status.Phase,
				"url":    url,
			})
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			UserID          string                   `json:"userId"`
			Namespace       string                   `json:"namespace,omitempty"`
			Image           string                   `json:"image,omitempty"`
			Port            int32                    `json:"port,omitempty"`
			CPU             string                   `json:"cpu,omitempty"`
			Memory          string                   `json:"memory,omitempty"`
			StorageSize     string                   `json:"storageSize,omitempty"`
			StorageClass    string                   `json:"storageClass,omitempty"`
			IdleTimeout     string                   `json:"idleTimeout,omitempty"`
			ExposeSSH       *bool                    `json:"exposeSSH,omitempty"`
			Env             []aiv1alpha1.EnvVar      `json:"env,omitempty"`
			Command         []string                 `json:"command,omitempty"`
			Args            []string                 `json:"args,omitempty"`
			VolumeMounts    []aiv1alpha1.VolumeMount `json:"volumeMounts,omitempty"`
			PostStartScript string                   `json:"postStartScript,omitempty"`
			HealthPath      string                   `json:"healthPath,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		wsName := fmt.Sprintf("ws-%s", req.UserID)
		namespace := req.Namespace
		if namespace == "" {
			namespace = "default"
		}

		ws := &aiv1alpha1.Workspace{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, ws)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Set defaults
				image := req.Image
				if image == "" {
					image = "smanx/opencode:latest"
				}
				port := req.Port
				if port == 0 {
					port = 4096
				}
				storageSize := req.StorageSize
				if storageSize == "" {
					storageSize = "1Gi"
				}
				idleTimeout := req.IdleTimeout
				if idleTimeout == "" {
					idleTimeout = "5m"
				}
				exposeSSH := false
				if req.ExposeSSH != nil {
					exposeSSH = *req.ExposeSSH
				}

				// Create a new Workspace
				ws = &aiv1alpha1.Workspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:      wsName,
						Namespace: namespace,
					},
					Spec: aiv1alpha1.WorkspaceSpec{
						Owner:       req.UserID,
						IdleTimeout: idleTimeout,
						ExposeSSH:   exposeSSH,
						Runtime: aiv1alpha1.RuntimeSpec{
							Image:           image,
							Port:            port,
							CPU:             req.CPU,
							Memory:          req.Memory,
							Env:             req.Env,
							Command:         req.Command,
							Args:            req.Args,
							VolumeMounts:    req.VolumeMounts,
							PostStartScript: req.PostStartScript,
							HealthPath:      req.HealthPath,
						},
						Storage: aiv1alpha1.StorageSpec{
							Size:         storageSize,
							StorageClass: req.StorageClass,
						},
					},
				}
				if err := c.Create(ctx, ws); err != nil {
					log.Printf("Failed to create Workspace: %v", err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				log.Printf("Successfully created Workspace resource: %s", wsName)
			} else {
				log.Printf("Failed to fetch Workspace: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			// Workspace exists! Let's check if it was stopped. If so, resume it!
			needsUpdate := false
			if ws.Spec.Stopped {
				ws.Spec.Stopped = false
				needsUpdate = true
			}

			// Update specifications if explicitly passed in the request payload
			if req.Image != "" && ws.Spec.Runtime.Image != req.Image {
				ws.Spec.Runtime.Image = req.Image
				needsUpdate = true
			}
			if req.Port != 0 && ws.Spec.Runtime.Port != req.Port {
				ws.Spec.Runtime.Port = req.Port
				needsUpdate = true
			}
			if req.CPU != "" && ws.Spec.Runtime.CPU != req.CPU {
				ws.Spec.Runtime.CPU = req.CPU
				needsUpdate = true
			}
			if req.Memory != "" && ws.Spec.Runtime.Memory != req.Memory {
				ws.Spec.Runtime.Memory = req.Memory
				needsUpdate = true
			}
			if len(req.Env) > 0 {
				ws.Spec.Runtime.Env = req.Env
				needsUpdate = true
			}
			if len(req.Command) > 0 {
				ws.Spec.Runtime.Command = req.Command
				needsUpdate = true
			}
			if len(req.Args) > 0 {
				ws.Spec.Runtime.Args = req.Args
				needsUpdate = true
			}
			if len(req.VolumeMounts) > 0 {
				ws.Spec.Runtime.VolumeMounts = req.VolumeMounts
				needsUpdate = true
			}
			if req.PostStartScript != "" {
				ws.Spec.Runtime.PostStartScript = req.PostStartScript
				needsUpdate = true
			}
			if req.HealthPath != "" && ws.Spec.Runtime.HealthPath != req.HealthPath {
				ws.Spec.Runtime.HealthPath = req.HealthPath
				needsUpdate = true
			}
			if req.StorageSize != "" && ws.Spec.Storage.Size != req.StorageSize {
				ws.Spec.Storage.Size = req.StorageSize
				needsUpdate = true
			}
			if req.StorageClass != "" && ws.Spec.Storage.StorageClass != req.StorageClass {
				ws.Spec.Storage.StorageClass = req.StorageClass
				needsUpdate = true
			}
			if req.IdleTimeout != "" && ws.Spec.IdleTimeout != req.IdleTimeout {
				ws.Spec.IdleTimeout = req.IdleTimeout
				needsUpdate = true
			}
			if req.ExposeSSH != nil && ws.Spec.ExposeSSH != *req.ExposeSSH {
				ws.Spec.ExposeSSH = *req.ExposeSSH
				needsUpdate = true
			}

			if needsUpdate {
				if err := c.Update(ctx, ws); err != nil {
					log.Printf("Failed to update Workspace: %v", err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				log.Printf("Updated existing Workspace spec: %s", wsName)
			}

			// Always update LastActiveTime to now when the user starts/resumes the workspace,
			// which wakes it up from Sleeping state.
			ws.Status.LastActiveTime = &metav1.Time{Time: time.Now()}
			if err := c.Status().Update(ctx, ws); err != nil {
				log.Printf("Failed to update Workspace lastActiveTime: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			log.Printf("Updated Workspace lastActiveTime: %s", wsName)
		}

		// Poll until Workspace status.phase is Running
		timeout := time.After(90 * time.Second)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-timeout:
				http.Error(w, "Workspace creation timed out", http.StatusGatewayTimeout)
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				currentWs := &aiv1alpha1.Workspace{}
				if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, currentWs); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}

				if currentWs.Status.Phase == aiv1alpha1.WorkspaceRunning {
					url := getWorkspaceURL(req.UserID, wsName, currentWs.Status.Endpoint)

					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]string{
						"url":   url,
						"phase": string(currentWs.Status.Phase),
					})
					return
				}
			}
		}
	}
}

func stopWorkspaceHandler(c client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			UserID    string `json:"userId"`
			Namespace string `json:"namespace,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		wsName := fmt.Sprintf("ws-%s", req.UserID)
		namespace := req.Namespace
		if namespace == "" {
			namespace = "default"
		}

		ws := &aiv1alpha1.Workspace{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, ws)
		if err != nil {
			log.Printf("Failed to fetch Workspace %s: %v", wsName, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Update stopped to true
		ws.Spec.Stopped = true
		if err := c.Update(ctx, ws); err != nil {
			log.Printf("Failed to stop Workspace %s: %v", wsName, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		log.Printf("Successfully stopped Workspace %s (replicas set to 0)", wsName)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "stopped",
		})
	}
}

func wakeupWorkspaceHandler(c client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			UserID    string `json:"userId"`
			Namespace string `json:"namespace,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		wsName := fmt.Sprintf("ws-%s", req.UserID)
		namespace := req.Namespace
		if namespace == "" {
			namespace = "default"
		}

		ws := &aiv1alpha1.Workspace{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, ws)
		if err != nil {
			if apierrors.IsNotFound(err) {
				http.Error(w, "Workspace not found", http.StatusNotFound)
				return
			}
			log.Printf("Failed to fetch Workspace: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Update stopped to false if it was manually stopped
		needsUpdateSpec := false
		if ws.Spec.Stopped {
			ws.Spec.Stopped = false
			needsUpdateSpec = true
		}
		if needsUpdateSpec {
			if err := c.Update(ctx, ws); err != nil {
				log.Printf("Failed to resume Workspace spec: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			log.Printf("Resumed stopped Workspace spec in wakeup: %s", wsName)
		}

		// Always update LastActiveTime to now when waking it up from Sleeping state
		ws.Status.LastActiveTime = &metav1.Time{Time: time.Now()}
		if err := c.Status().Update(ctx, ws); err != nil {
			log.Printf("Failed to update Workspace lastActiveTime in wakeup: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("Successfully waked up Workspace lastActiveTime: %s", wsName)

		// Poll until Workspace status.phase is Running
		timeout := time.After(90 * time.Second)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-timeout:
				http.Error(w, "Workspace wakeup timed out", http.StatusGatewayTimeout)
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				currentWs := &aiv1alpha1.Workspace{}
				if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, currentWs); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}

				if currentWs.Status.Phase == aiv1alpha1.WorkspaceRunning {
					url := getWorkspaceURL(req.UserID, wsName, currentWs.Status.Endpoint)

					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]string{
						"url":   url,
						"phase": string(currentWs.Status.Phase),
					})
					return
				}
			}
		}
	}
}

func getWorkspaceURL(userID string, wsName string, endpoint string) string {
	baseURL := os.Getenv("WORKSPACE_BASE_URL")
	if baseURL != "" {
		baseURL = strings.TrimSuffix(baseURL, "/")
		return fmt.Sprintf("%s/%s/", baseURL, userID)
	}

	if endpoint == "" {
		domain := os.Getenv("WORKSPACE_DOMAIN")
		if domain == "" {
			domain = "localhost"
		}
		endpoint = fmt.Sprintf("%s.%s", wsName, domain)
	}
	return fmt.Sprintf("http://%s", endpoint)
}

//go:embed portal.html
var htmlTemplate string
