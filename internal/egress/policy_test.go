package egress

import "testing"

func TestPolicyRejectsProtectedAndPreservesIngress(t *testing.T) {
	c := Config{IPs: []IP{{CIDR: "12.244.167.46/32", Protocol: "TCP", Port: 80}}}
	if _, err := c.WorkspacePolicy("work", "proxy", nil); err == nil {
		t.Fatal("missing protected inventory accepted")
	}
	if _, err := c.WorkspacePolicy("work", "proxy", []string{"12.244.167.0/24"}); err == nil {
		t.Fatal("protected target accepted")
	}
	p, err := c.WorkspacePolicy("work", "proxy", []string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Spec.Ingress) != 0 || len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != "Egress" {
		t.Fatal("ingress changed")
	}
	if len(p.Spec.Egress) != 2 {
		t.Fatal("unexpected allowances")
	}
	c.IPs[0].CIDR = "12.244.0.0/16"
	p, err = c.WorkspacePolicy("work", "proxy", []string{"12.244.167.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Spec.Egress[1].To[0].IPBlock.Except) != 1 {
		t.Fatal("protected subnet not excluded")
	}
}
