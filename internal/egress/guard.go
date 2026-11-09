package egress

import (
	"fmt"
	"net/netip"
)

// GuardCommands protects actual destination IPs at socket egress, including
// loopback. Only Envoy UID 101 is constrained; the sync UID cannot be impersonated
// by Envoy because runtime containers must drop SETUID and all other capabilities.
// DNS and protected networks are explicit infrastructure inputs, not business grants.
func GuardCommands(protected, dns []string) ([][]string, error) {
	if len(protected) == 0 || len(dns) == 0 {
		return nil, fmt.Errorf("explicit protected networks and DNS servers are required")
	}
	for _, s := range protected {
		p, e := netip.ParsePrefix(s)
		if e != nil || p != p.Masked() {
			return nil, fmt.Errorf("invalid protected network %q", s)
		}
	}
	for _, s := range dns {
		a, e := netip.ParseAddr(s)
		if e != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() {
			return nil, fmt.Errorf("invalid IPv4 DNS server %q", s)
		}
	}
	commands := [][]string{}
	add := func(binary string, args ...string) {
		commands = append(commands, append([]string{binary, "-w", "5"}, args...))
	}
	for _, binary := range []string{"iptables", "ip6tables"} {
		add(binary, "-N", "EGRESS_ENVOY")
		add(binary, "-A", "EGRESS_ENVOY", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN")
		if binary == "iptables" {
			for _, ip := range dns {
				for _, proto := range []string{"tcp", "udp"} {
					add(binary, "-A", "EGRESS_ENVOY", "-p", proto, "-m", "conntrack", "--ctorigdst", ip, "--ctorigdstport", "53", "--ctdir", "ORIGINAL", "-j", "RETURN")
				}
			}
			// Local includes the Pod's own addresses, not just 127/8.
			add(binary, "-A", "EGRESS_ENVOY", "-m", "addrtype", "--dst-type", "LOCAL", "-j", "REJECT")
			blocks := append([]string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4"}, protected...)
			for _, cidr := range blocks {
				p, _ := netip.ParsePrefix(cidr)
				if p.Addr().Is4() {
					add(binary, "-A", "EGRESS_ENVOY", "-d", cidr, "-j", "REJECT")
				}
			}
			add(binary, "-A", "EGRESS_ENVOY", "-p", "tcp", "-j", "RETURN")
		}
		// First version deliberately has no IPv6 upstream capability.
		add(binary, "-A", "EGRESS_ENVOY", "-j", "REJECT")
		add(binary, "-I", "OUTPUT", "1", "-m", "owner", "--uid-owner", "101", "-j", "EGRESS_ENVOY")
	}
	return commands, nil
}
