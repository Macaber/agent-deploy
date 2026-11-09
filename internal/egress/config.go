// Package egress defines the unified outbound allowlist shared by the API,
// controller and Envoy configuration synchronizer.
package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

const ConfigMapName = "workspace-egress-config"
const ConfigKey = "policy.json"
const MigrationLabel = "workspace.bocom.io/egress-version"
const MigrationVersion = "envoy-v1"

type IP struct {
	CIDR     string `json:"cidr"`
	Protocol string `json:"protocol"`
	Port     int32  `json:"port"`
}
type Domain struct {
	Host string `json:"host"`
	Port int32  `json:"port"`
}
type Service struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	PodSelector map[string]string `json:"podSelector"`
	Protocol    string            `json:"protocol"`
	Port        int32             `json:"port"`
}
type Config struct {
	IPs      []IP      `json:"ips"`
	Domains  []Domain  `json:"domains"`
	Services []Service `json:"services"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Validate does not resolve domains. DNS changes are handled by Envoy.
func (c Config) Validate() error {
	if len(c.IPs)+len(c.Domains)+len(c.Services) > 1000 {
		return fmt.Errorf("at most 1000 rules are allowed")
	}
	seen := map[string]bool{}
	check := func(key string, port int32, protocol string) error {
		if port < 1 || port > 65535 {
			return fmt.Errorf("port must be between 1 and 65535")
		}
		if protocol != "TCP" && protocol != "UDP" {
			return fmt.Errorf("protocol must be TCP or UDP")
		}
		if seen[key] {
			return fmt.Errorf("duplicate rule: %s", key)
		}
		seen[key] = true
		return nil
	}
	for _, r := range c.IPs {
		p, err := netip.ParsePrefix(r.CIDR)
		if err != nil || p != p.Masked() || p.Bits() == 0 || p.Addr().Is4In6() {
			return fmt.Errorf("invalid canonical CIDR: %s", r.CIDR)
		}
		if err := check(fmt.Sprintf("ip:%s:%s:%d", r.CIDR, r.Protocol, r.Port), r.Port, r.Protocol); err != nil {
			return err
		}
	}
	for _, r := range c.Domains {
		if len(r.Host) > 253 || r.Host != strings.ToLower(r.Host) || !strings.Contains(r.Host, ".") {
			return fmt.Errorf("domain must be a lowercase full DNS name: %s", r.Host)
		}
		if _, err := netip.ParseAddr(r.Host); err == nil {
			return fmt.Errorf("IP literals belong in direct rules")
		}
		for _, label := range strings.Split(r.Host, ".") {
			if !dnsLabel.MatchString(label) {
				return fmt.Errorf("invalid domain: %s", r.Host)
			}
		}
		if err := check(fmt.Sprintf("domain:%s:%d", r.Host, r.Port), r.Port, "TCP"); err != nil {
			return err
		}
	}
	for _, r := range c.Services {
		if r.Name == "" || !dnsLabel.MatchString(r.Namespace) || len(r.PodSelector) == 0 {
			return fmt.Errorf("base service requires name, namespace and nonempty pod selector")
		}
		b, _ := json.Marshal(r.PodSelector)
		if err := check(fmt.Sprintf("service:%s:%s:%s:%d", r.Namespace, b, r.Protocol, r.Port), r.Port, r.Protocol); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) Revision() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
