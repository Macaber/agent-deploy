package egress

import "testing"

func TestMigrationRequiresExplicitDNSPair(t *testing.T) {
	c := Config{}
	if c.MigrationCheck() == nil {
		t.Fatal("empty policy migrates without DNS")
	}
	c.IPs = []IP{{CIDR: "169.254.20.10/32", Protocol: "UDP", Port: 53}}
	if c.MigrationCheck() == nil {
		t.Fatal("missing TCP DNS accepted")
	}
	c.IPs = append(c.IPs, IP{CIDR: "169.254.20.10/32", Protocol: "TCP", Port: 53})
	if err := c.MigrationCheck(); err != nil {
		t.Fatal(err)
	}
	c.IPs[1].CIDR = "169.254.20.11/32"
	if c.MigrationCheck() == nil {
		t.Fatal("different DNS destinations accepted")
	}
}
