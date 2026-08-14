package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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
	http.HandleFunc("/api/login", loginHandler)
	http.HandleFunc("/api/logout", logoutHandler)
	http.HandleFunc("/api/auth/check", authCheckHandler)
	http.HandleFunc("/api/workspaces", workspaceRouter(k8sClient))
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

// workspaceRequest is the shared request body for workspace create/update operations.
type workspaceRequest struct {
	UserID                string                            `json:"userId"`
	Namespace             string                            `json:"namespace,omitempty"`
	Image                 string                            `json:"image,omitempty"`
	Port                  int32                             `json:"port,omitempty"`
	CPU                   string                            `json:"cpu,omitempty"`
	Memory                string                            `json:"memory,omitempty"`
	StorageSize           string                            `json:"storageSize,omitempty"`
	StorageClass          string                            `json:"storageClass,omitempty"`
	IdleTimeout           string                            `json:"idleTimeout,omitempty"`
	ExposeSSH             *bool                             `json:"exposeSSH,omitempty"`
	Env                   []aiv1alpha1.EnvVar               `json:"env,omitempty"`
	Command               []string                          `json:"command,omitempty"`
	Cmd                   []string                          `json:"cmd,omitempty"`
	Args                  []string                          `json:"args,omitempty"`
	VolumeMounts          []aiv1alpha1.VolumeMount          `json:"volumeMounts,omitempty"`
	PostStartScript       string                            `json:"postStartScript,omitempty"`
	HealthPath            string                            `json:"healthPath,omitempty"`
	SharedVolumeMounts    []aiv1alpha1.SharedVolumeMount    `json:"sharedVolumeMounts,omitempty"`
	ConfigMapVolumeMounts []aiv1alpha1.ConfigMapVolumeMount `json:"configMapVolumeMounts,omitempty"`
	InitContainers        []aiv1alpha1.InitContainerSpec    `json:"initContainers,omitempty"`
}

// workspaceItem is the JSON response type for listing workspaces.
type workspaceItem struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	Phase  string `json:"phase"`
	URL    string `json:"url"`
	Image  string `json:"image"`
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// workspaceRouter dispatches /api/workspaces requests to the appropriate handler by HTTP method.
func workspaceRouter(c client.Client) http.HandlerFunc {
	listHandler := listWorkspacesHandler(c)
	getHandler := getWorkspaceHandler(c)
	createHandler := createOrUpdateWorkspaceHandler(c)

	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("userId") == "" {
				listHandler.ServeHTTP(w, r)
			} else {
				getHandler.ServeHTTP(w, r)
			}
		case http.MethodPost:
			createHandler.ServeHTTP(w, r)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// listWorkspacesHandler handles GET /api/workspaces (without userId) — lists all workspaces in a namespace.
func listWorkspacesHandler(c client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		items := []workspaceItem{}
		for _, ws := range wsList.Items {
			url := getWorkspaceURL(ws.Spec.Owner, ws.Name, ws.Status.Endpoint)
			items = append(items, workspaceItem{
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
	}
}

// getWorkspaceHandler handles GET /api/workspaces?userId=xxx — queries a single workspace's status.
func getWorkspaceHandler(c client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("userId")
		ctx := r.Context()
		wsName := sanitizeK8sName(fmt.Sprintf("ws-%s", userID))
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
			"spec":   ws.Spec,
		})
	}
}

// getEffectiveUserID returns the effective user ID for workspace creation and lookup.
// If namespace is "bocomwork" and an env variable named "USER_CODE" (case-insensitive) exists with a non-empty value,
// that value replaces the incoming userID.
func getEffectiveUserID(userID string, namespace string, envs []aiv1alpha1.EnvVar) string {
	if namespace == "bocomwork" {
		for _, env := range envs {
			if strings.EqualFold(env.Name, "USER_CODE") && strings.TrimSpace(env.Value) != "" {
				return strings.TrimSpace(env.Value)
			}
		}
	}
	return userID
}

