package egress

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicRoutesKeepGoodFileOnInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	c := Config{Domains: []Domain{{Host: "good.example.com", Port: 443}}}
	if err := ReplaceRoutes(path, c); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	c.Domains[0].Host = "*.example.com"
	if err := ReplaceRoutes(path, c); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("last valid snapshot lost")
	}
}
func TestLoadedRevisionRequiresActiveNamedRoute(t *testing.T) {
	fixtures := []struct {
		json string
		want string
	}{
		{`{"configs":[{"@type":"other","dynamic_route_configs":[{"version_info":"wrong","route_config":{"name":"egress"}}]}]}`, ""},
		{`{"configs":[{"@type":"type.googleapis.com/envoy.admin.v3.RoutesConfigDump","dynamic_route_configs":[{"version_info":"right","route_config":{"name":"egress"}}]}]}`, "right"},
		{`{"configs":[{"@type":"type.googleapis.com/envoy.admin.v3.RoutesConfigDump","dynamic_route_configs":[{"version_info":"old","route_config":{"name":"egress"},"error_state":{"details":"invalid"}}]}]}`, ""},
	}
	for _, f := range fixtures {
		got, err := LoadedRevision([]byte(f.json))
		if got != f.want || (f.want == "" && err == nil) {
			t.Fatalf("got %q %v for %s", got, err, f.json)
		}
	}
}
