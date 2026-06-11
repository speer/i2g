package controller

import (
	"testing"

	netv1 "k8s.io/api/networking/v1"
)

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
