package controller

import (
	"context"
	"strings"
	"testing"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestGatewayStatusWarnings(t *testing.T) {
	kind := gatewayv1.Kind("ListenerSet")
	section := gatewayv1.SectionName("https-x")

	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: gatewayv1.ListenerSetStatus{
			Conditions: []metav1.Condition{
				{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "NotAllowed", Message: "listeners not allowed by gateway"},
				{Type: "Programmed", Status: metav1.ConditionUnknown, Reason: "Pending"}, // skipped: not processed yet
			},
			Listeners: []gatewayv1.ListenerEntryStatus{{
				Name: "https-x",
				Conditions: []metav1.Condition{
					{Type: "ResolvedRefs", Status: metav1.ConditionFalse, Reason: "InvalidCertificateRef", Message: "secret not found"},
					{Type: "Conflicted", Status: metav1.ConditionTrue, Reason: "HostnameConflict", Message: "conflicting hostname"},
					{Type: "Accepted", Status: metav1.ConditionTrue}, // healthy: no warning
				},
			}},
		},
	}
	gatewayKind := gatewayv1.Kind("Gateway")
	listenerSetRef := gatewayv1.ParentReference{Kind: &kind, Name: "shop", SectionName: &section}
	routes := []*gatewayv1.HTTPRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-shop-example-com-abc"},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{listenerSetRef},
		}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{
			{
				ParentRef: listenerSetRef,
				Conditions: []metav1.Condition{
					{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "NoMatchingParent", Message: "no listener https-x"},
					{Type: "ResolvedRefs", Status: metav1.ConditionTrue}, // healthy: no warning
				},
			},
			{
				// Stale entry for a parent no longer in spec.parentRefs
				// (e.g. after the route was re-applied without it): the
				// implementation prunes it asynchronously; never warn.
				ParentRef: gatewayv1.ParentReference{Kind: &gatewayKind, Name: "old-gateway"},
				Conditions: []metav1.Condition{
					{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "NotAllowedByListeners", Message: "stale"},
				},
			},
		}}},
	}}

	got := gatewayStatusWarnings(listenerSet, routes)
	wantReasons := []string{"ListenerSetRejected", "ListenerRejected", "ListenerRejected", "HTTPRouteRejected"}
	if len(got) != len(wantReasons) {
		t.Fatalf("expected %d warnings, got %d: %+v", len(wantReasons), len(got), got)
	}
	for i, reason := range wantReasons {
		if got[i].Reason != reason {
			t.Errorf("warning %d reason = %s, want %s (%s)", i, got[i].Reason, reason, got[i].Message)
		}
	}
	if !strings.Contains(got[0].Message, "listeners not allowed by gateway") {
		t.Errorf("condition message must be passed through: %s", got[0].Message)
	}
	if !strings.Contains(got[3].Message, `ListenerSet "shop" (listener "https-x")`) {
		t.Errorf("route warning must describe the parent: %s", got[3].Message)
	}

	if ws := gatewayStatusWarnings(nil, nil); len(ws) != 0 {
		t.Errorf("no resources must produce no warnings, got %+v", ws)
	}
}

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

func TestSkipAnnotation(t *testing.T) {
	r := &IngressReconciler{NamespaceSelector: labels.Everything()}

	for value, wantMatch := range map[string]bool{"true": false, "false": true, "": true} {
		ing := newIngress()
		if value != "" {
			ing.Annotations = map[string]string{skipAnnotation: value}
		}
		match, err := r.ingressMatches(context.Background(), ing)
		if err != nil {
			t.Fatal(err)
		}
		if match != wantMatch {
			t.Errorf("ingressMatches with skip=%q = %v, want %v", value, match, wantMatch)
		}
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
