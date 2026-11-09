package main

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/example/workspace-operator/internal/egress"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type egressDocument struct {
	Config          egress.Config `json:"config"`
	ResourceVersion string        `json:"resourceVersion"`
	Revision        string        `json:"revision"`
	State           string        `json:"state"`
	Warnings        []string      `json:"warnings,omitempty"`
}

// Authentication is deliberately the same optional portal authentication used
// elsewhere. Ordinary Workspace handlers retain their existing public access.
func egressConfigHandler(k client.Client) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		namespace := egress.Namespace()
		switch r.Method {
		case http.MethodGet:
			c, version, err := egress.Read(r.Context(), k, namespace)
			state := "saved"
			if apierrors.IsNotFound(err) {
				c = egress.Config{}
				err = nil
				state = "unconfigured"
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			var warnings []string
			if err := c.MigrationCheck(); err != nil {
				warnings = append(warnings, err.Error())
			}
			_ = json.NewEncoder(w).Encode(egressDocument{Config: c, ResourceVersion: version, Revision: c.Revision(), State: state, Warnings: warnings})
		case http.MethodPut:
			r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
			var doc egressDocument
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&doc); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var trailing any
			if err := decoder.Decode(&trailing); err != io.EOF {
				http.Error(w, "Expected one JSON document", http.StatusBadRequest)
				return
			}
			if err := doc.Config.Validate(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := egress.Save(r.Context(), k, namespace, doc.ResourceVersion, doc.Config); err != nil {
				status := http.StatusInternalServerError
				if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
					status = http.StatusConflict
				}
				http.Error(w, err.Error(), status)
				return
			}
			// Saving is not a claim that the controller or any proxy has loaded it.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "saved", "revision": doc.Config.Revision()})
		default:
			w.Header().Set("Allow", "GET, PUT")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
