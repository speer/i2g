package controller

import (
	"testing"

	netv1 "k8s.io/api/networking/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestIngressLoadBalancerEntries(t *testing.T) {
	addrType := func(t gatewayv1.AddressType) *gatewayv1.AddressType { return &t }
	entries := ingressLoadBalancerEntries([]gatewayv1.GatewayStatusAddress{
		{Type: addrType(gatewayv1.IPAddressType), Value: "192.0.2.10"},
		{Type: addrType(gatewayv1.HostnameAddressType), Value: "lb.example.com"},
		{Value: "192.0.2.11"}, // no type defaults to IP
		{Type: addrType(gatewayv1.NamedAddressType), Value: "named"}, // no Ingress equivalent
	})
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].IP == nil || *entries[0].IP != "192.0.2.10" {
		t.Errorf("entry 0 = %+v, want IP 192.0.2.10", entries[0])
	}
	if entries[1].Hostname == nil || *entries[1].Hostname != "lb.example.com" {
		t.Errorf("entry 1 = %+v, want hostname lb.example.com", entries[1])
	}
	if entries[2].IP == nil || *entries[2].IP != "192.0.2.11" {
		t.Errorf("entry 2 = %+v, want IP 192.0.2.11", entries[2])
	}
}

func TestIngressReferencesService(t *testing.T) {
	ing := newIngress()
	ing.Spec.DefaultBackend = &netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "fallback"}}
	ing.Spec.Rules = []netv1.IngressRule{{
		IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{Paths: []netv1.HTTPIngressPath{
			{Path: "/", Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "frontend"}}},
		}}},
	}}

	for name, want := range map[string]bool{"frontend": true, "fallback": true, "other": false} {
		if got := ingressReferencesService(ing, name); got != want {
			t.Errorf("ingressReferencesService(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestClassMatches(t *testing.T) {
	className := func(s string) *netv1.Ingress {
		ing := newIngress()
		ing.Spec.IngressClassName = &s
		return ing
	}
	annotated := func(s string) *netv1.Ingress {
		ing := newIngress()
		ing.Annotations = map[string]string{legacyIngressClassAnnotation: s}
		return ing
	}

	tests := []struct {
		name         string
		ingressClass string
		ing          *netv1.Ingress
		want         bool
	}{
		{"spec match", "nginx", className("nginx"), true},
		{"spec mismatch", "nginx", className("traefik"), false},
		{"annotation match", "nginx", annotated("nginx"), true},
		{"annotation mismatch", "nginx", annotated("traefik"), false},
		{"no class at all", "nginx", newIngress(), false},
		{"spec takes precedence over annotation", "nginx", func() *netv1.Ingress {
			ing := annotated("nginx")
			other := "traefik"
			ing.Spec.IngressClassName = &other
			return ing
		}(), false},
		{"empty class matches spec class", "", className("traefik"), true},
		{"empty class matches annotation class", "", annotated("traefik"), true},
		{"empty class matches classless ingress", "", newIngress(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &IngressReconciler{IngressClass: tt.ingressClass}
			if got := r.classMatches(tt.ing); got != tt.want {
				t.Errorf("classMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}