// createOrUpdateWorkspaceHandler handles POST /api/workspaces — creates a new workspace or updates an existing one.
func createOrUpdateWorkspaceHandler(c client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req workspaceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Image == "" {
			http.Error(w, "Missing required parameter: image", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		namespace := req.Namespace
		if namespace == "" {
			namespace = "default"
		}
		effectiveUserID := getEffectiveUserID(req.UserID, namespace, req.Env)
		wsName := sanitizeK8sName(fmt.Sprintf("ws-%s", effectiveUserID))

		ws := &aiv1alpha1.Workspace{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, ws)
		if err != nil {
			if apierrors.IsNotFound(err) {
				if err := createNewWorkspace(ctx, c, &req, wsName, namespace, effectiveUserID); err != nil {
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
			if err := updateExistingWorkspace(ctx, c, ws, &req, wsName, namespace); err != nil {
				log.Printf("Failed to update Workspace: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// Poll until Workspace status.phase is Running or Failed
		pollWorkspaceRunning(ctx, c, w, effectiveUserID, wsName, namespace, "Workspace creation timed out")
	}
}

// createNewWorkspace creates a brand new Workspace CR with default values applied.
func createNewWorkspace(ctx context.Context, c client.Client, req *workspaceRequest, wsName, namespace, ownerID string) error {
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

	cmdList := req.Command
	if len(cmdList) == 0 && len(req.Cmd) > 0 {
		cmdList = req.Cmd
	}

	ws := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wsName,
			Namespace: namespace,
		},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner:       ownerID,
			IdleTimeout: idleTimeout,
			ExposeSSH:   exposeSSH,
			Runtime: aiv1alpha1.RuntimeSpec{
				Image:           req.Image,
				Port:            port,
				CPU:             req.CPU,
				Memory:          req.Memory,
				Env:             req.Env,
				Command:         cmdList,
				Args:            req.Args,
				VolumeMounts:    req.VolumeMounts,
				PostStartScript: req.PostStartScript,
				HealthPath:      req.HealthPath,
				InitContainers:  req.InitContainers,
			},
			Storage: aiv1alpha1.StorageSpec{
				Size:         storageSize,
				StorageClass: req.StorageClass,
			},
			SharedVolumeMounts:    req.SharedVolumeMounts,
			ConfigMapVolumeMounts: req.ConfigMapVolumeMounts,
		},
	}
	return c.Create(ctx, ws)
}

// isSliceDifferent returns true if two slices are different, treating both empty and nil slices as equal length 0.
func isSliceDifferent[T any](current []T, req []T) bool {
	if len(current) == 0 && len(req) == 0 {
		return false
	}
	return !reflect.DeepEqual(current, req)
}

// updateExistingWorkspace applies spec changes and resets LastActiveTime to resume a workspace.
func updateExistingWorkspace(ctx context.Context, c client.Client, ws *aiv1alpha1.Workspace, req *workspaceRequest, wsName, namespace string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		currentWs := &aiv1alpha1.Workspace{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, currentWs); err != nil {
			return err
		}

		needsUpdate := false
		if currentWs.Spec.Stopped {
			currentWs.Spec.Stopped = false
			needsUpdate = true
		}

		// Update specifications if explicitly passed in the request payload
		if req.Image != "" && currentWs.Spec.Runtime.Image != req.Image {
			currentWs.Spec.Runtime.Image = req.Image
			needsUpdate = true
		}
		if req.Port != 0 && currentWs.Spec.Runtime.Port != req.Port {
			currentWs.Spec.Runtime.Port = req.Port
			needsUpdate = true
		}
		if req.CPU != "" && currentWs.Spec.Runtime.CPU != req.CPU {
			currentWs.Spec.Runtime.CPU = req.CPU
			needsUpdate = true
		}
		if req.Memory != "" && currentWs.Spec.Runtime.Memory != req.Memory {
			currentWs.Spec.Runtime.Memory = req.Memory
			needsUpdate = true
		}
		if req.Env != nil && isSliceDifferent(currentWs.Spec.Runtime.Env, req.Env) {
			currentWs.Spec.Runtime.Env = req.Env
			needsUpdate = true
		}
		cmdList := req.Command
		if len(cmdList) == 0 && len(req.Cmd) > 0 {
			cmdList = req.Cmd
		}
		if cmdList != nil && isSliceDifferent(currentWs.Spec.Runtime.Command, cmdList) {
			currentWs.Spec.Runtime.Command = cmdList
			needsUpdate = true
		}
		if req.Args != nil && isSliceDifferent(currentWs.Spec.Runtime.Args, req.Args) {
			currentWs.Spec.Runtime.Args = req.Args
			needsUpdate = true
		}
		if req.VolumeMounts != nil && isSliceDifferent(currentWs.Spec.Runtime.VolumeMounts, req.VolumeMounts) {
			currentWs.Spec.Runtime.VolumeMounts = req.VolumeMounts
			needsUpdate = true
		}
		if req.PostStartScript != "" && currentWs.Spec.Runtime.PostStartScript != req.PostStartScript {
			currentWs.Spec.Runtime.PostStartScript = req.PostStartScript
			needsUpdate = true
		}
		if req.HealthPath != "" && currentWs.Spec.Runtime.HealthPath != req.HealthPath {
			currentWs.Spec.Runtime.HealthPath = req.HealthPath
			needsUpdate = true
		}
		if req.StorageSize != "" && currentWs.Spec.Storage.Size != req.StorageSize {
			currentWs.Spec.Storage.Size = req.StorageSize
			needsUpdate = true
		}
		if req.StorageClass != "" && currentWs.Spec.Storage.StorageClass != req.StorageClass {
			currentWs.Spec.Storage.StorageClass = req.StorageClass
			needsUpdate = true
		}
		if req.IdleTimeout != "" && currentWs.Spec.IdleTimeout != req.IdleTimeout {
			currentWs.Spec.IdleTimeout = req.IdleTimeout
			needsUpdate = true
		}
		if req.ExposeSSH != nil && currentWs.Spec.ExposeSSH != *req.ExposeSSH {
			currentWs.Spec.ExposeSSH = *req.ExposeSSH
			needsUpdate = true
		}
		if req.SharedVolumeMounts != nil && isSliceDifferent(currentWs.Spec.SharedVolumeMounts, req.SharedVolumeMounts) {
			currentWs.Spec.SharedVolumeMounts = req.SharedVolumeMounts
			needsUpdate = true
		}
		if req.ConfigMapVolumeMounts != nil && isSliceDifferent(currentWs.Spec.ConfigMapVolumeMounts, req.ConfigMapVolumeMounts) {
			currentWs.Spec.ConfigMapVolumeMounts = req.ConfigMapVolumeMounts
			needsUpdate = true
		}
		if req.InitContainers != nil && isSliceDifferent(currentWs.Spec.Runtime.InitContainers, req.InitContainers) {
			currentWs.Spec.Runtime.InitContainers = req.InitContainers
			needsUpdate = true
		}

		if needsUpdate {
			if err := c.Update(ctx, currentWs); err != nil {
				return err
			}
			log.Printf("Updated existing Workspace spec: %s", wsName)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to update Workspace spec: %w", err)
	}

	// Always update LastActiveTime to now when the user starts/resumes the workspace,
	// which wakes it up from Sleeping state.
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		currentWs := &aiv1alpha1.Workspace{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, currentWs); err != nil {
			return err
		}
		currentWs.Status.LastActiveTime = &metav1.Time{Time: time.Now()}
		return c.Status().Update(ctx, currentWs)
	})
	if err != nil {
		return fmt.Errorf("failed to update Workspace lastActiveTime: %w", err)
	}
	log.Printf("Updated Workspace lastActiveTime: %s", wsName)
	return nil
}

// pollWorkspaceRunning polls the workspace status until it reaches Running or Failed state,
// and verifies that the Ingress route is healthy and serving before writing the JSON result.
func pollWorkspaceRunning(ctx context.Context, c client.Client, w http.ResponseWriter, userID, wsName, namespace, timeoutMsg string) {
	timeout := time.After(90 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			http.Error(w, timeoutMsg, http.StatusGatewayTimeout)
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			currentWs := &aiv1alpha1.Workspace{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, currentWs); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			if currentWs.Status.Phase == aiv1alpha1.WorkspaceFailed {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "Workspace failed to start",
					"phase": string(currentWs.Status.Phase),
				})
				return
			}

			if currentWs.Status.Phase == aiv1alpha1.WorkspaceRunning {
				if probeWorkspaceViaIngress(ctx, userID, wsName, currentWs) {
					url := getWorkspaceURL(userID, wsName, currentWs.Status.Endpoint)

					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]string{
						"url":   url,
						"phase": string(currentWs.Status.Phase),
					})
					return
				}
				// Ingress route still warming up or 404/503; continue polling until Ingress is ready
			}
		}
	}
}

