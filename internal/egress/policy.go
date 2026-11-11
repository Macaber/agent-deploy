package egress

import (
	"fmt"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// WorkspacePolicy selects only migrated Pods and never changes ingress.
// protected is an inventory of forbidden infrastructure/Pod networks, not an
// authorization list. A deployment must provide and maintain that inventory.
func (c Config) WorkspacePolicy(namespace, proxyNamespace string, protected []string) (*networkingv1.NetworkPolicy, error) {
	if err := c.RequireAllowlist(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if len(protected) == 0 {
		return nil, fmt.Errorf("protected network inventory is required")
	}
	excluded := []netip.Prefix{}
	for _, raw := range protected {
		p, err := netip.ParsePrefix(raw)
		if err != nil || p != p.Masked() {
			return nil, fmt.Errorf("invalid protected network %q", raw)
		}
		excluded = append(excluded, p)
	}
	port := func(protocol string, n int32) []networkingv1.NetworkPolicyPort {
		p := corev1.Protocol(protocol)
		v := intstr.FromInt32(n)
		return []networkingv1.NetworkPolicyPort{{Protocol: &p, Port: &v}}
	}
	rules := []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": proxyNamespace}}, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "workspace-egress"}}}}, Ports: port("TCP", 3128)}}
	for _, r := range c.IPs {
		allowed, _ := netip.ParsePrefix(r.CIDR)
		block := &networkingv1.IPBlock{CIDR: r.CIDR}
		fullyExcluded := false
		for _, p := range excluded {
			if !allowed.Overlaps(p) {
				continue
			}
			if p.Bits() <= allowed.Bits() {
				fullyExcluded = true
				break
			}
			block.Except = append(block.Except, p.String())
		}
		if fullyExcluded {
			return nil, fmt.Errorf("direct rule %s targets protected addresses", r.CIDR)
		}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: block}}, Ports: port(r.Protocol, r.Port)})
	}
	for _, r := range c.Services {
		// Never turn a base-service selector into a Workspace-to-Workspace exception.
		selector := &metav1.LabelSelector{MatchLabels: r.PodSelector, MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"workspace"}}}}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": r.Namespace}}, PodSelector: selector}}, Ports: port(r.Protocol, r.Port)})
	}
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "workspace-egress-v1", Namespace: namespace, Annotations: map[string]string{"workspace.bocom.io/egress-revision": c.Revision()}}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{MigrationLabel: MigrationVersion}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, Egress: rules}}, nil
}
