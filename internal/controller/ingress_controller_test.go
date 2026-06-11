package controller

import (
	"testing"

	netv1 "k8s.io/api/networking/v1"
)

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