// probeWorkspaceViaIngress sends an HTTP GET probe through Ingress to verify route readiness.
// When namespace is "bocomwork", it sets Cookie: oa=<effectiveID> (where effectiveID respects USER_CODE if configured)
// and probes the bocomwork Ingress endpoint.
func probeWorkspaceViaIngress(ctx context.Context, userID, wsName string, ws *aiv1alpha1.Workspace) bool {
	healthPath := ws.Spec.Runtime.HealthPath
	if healthPath == "" {
		healthPath = "/health"
	}
	if !strings.HasPrefix(healthPath, "/") {
		healthPath = "/" + healthPath
	}

	effectiveID := getEffectiveUserID(userID, ws.Namespace, ws.Spec.Runtime.Env)
	if effectiveID == "" && ws.Spec.Owner != "" {
		effectiveID = ws.Spec.Owner
	}

	var cookieHeader string
	var probeURL string

	if ws.Namespace == "bocomwork" {
		// In bocomwork namespace, Ingress routes via Cookie: oa=<effectiveID> (or WorkspaceUser)
		cookieHeader = fmt.Sprintf("oa=%s; WorkspaceUser=%s", effectiveID, effectiveID)

		var probeBaseURL string
		if envProbe := os.Getenv("WORKSPACE_INGRESS_PROBE_URL"); envProbe != "" {
			probeBaseURL = envProbe
		} else if envBase := os.Getenv("WORKSPACE_BASE_URL"); envBase != "" {
			probeBaseURL = envBase
		}

		if probeBaseURL != "" {
			basePrefix := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(probeBaseURL, "/"), "/ws"), "/")
			probeURL = fmt.Sprintf("%s%s", basePrefix, healthPath)
		} else {
			endpoint := ws.Status.Endpoint
			if endpoint == "" {
				domain := os.Getenv("WORKSPACE_DOMAIN")
				if domain == "" {
					domain = "localhost"
				}
				endpoint = fmt.Sprintf("%s.%s", wsName, domain)
			}
			probeURL = fmt.Sprintf("http://%s%s", endpoint, healthPath)
		}
	} else {
		// Non-bocomwork namespace: standard probing
		cookieHeader = fmt.Sprintf("WorkspaceUser=%s", effectiveID)

		endpoint := ws.Status.Endpoint
		if endpoint == "" {
			baseURL := os.Getenv("WORKSPACE_BASE_URL")
			if baseURL != "" {
				probeURL = fmt.Sprintf("%s%s", strings.TrimSuffix(baseURL, "/"), healthPath)
			} else {
				domain := os.Getenv("WORKSPACE_DOMAIN")
				if domain == "" {
					domain = "localhost"
				}
				endpoint = fmt.Sprintf("%s.%s", wsName, domain)
				probeURL = fmt.Sprintf("http://%s%s", endpoint, healthPath)
			}
		} else {
			probeURL = fmt.Sprintf("http://%s%s", endpoint, healthPath)
		}
	}

	if doSingleProbe(ctx, probeURL, cookieHeader) {
		return true
	}

	// If healthPath was not explicitly specified and returned non-healthy, fallback to probe root "/"
	if ws.Spec.Runtime.HealthPath == "" && healthPath != "/" {
		var rootProbeURL string
		if ws.Namespace == "bocomwork" {
			var probeBaseURL string
			if envProbe := os.Getenv("WORKSPACE_INGRESS_PROBE_URL"); envProbe != "" {
				probeBaseURL = envProbe
			} else if envBase := os.Getenv("WORKSPACE_BASE_URL"); envBase != "" {
				probeBaseURL = envBase
			}
			if probeBaseURL != "" {
				basePrefix := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(probeBaseURL, "/"), "/ws"), "/")
				rootProbeURL = fmt.Sprintf("%s/", basePrefix)
			} else {
				rootProbeURL = fmt.Sprintf("http://%s/", ws.Status.Endpoint)
			}
		} else {
			rootProbeURL = fmt.Sprintf("http://%s/", ws.Status.Endpoint)
		}
		if doSingleProbe(ctx, rootProbeURL, cookieHeader) {
			return true
		}
	}

	return false
}

