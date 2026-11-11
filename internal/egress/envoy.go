package egress

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

type object = map[string]any

// Routes emits a single filesystem RDS snapshot. Only authorized authorities
// have forwarding routes; the catch-all is a local denial. Ports are never
// stripped from authority matching. CONNECT has no path and needs its own match.
func (c Config) Routes() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	hosts := []any{}
	for i, r := range c.Domains {
		authority := fmt.Sprintf("%s:%d", r.Host, r.Port)
		domains := []string{authority}
		if r.Port == 80 {
			domains = append(domains, r.Host)
		}
		connect := object{"match": object{"connect_matcher": object{}, "headers": []any{object{"name": ":authority", "string_match": object{"exact": authority}}}}, "route": object{"cluster": "forward", "timeout": "0s", "idle_timeout": "0s", "upgrade_configs": []any{object{"upgrade_type": "CONNECT", "connect_config": object{}}}}}
		plain := object{"match": object{"prefix": "/", "headers": []any{object{"name": ":method", "string_match": object{"exact": "CONNECT"}, "invert_match": true}}}, "route": object{"cluster": "forward", "timeout": "0s", "idle_timeout": "0s"}}
		hosts = append(hosts, object{"name": fmt.Sprintf("allow_%d", i), "domains": domains, "routes": []any{connect, plain}})
	}
	// IPv4 TCP rules authorize literal destinations only. Never resolve a
	// domain here: an unlisted domain must not inherit permission from its IP.
	ipRoutes := []any{}
	for _, r := range c.IPs {
		prefix := netip.MustParsePrefix(r.CIDR)
		if r.Protocol != "TCP" || !prefix.Addr().Is4() {
			continue
		}
		authority := ipv4AuthorityPattern(prefix, r.Port, false)
		header := func(pattern string) any {
			return object{"name": ":authority", "string_match": object{"safe_regex": object{"google_re2": object{}, "regex": pattern}}}
		}
		ipRoutes = append(ipRoutes,
			object{"match": object{"connect_matcher": object{}, "headers": []any{header(authority)}}, "route": object{"cluster": "forward", "timeout": "0s", "idle_timeout": "0s", "upgrade_configs": []any{object{"upgrade_type": "CONNECT", "connect_config": object{}}}}},
			object{"match": object{"prefix": "/", "headers": []any{header(ipv4AuthorityPattern(prefix, r.Port, true)), object{"name": ":method", "string_match": object{"exact": "CONNECT"}, "invert_match": true}}}, "route": object{"cluster": "forward", "timeout": "0s", "idle_timeout": "0s"}},
		)
	}
	// Include both CONNECT and ordinary requests in the explicit local rejection.
	hosts = append(hosts, object{"name": "deny", "domains": []string{"*"}, "routes": append(ipRoutes, []any{
		object{"match": object{"connect_matcher": object{}}, "direct_response": object{"status": 403}},
		object{"match": object{"prefix": "/"}, "direct_response": object{"status": 403}},
	}...)})
	return json.MarshalIndent(object{"version_info": c.Revision(), "resources": []any{object{"@type": "type.googleapis.com/envoy.config.route.v3.RouteConfiguration", "name": "egress", "metadata": object{"filter_metadata": object{"workspace.egress": object{"revision": c.Revision()}}}, "virtual_hosts": hosts}}}, "", "  ")
}

// Bootstrap is fixed for the deployment lifetime; only routes.json changes.
// The admin endpoint is loopback-only and must never be exposed by the Service.
func Bootstrap(nodeID, dynamicPath string) ([]byte, error) {
	cache := object{"name": "egress_dns", "dns_lookup_family": "V4_ONLY", "dns_refresh_rate": "30s", "dns_min_refresh_rate": "5s", "dns_failure_refresh_rate": object{"base_interval": "2s", "max_interval": "10s"}, "host_ttl": "300s", "max_hosts": 1024}
	hcm := object{
		"@type":       "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
		"stat_prefix": "egress", "codec_type": "AUTO", "stream_idle_timeout": "300s",
		"http_protocol_options":   object{"allow_absolute_url": true},
		"request_headers_timeout": "10s",
		"upgrade_configs":         []any{object{"upgrade_type": "CONNECT"}},
		"rds":                     object{"route_config_name": "egress", "config_source": object{"path_config_source": object{"path": dynamicPath}, "resource_api_version": "V3"}},
		"http_filters": []any{
			object{"name": "envoy.filters.http.dynamic_forward_proxy", "typed_config": object{"@type": "type.googleapis.com/envoy.extensions.filters.http.dynamic_forward_proxy.v3.FilterConfig", "dns_cache_config": cache}},
			object{"name": "envoy.filters.http.router", "typed_config": object{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}},
		},
	}
	return json.MarshalIndent(object{
		"layered_runtime": object{"layers": []any{object{"name": "regex_limits", "static_layer": object{"re2.max_program_size.error_level": 2048}}}},
		"node":            object{"id": nodeID, "cluster": "workspace-egress"},
		"admin":           object{"address": object{"socket_address": object{"address": "127.0.0.1", "port_value": 9901}}},
		"static_resources": object{
			"listeners": []any{object{"name": "proxy", "address": object{"socket_address": object{"address": "0.0.0.0", "port_value": 3128}}, "filter_chains": []any{object{"filters": []any{object{"name": "envoy.filters.network.http_connection_manager", "typed_config": hcm}}}}}},
			"clusters":  []any{object{"name": "forward", "connect_timeout": "10s", "lb_policy": "CLUSTER_PROVIDED", "cluster_type": object{"name": "envoy.clusters.dynamic_forward_proxy", "typed_config": object{"@type": "type.googleapis.com/envoy.extensions.clusters.dynamic_forward_proxy.v3.ClusterConfig", "dns_cache_config": cache}}}},
		},
	}, "", "  ")
}

// ipv4AuthorityPattern matches canonical dotted-decimal literals in a prefix,
// with an exact port. No shorthand, leading zeroes, suffixes or DNS names match.
// Enumerating at most one partial octet keeps generation bounded for any CIDR.
func ipv4AuthorityPattern(prefix netip.Prefix, port int32, plain bool) string {
	octets := prefix.Addr().As4()
	parts := make([]string, 4)
	bits := prefix.Bits()
	for i, octet := range octets {
		switch {
		case bits >= 8:
			parts[i] = strconv.Itoa(int(octet))
		case bits <= 0:
			parts[i] = `(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])`
		default:
			values := []string{}
			for n := int(octet); n < int(octet)+(1<<(8-bits)); n++ {
				values = append(values, strconv.Itoa(n))
			}
			parts[i] = "(?:" + strings.Join(values, "|") + ")"
		}
		bits -= 8
	}
	suffix := ":" + strconv.Itoa(int(port))
	if plain && port == 80 {
		suffix = "(?::80)?"
	}
	return "^" + strings.Join(parts, `\.`) + suffix + "$"
}
