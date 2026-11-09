package egress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ReplaceRoutes uses a same-directory rename, the filesystem notification Envoy
// subscribes to. Production runs on Linux. No in-place edits or symlink swaps.
func ReplaceRoutes(path string, c Config) error {
	data, err := c.Routes()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".routes-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err = f.Chmod(0644); err != nil {
		_ = f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadedRevision inspects the active named RDS resource, not an arbitrary
// occurrence of version_info or a filesystem write acknowledgement.
func LoadedRevision(data []byte) (string, error) {
	var dump struct {
		Configs []struct {
			Type   string `json:"@type"`
			Routes []struct {
				Version string `json:"version_info"`
				Route   struct {
					Name string `json:"name"`
				} `json:"route_config"`
				Error json.RawMessage `json:"error_state"`
			} `json:"dynamic_route_configs"`
		} `json:"configs"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		return "", err
	}
	for _, c := range dump.Configs {
		if c.Type != "type.googleapis.com/envoy.admin.v3.RoutesConfigDump" {
			continue
		}
		for _, r := range c.Routes {
			if r.Route.Name == "egress" {
				if len(r.Error) > 0 && string(r.Error) != "null" {
					return "", fmt.Errorf("Envoy rejected route configuration: %s", r.Error)
				}
				if r.Version == "" {
					return "", fmt.Errorf("active route has no version")
				}
				return r.Version, nil
			}
		}
	}
	return "", fmt.Errorf("egress routes have not been loaded")
}