// doSingleProbe sends a single probe request with Cookie header and checks whether Ingress successfully forwarded to the Pod.
func doSingleProbe(ctx context.Context, probeURL, cookieHeader string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		log.Printf("Failed to create Ingress probe request for %s: %v", probeURL, err)
		return false
	}

	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}
	req.Header.Set("User-Agent", "Workspace-Ingress-Probe/1.0")

	probeClient := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := probeClient.Do(req)
	if err != nil {
		log.Printf("Ingress probe to %s failed (network/connect): %v", probeURL, err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		log.Printf("Ingress probe to %s succeeded (status: %d)", probeURL, resp.StatusCode)
		return true
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		log.Printf("Ingress probe to %s reached target application (status: %d)", probeURL, resp.StatusCode)
		return true
	}

	log.Printf("Ingress probe to %s waiting for route sync (status: %d)", probeURL, resp.StatusCode)
	return false
}

func stopWorkspaceHandler(c client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			UserID    string              `json:"userId"`
			Namespace string              `json:"namespace,omitempty"`
			Env       []aiv1alpha1.EnvVar `json:"env,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		namespace := req.Namespace
		if namespace == "" {
			namespace = "default"
		}
		effectiveUserID := getEffectiveUserID(req.UserID, namespace, req.Env)
		wsName := sanitizeK8sName(fmt.Sprintf("ws-%s", effectiveUserID))

		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			ws := &aiv1alpha1.Workspace{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, ws); err != nil {
				return err
			}
			ws.Spec.Stopped = true
			return c.Update(ctx, ws)
		})
		if err != nil {
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
			UserID    string              `json:"userId"`
			Namespace string              `json:"namespace,omitempty"`
			Env       []aiv1alpha1.EnvVar `json:"env,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		namespace := req.Namespace
		if namespace == "" {
			namespace = "default"
		}
		effectiveUserID := getEffectiveUserID(req.UserID, namespace, req.Env)
		wsName := sanitizeK8sName(fmt.Sprintf("ws-%s", effectiveUserID))

		// Update stopped to false if it was manually stopped
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			ws := &aiv1alpha1.Workspace{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, ws); err != nil {
				return err
			}
			if ws.Spec.Stopped {
				ws.Spec.Stopped = false
				return c.Update(ctx, ws)
			}
			return nil
		})
		if err != nil {
			if apierrors.IsNotFound(err) {
				http.Error(w, "Workspace not found", http.StatusNotFound)
				return
			}
			log.Printf("Failed to resume Workspace spec: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Always update LastActiveTime to now when waking it up from Sleeping state
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			ws := &aiv1alpha1.Workspace{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: wsName}, ws); err != nil {
				return err
			}
			ws.Status.LastActiveTime = &metav1.Time{Time: time.Now()}
			return c.Status().Update(ctx, ws)
		})
		if err != nil {
			log.Printf("Failed to update Workspace lastActiveTime in wakeup: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("Successfully woke up Workspace lastActiveTime: %s", wsName)

		// Poll until Workspace status.phase is Running or Failed
		pollWorkspaceRunning(ctx, c, w, effectiveUserID, wsName, namespace, "Workspace wakeup timed out")
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

// sanitizeK8sName converts a raw string into a valid Kubernetes resource name (RFC 1123).
// Rules: lowercase, only [a-z0-9-], no leading/trailing hyphens, max 63 chars.
var invalidK8sChars = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeK8sName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	name = invalidK8sChars.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

var (
	portalUsername = os.Getenv("USERNAME")
	portalPassword = os.Getenv("PASSWORD")
	expectedToken  string
	authEnabled    bool
)

func init() {
	if portalUsername != "" {
		authEnabled = true
		hasher := sha256.New()
		hasher.Write([]byte(portalUsername + ":" + portalPassword))
		expectedToken = fmt.Sprintf("%x", hasher.Sum(nil))
		log.Printf("Portal authentication enabled. Username: %s", portalUsername)
	} else {
		log.Println("Portal authentication disabled (USERNAME not set).")
	}
}

func isAuthorized(r *http.Request) bool {
	if !authEnabled {
		return true
	}
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return false
	}
	return cookie.Value == expectedToken
}

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isAuthorized(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Username == portalUsername && req.Password == portalPassword {
		http.SetCookie(w, &http.Cookie{
			Name:     "session_token",
			Value:    expectedToken,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   86400 * 7, // 7 days
			SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true})
		return
	}

	http.Error(w, "用户名或密码错误", http.StatusUnauthorized)
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"success": true})
}

func authCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"authenticated": isAuthorized(r),
		"authEnabled":   authEnabled,
	})
}

//go:embed portal.html
var htmlTemplate string
