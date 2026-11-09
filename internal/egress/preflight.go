package egress

import (
	"encoding/json"
	"fmt"
)

// MigrationCheck is deliberately separate from syntax validation: saving a
// deny-all business policy is valid, but automatic migration requires explicit
// DNS infrastructure rules. No default destination is silently authorized.
func (c Config) MigrationCheck() error {
	pairs := map[string]map[string]bool{}
	add := func(key, protocol string) {
		if pairs[key] == nil {
			pairs[key] = map[string]bool{}
		}
		pairs[key][protocol] = true
	}
	for _, r := range c.Services {
		if r.Port == 53 {
			b, _ := json.Marshal(r.PodSelector)
			add("svc:"+r.Namespace+":"+string(b), r.Protocol)
		}
	}
	for _, r := range c.IPs {
		if r.Port == 53 {
			add("ip:"+r.CIDR, r.Protocol)
		}
	}
	for _, p := range pairs {
		if p["TCP"] && p["UDP"] {
			return nil
		}
	}
	return fmt.Errorf("migration requires explicit TCP and UDP port 53 rules for the same DNS destination; verify CoreDNS or NodeLocal DNS before enabling migration")
}
