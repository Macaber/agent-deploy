package main

import (
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
				http.Error(w, "Missing userId parameter", http.StatusBadRequest)
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

const htmlTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>BocomWork Workspace - Sign In</title>
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
    <link href="https://fonts.googleapis.com/css2?family=Outfit:wght@300;400;600;700&display=swap" rel="stylesheet">
    <style>
        :root {
            --bg-color: #f8fafc;
            --card-bg: rgba(255, 255, 255, 0.85);
            --primary: #0284c7;
            --primary-hover: #0369a1;
            --accent: #0ea5e9;
            --text-main: #0f172a;
            --text-muted: #64748b;
        }

        body {
            font-family: 'Outfit', sans-serif;
            background-color: var(--bg-color);
            color: var(--text-main);
            display: flex;
            justify-content: center;
            align-items: center;
            height: 100vh;
            margin: 0;
            overflow: hidden;
            position: relative;
        }

        /* Ambient Glow Effects */
        .ambient-glow-1 {
            position: absolute;
            width: 400px;
            height: 400px;
            background: radial-gradient(circle, rgba(14, 165, 233, 0.12) 0%, rgba(255, 255, 255, 0) 70%);
            top: -100px;
            left: -100px;
            z-index: 0;
        }

        .ambient-glow-2 {
            position: absolute;
            width: 500px;
            height: 500px;
            background: radial-gradient(circle, rgba(59, 130, 246, 0.08) 0%, rgba(255, 255, 255, 0) 70%);
            bottom: -150px;
            right: -150px;
            z-index: 0;
        }

        .card {
            position: relative;
            background: var(--card-bg);
            backdrop-filter: blur(20px);
            -webkit-backdrop-filter: blur(20px);
            border: 1px solid rgba(226, 232, 240, 0.8);
            border-radius: 20px;
            padding: 48px;
            box-shadow: 0 20px 40px -10px rgba(15, 23, 42, 0.08);
            width: 380px;
            z-index: 10;
            transition: all 0.4s cubic-bezier(0.4, 0, 0.2, 1);
        }

        /* Brand & Logo Header */
        .brand-container {
            display: flex;
            flex-direction: column;
            align-items: center;
            margin-bottom: 36px;
        }

        .logo-svg {
            width: 56px;
            height: 56px;
            margin-bottom: 16px;
            filter: drop-shadow(0 4px 12px rgba(14, 165, 233, 0.35));
        }

        .brand-name {
            font-size: 26px;
            font-weight: 700;
            letter-spacing: 1px;
            background: linear-gradient(135deg, #0f172a 0%, #334155 100%);
            -webkit-background-clip: text;
            -webkit-text-fill-color: transparent;
            margin: 0;
        }

        .brand-subtitle {
            font-size: 13px;
            color: var(--text-muted);
            margin-top: 4px;
            letter-spacing: 2px;
            text-transform: uppercase;
        }

        /* Form Controls */
        .input-group {
            position: relative;
            margin-bottom: 24px;
            text-align: left;
        }

        .input-label {
            display: block;
            font-size: 13px;
            font-weight: 600;
            color: var(--text-muted);
            margin-bottom: 8px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
        }

        .input-wrapper {
            position: relative;
        }

        .input-field {
            width: 100%;
            padding: 14px 16px;
            border-radius: 10px;
            border: 1px solid #cbd5e1;
            background-color: #ffffff;
            color: var(--text-main);
            font-size: 15px;
            box-sizing: border-box;
            outline: none;
            transition: all 0.3s;
        }

        .input-field:focus {
            border-color: var(--accent);
            box-shadow: 0 0 0 3px rgba(14, 165, 233, 0.15);
            background-color: #ffffff;
        }

        /* Action Button */
        .btn-submit {
            width: 100%;
            padding: 14px;
            border-radius: 10px;
            border: none;
            background: linear-gradient(135deg, #0284c7 0%, #0369a1 100%);
            color: #ffffff;
            font-size: 16px;
            font-weight: 700;
            cursor: pointer;
            transition: all 0.3s;
            box-shadow: 0 4px 12px rgba(2, 132, 199, 0.3);
            letter-spacing: 0.5px;
        }

        .btn-submit:hover {
            background: linear-gradient(135deg, #0ea5e9 0%, #0284c7 100%);
            box-shadow: 0 6px 16px rgba(2, 132, 199, 0.5);
            transform: translateY(-1px);
        }

        .btn-submit:active {
            transform: translateY(1px);
        }

        /* Views management */
        #loadingView {
            display: none;
            flex-direction: column;
            align-items: center;
            justify-content: center;
            padding: 20px 0;
        }

        /* Tech Circle Loader */
        .loader-container {
            position: relative;
            width: 80px;
            height: 80px;
            margin-bottom: 24px;
        }

        .loader-ring {
            position: absolute;
            width: 100%;
            height: 100%;
            border: 3px solid rgba(14, 165, 233, 0.1);
            border-radius: 50%;
        }

        .loader-spinner {
            position: absolute;
            width: 100%;
            height: 100%;
            border: 3px solid transparent;
            border-top: 3px solid var(--accent);
            border-radius: 50%;
            animation: spin 1s cubic-bezier(0.5, 0.1, 0.4, 0.9) infinite;
        }

        .loader-glow {
            position: absolute;
            width: 6px;
            height: 6px;
            background-color: var(--accent);
            border-radius: 50%;
            top: 6px;
            left: 50%;
            transform: translateX(-50%);
            box-shadow: 0 0 8px var(--accent);
        }

        @keyframes spin {
            0% { transform: rotate(0deg); }
            100% { transform: rotate(360deg); }
        }

        .status-title {
            font-size: 18px;
            font-weight: 600;
            margin-bottom: 8px;
            color: var(--text-main);
        }

        .status-desc {
            font-size: 14px;
            color: var(--text-muted);
            text-align: center;
            min-height: 20px;
        }

        /* Footer credits */
        .footer-credit {
            margin-top: 24px;
            font-size: 11px;
            color: rgba(15, 23, 42, 0.35);
            letter-spacing: 1px;
            text-transform: uppercase;
        }

        .error-message {
            color: #ef4444;
            font-size: 13px;
            margin-top: 12px;
            min-height: 18px;
            text-align: center;
        }
    </style>
</head>
<body>
    <div class="ambient-glow-1"></div>
    <div class="ambient-glow-2"></div>

    <div class="card" id="cardContainer">
        <!-- Brand Header (Common) -->
        <div class="brand-container">
            <svg class="logo-svg" viewBox="0 0 64 64" fill="none" xmlns="http://www.w3.org/2000/svg">
                <path d="M32 2L60 18V50L32 62L4 50V18L32 2Z" stroke="url(#logoGrad)" stroke-width="3" stroke-linejoin="round"/>
                <path d="M32 12L51 23V41L32 52L13 41V23L32 12Z" fill="url(#logoFillGrad)" opacity="0.85"/>
                <circle cx="32" cy="32" r="6" fill="#0284c7" />
                <defs>
                    <linearGradient id="logoGrad" x1="4" y1="2" x2="60" y2="62" gradientUnits="userSpaceOnUse">
                        <stop stop-color="#0ea5e9"/>
                        <stop offset="1" stop-color="#2563eb"/>
                    </linearGradient>
                    <linearGradient id="logoFillGrad" x1="13" y1="12" x2="51" y2="52" gradientUnits="userSpaceOnUse">
                        <stop stop-color="rgba(14, 165, 233, 0.15)"/>
                        <stop offset="1" stop-color="rgba(37, 99, 235, 0.15)"/>
                    </linearGradient>
                </defs>
            </svg>
            <h2 class="brand-name">BOCOMWORK</h2>
            <div class="brand-subtitle">Workspace Portal</div>
        </div>

        <!-- View 1: Login Form -->
        <div id="loginView">
            <div class="input-group">
                <label class="input-label" for="username">用户名 / 用户 ID</label>
                <div class="input-wrapper">
                    <input class="input-field" type="text" id="username" placeholder="输入您的用户名，例如: aaa" autocomplete="off">
                </div>
            </div>
            
            <div class="input-group">
                <label class="input-label" for="password">登录密码 (可选)</label>
                <div class="input-wrapper">
                    <input class="input-field" type="password" id="password" placeholder="••••••••">
                </div>
            </div>

            <button class="btn-submit" onclick="handleLoginSubmit()">登录并进入空间</button>
            <div class="error-message" id="errorBox"></div>
        </div>

        <!-- View 2: Loading State -->
        <div id="loadingView">
            <div class="loader-container">
                <div class="loader-ring"></div>
                <div class="loader-spinner"></div>
                <div class="loader-glow"></div>
            </div>
            <div class="status-title" id="statusTitle">身份验证中...</div>
            <div class="status-desc" id="statusDesc">请稍候，系统正在校验并连接您的工作空间</div>
        </div>

        <!-- Footer -->
        <div class="footer-credit">© 2026 BocomWork Cloud IDE</div>
    </div>

    <script>
        // Form submission handler
        async function handleLoginSubmit() {
            const usernameInput = document.getElementById('username').value.trim();
            const errorBox = document.getElementById('errorBox');
            
            if (!usernameInput) {
                errorBox.innerText = "请输入您的用户名！";
                return;
            }
            
            // Clear errors and transition UI to Loading
            errorBox.innerText = "";
            const loginView = document.getElementById('loginView');
            const loadingView = document.getElementById('loadingView');
            
            loginView.style.display = "none";
            loadingView.style.display = "flex";

            try {
                // Step 1: Check Workspace status
                setStatus("正在验证...", "正在获取工作空间状态，请稍候...");
                const statusRes = await fetch('/api/workspaces?userId=' + encodeURIComponent(usernameInput));
                if (!statusRes.ok) {
                    throw new Error("查询工作空间状态失败: " + await statusRes.text());
                }
                
                const statusData = await statusRes.json();
                
                if (statusData.exists) {
                    // Workspace exists! Check the phase.
                    if (statusData.phase === "Running") {
                        // Already running! Redirect immediately
                        setStatus("验证成功！", "工作空间已就绪，正在进入系统...");
                        setTimeout(() => {
                            window.location.href = statusData.url;
                        }, 800);
                        return;
                    } else if (statusData.phase === "Sleeping" || statusData.phase === "Stopped") {
                        // Exists but is sleeping or stopped. Call Wakeup endpoint!
                        const actionText = statusData.phase === "Sleeping" ? "自动唤醒" : "重新拉起";
                        setStatus('正在' + actionText + '...', '正在请求 K8s 集群激活您的工作空间容器...');
                        
                        const wakeupRes = await fetch('/api/workspaces/wakeup', {
                            method: 'POST',
                            headers: { 'Content-Type': 'application/json' },
                            body: JSON.stringify({ userId: usernameInput })
                        });
                        
                        if (!wakeupRes.ok) {
                            throw new Error("唤醒/重新启动工作空间失败: " + await wakeupRes.text());
                        }
                        
                        const wakeupData = await wakeupRes.json();
                        setStatus("就绪中...", "已拉起容器服务，正在更新域名路由...");
                        
                        // Wait for K8s routing and redirection
                        setTimeout(() => {
                            window.location.href = wakeupData.url;
                        }, 1200);
                        return;
                    } else if (statusData.phase === "Starting" || statusData.phase === "Pending") {
                        // In intermediate states. Wait and poll status until Running
                        setStatus("容器启动中...", "正在调度集群算力并部署存储，请稍候...");
                        await pollForRunningState(usernameInput);
                        return;
                    } else {
                        // Failed or other phases. Trigger startup endpoint with defaults
                        setStatus("重新部署中...", "工作空间处于异常状态，正在为您重建容器...");
                        await triggerStartup(usernameInput);
                        return;
                    }
                } else {
                    // Does not exist. Trigger normal create/startup endpoint
                    setStatus("初始化中...", "首次访问，系统正在为您自动生成专属工作空间...");
                    await triggerStartup(usernameInput);
                }
            } catch (err) {
                // Show login view again and display error
                loginView.style.display = "block";
                loadingView.style.display = "none";
                errorBox.innerText = err.message;
            }
        }

        // Helper to update loading status UI
        function setStatus(title, desc) {
            document.getElementById('statusTitle').innerText = title;
            document.getElementById('statusDesc').innerText = desc;
        }

        // Trigger POST /api/workspaces (Create/Startup)
        async function triggerStartup(userId) {
            const startupRes = await fetch('/api/workspaces', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ userId })
            });
            
            if (!startupRes.ok) {
                throw new Error("静态初始化工作空间失败: " + await startupRes.text());
            }
            
            const data = await startupRes.json();
            setStatus("就绪中...", "专属容器初始化成功，正在配置内网网关...");
            
            setTimeout(() => {
                window.location.href = data.url;
            }, 1200);
        }

        // Poll Workspace endpoint until state is Running
        async function pollForRunningState(userId) {
            const maxRetries = 45; // 90 seconds (2s intervals)
            for (let i = 0; i < maxRetries; i++) {
                try {
                    const statusRes = await fetch('/api/workspaces?userId=' + encodeURIComponent(userId));
                    if (statusRes.ok) {
                        const statusData = await statusRes.json();
                        if (statusData.phase === "Running") {
                            setStatus("拉起就绪！", "工作空间已就绪，正在跳转进入...");
                            setTimeout(() => {
                                window.location.href = statusData.url;
                            }, 1000);
                            return;
                        }
                    }
                } catch (e) {
                    // Ignore single polling network errors
                }
                await new Promise(resolve => setTimeout(resolve, 2000));
            }
            throw new Error("工作空间拉起超时，请刷新页面重新登录！");
        }
    </script>
</body>
</html>
`
