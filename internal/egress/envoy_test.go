package egress

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRoutesUseExactAuthorityAndLocalDenial(t *testing.T) {
	c := Config{Domains: []Domain{{Host: "model.example.com", Port: 443}}}
	b, err := c.Routes()
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Version   string `json:"version_info"`
		Resources []struct {
			Hosts []struct {
				Domains []string         `json:"domains"`
				Routes  []map[string]any `json:"routes"`
			} `json:"virtual_hosts"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != c.Revision() {
		t.Fatal("missing revision")
	}
	hosts := snapshot.Resources[0].Hosts
	if len(hosts) != 2 || len(hosts[0].Domains) != 1 || hosts[0].Domains[0] != "model.example.com:443" {
		t.Fatalf("unexpected authority matching: %s", b)
	}
	for _, r := range hosts[1].Routes {
		if _, ok := r["route"]; ok {
			t.Fatal("default route forwards")
		}
		if r["direct_response"].(map[string]any)["status"] != float64(403) {
			t.Fatal("default route does not deny")
		}
	}
	empty, err := (Config{}).Routes()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), `"cluster": "forward"`) {
		t.Fatal("empty policy forwards traffic")
	}
}

func TestStreamingRoutesDisableIdleCutoff(t *testing.T) {
	c := Config{Domains: []Domain{{Host: "model.example.com", Port: 443}}}
	b, err := c.Routes()
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err = json.Unmarshal(b, &snapshot); err != nil {
		t.Fatal(err)
	}
	resource := snapshot["resources"].([]any)[0].(map[string]any)
	hosts := resource["virtual_hosts"].([]any)
	routes := hosts[0].(map[string]any)["routes"].([]any)
	for _, raw := range routes {
		route := raw.(map[string]any)["route"].(map[string]any)
		if route["timeout"] != "0s" || route["idle_timeout"] != "0s" {
			t.Fatal("authorized stream can time out", route)
		}
	}
}

// Envoy deduplicates identical RouteConfiguration resources even when the RDS
// envelope version changes. Non-domain edits must change the resource as well.
func TestRoutesRevisionChangesResourceForDirectRuleEdits(t *testing.T) {
	configs := []Config{
		{},
		{IPs: []IP{{CIDR: "192.0.2.1/32", Protocol: "TCP", Port: 80}}},
		{Services: []Service{{Name: "dns", Namespace: "kube-system", PodSelector: map[string]string{"k8s-app": "kube-dns"}, Protocol: "UDP", Port: 53}}},
	}
	for _, c := range configs {
		b, err := c.Routes()
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct {
			Resources []struct {
				Metadata struct {
					Filters map[string]struct {
						Revision string `json:"revision"`
					} `json:"filter_metadata"`
				} `json:"metadata"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(b, &snapshot); err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Resources) != 1 || snapshot.Resources[0].Metadata.Filters["workspace.egress"].Revision != c.Revision() {
			t.Fatalf("resource lacks current revision: %s", b)
		}
		if strings.Contains(string(b), `"cluster": "forward"`) {
			t.Fatal("direct-only policy introduced forwarding")
		}
	}
}
