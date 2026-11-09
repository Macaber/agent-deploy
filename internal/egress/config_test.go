package egress

import "testing"

func TestRejectInvalidRules(t *testing.T) {
	cases := []Config{
		{IPs: []IP{{CIDR: "0.0.0.0/0", Protocol: "TCP", Port: 80}}},
		{IPs: []IP{{CIDR: "12.244.167.46/24", Protocol: "TCP", Port: 80}}},
		{IPs: []IP{{CIDR: "12.244.167.46/32", Protocol: "ICMP", Port: 80}}},
		{Domains: []Domain{{Host: "*.example.com", Port: 443}}},
		{Domains: []Domain{{Host: "example.com/path", Port: 443}}},
		{Domains: []Domain{{Host: "127.0.0.1", Port: 443}}},
		{Domains: []Domain{{Host: "example.com", Port: 0}}},
		{Domains: []Domain{{Host: "example.com", Port: 443}, {Host: "example.com", Port: 443}}},
		{Services: []Service{{Name: "dns", Namespace: "kube-system", Port: 53, Protocol: "UDP"}}},
	}
	for i, c := range cases {
		if c.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}
func TestAcceptExplicitRules(t *testing.T) {
	c := Config{IPs: []IP{{CIDR: "12.244.167.46/32", Protocol: "TCP", Port: 80}}, Domains: []Domain{{Host: "gateway.example.com", Port: 443}}, Services: []Service{{Name: "dns", Namespace: "kube-system", PodSelector: map[string]string{"k8s-app": "kube-dns"}, Protocol: "UDP", Port: 53}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	first := c.Revision()
	c.Domains[0].Port = 8443
	if first == c.Revision() {
		t.Fatal("revision did not change")
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatal("empty deny-all must be valid", err)
	}
}
