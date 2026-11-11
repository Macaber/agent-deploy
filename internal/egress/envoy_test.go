package egress

import (
	"encoding/json"
	"net/netip"
	"regexp"
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
		if len(c.IPs) == 0 && strings.Contains(string(b), `"cluster": "forward"`) {
			t.Fatal("direct-only policy introduced forwarding")
		}
	}
}

func TestIPv4AuthorityPatterns(t *testing.T) {
	for bits := 1; bits <= 32; bits++ {
		p := netip.PrefixFrom(netip.MustParseAddr("12.244.130.39"), bits).Masked()
		re := regexp.MustCompile(ipv4AuthorityPattern(p, 80, true))
		for _, addr := range []string{"0.0.0.0", "12.244.130.39", "12.244.130.38", "12.244.130.40", "12.244.129.255", "12.244.131.0", "127.0.0.1", "255.255.255.255"} {
			for _, suffix := range []string{"", ":80"} {
				if re.MatchString(addr+suffix) != p.Contains(netip.MustParseAddr(addr)) {
					t.Fatalf("%s mismatched %s", p, addr+suffix)
				}
			}
		}
		for _, bad := range []string{"12.244.130.39:81", "12.244.130.39:080", "012.244.130.39", "12.244.130.39.evil", "user@12.244.130.39", "12.244.130.256", "12.244.130.39:80.evil", "[::ffff:12.244.130.39]:80"} {
			if re.MatchString(bad) {
				t.Fatalf("%s accepted %s", p, bad)
			}
		}
	}
	re := regexp.MustCompile(ipv4AuthorityPattern(netip.MustParsePrefix("12.244.130.39/32"), 443, false))
	if !re.MatchString("12.244.130.39:443") || re.MatchString("12.244.130.39") || re.MatchString("12.244.130.39:80") {
		t.Fatal("CONNECT port matching")
	}
}

func TestIPRoutesExcludeUDPAndIPv6(t *testing.T) {
	for _, rule := range []IP{{CIDR: "192.0.2.0/24", Protocol: "UDP", Port: 80}, {CIDR: "2001:db8::/32", Protocol: "TCP", Port: 443}} {
		b, err := (Config{IPs: []IP{rule}}).Routes()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"cluster": "forward"`) {
			t.Fatal("unsupported rule forwards")
		}
	}
	c := Config{IPs: []IP{{CIDR: "192.0.2.0/24", Protocol: "TCP", Port: 80}}}
	b, err := c.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"safe_regex"`) || !strings.Contains(string(b), `"cluster": "forward"`) {
		t.Fatal("missing IP forwarding")
	}
}

// Exercise the emitted catch-all routes in order, including method and port
// constraints. This checks the generated RDS policy, not just the regex helper.
func TestIPRouteAuthorizationAndRemoval(t *testing.T) {
	c := Config{IPs: []IP{{CIDR: "12.244.130.0/24", Protocol: "TCP", Port: 80}, {CIDR: "12.244.130.39/32", Protocol: "TCP", Port: 443}, {CIDR: "12.244.130.0/24", Protocol: "UDP", Port: 53}}}
	allowed := func(c Config, method, authority string) bool {
		t.Helper()
		b, err := c.Routes()
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct {
			Resources []struct {
				Hosts []struct {
					Routes []struct {
						Match struct {
							Connect *struct{} `json:"connect_matcher"`
							Headers []struct {
								Name   string `json:"name"`
								Invert bool   `json:"invert_match"`
								String struct {
									Exact string `json:"exact"`
									Regex *struct {
										Pattern string `json:"regex"`
									} `json:"safe_regex"`
								} `json:"string_match"`
							} `json:"headers"`
						} `json:"match"`
						Route map[string]any `json:"route"`
					} `json:"routes"`
				} `json:"virtual_hosts"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(b, &snapshot); err != nil {
			t.Fatal(err)
		}
		for _, route := range snapshot.Resources[0].Hosts[0].Routes {
			if (route.Match.Connect != nil) != (method == "CONNECT") {
				continue
			}
			match := true
			for _, h := range route.Match.Headers {
				value := authority
				if h.Name == ":method" {
					value = method
				}
				ok := value == h.String.Exact
				if h.String.Regex != nil {
					ok = regexp.MustCompile(h.String.Regex.Pattern).MatchString(value)
				}
				if h.Invert {
					ok = !ok
				}
				match = match && ok
			}
			if match {
				return route.Route != nil
			}
		}
		return false
	}
	for _, tc := range []struct {
		method, authority string
		want              bool
	}{
		{"GET", "12.244.130.39", true}, {"GET", "12.244.130.255:80", true},
		{"CONNECT", "12.244.130.39:443", true}, {"CONNECT", "12.244.130.39:80", true},
		{"CONNECT", "12.244.130.39", false}, {"GET", "12.244.131.0:80", false},
		{"CONNECT", "12.244.130.40:443", false}, {"GET", "12.244.130.39:53", false},
		{"GET", "unlisted.example:80", false}, {"GET", "12.244.130.39.evil:80", false},
	} {
		if got := allowed(c, tc.method, tc.authority); got != tc.want {
			t.Errorf("%s %s: got %v", tc.method, tc.authority, got)
		}
		if allowed(Config{}, tc.method, tc.authority) {
			t.Fatal("removed rule still authorized")
		}
	}
}
