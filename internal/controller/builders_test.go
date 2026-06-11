package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1ac "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"
)

var testOpts = buildOptions{
	gatewayName:          "shared-gateway",
	gatewayNamespace:     "infra",
	httpsPort:            443,
	httpPort:             80,
	defaultClusterIssuer: "letsencrypt-prod",
}

func newIngress() *netv1.Ingress {
	return &netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shop",
			Namespace: "team-a",
			UID:       "uid-1",
		},
	}
}

// staticResolver resolves named ports from a "service/portName" map and
// passes numeric ports through, mirroring the real resolver's contract.
func staticResolver(ports map[string]int32) portResolver {
	return func(_ context.Context, _ string, b *netv1.IngressServiceBackend) (gatewayv1.PortNumber, error) {
		if b.Port.Number != 0 {
			return gatewayv1.PortNumber(b.Port.Number), nil
		}
		if p, ok := ports[b.Name+"/"+b.Port.Name]; ok {
			return gatewayv1.PortNumber(p), nil
		}
		return 0, fmt.Errorf("no such port %q on service %q", b.Port.Name, b.Name)
	}
}

func TestListenerSetAnnotations(t *testing.T) {
	tests := []struct {
		name          string
		annotations   map[string]string
		defaultIssuer string
		want          map[string]string
	}{
		{
			name: "copies only cert-manager annotations",
			annotations: map[string]string{
				"cert-manager.io/cluster-issuer":             "my-issuer",
				"cert-manager.io/common-name":                "example.com",
				"nginx.ingress.kubernetes.io/rewrite-target": "/",
				"kubernetes.io/ingress.class":                "nginx",
				"example.com/unrelated":                      "x",
			},
			defaultIssuer: "letsencrypt-prod",
			want: map[string]string{
				"cert-manager.io/cluster-issuer": "my-issuer",
				"cert-manager.io/common-name":    "example.com",
			},
		},
		{
			name:          "tls-acme without issuer gets default cluster-issuer",
			annotations:   map[string]string{"kubernetes.io/tls-acme": "true"},
			defaultIssuer: "letsencrypt-prod",
			want:          map[string]string{"cert-manager.io/cluster-issuer": "letsencrypt-prod"},
		},
		{
			name: "tls-acme does not override explicit cluster-issuer",
			annotations: map[string]string{
				"kubernetes.io/tls-acme":         "true",
				"cert-manager.io/cluster-issuer": "my-issuer",
			},
			defaultIssuer: "letsencrypt-prod",
			want:          map[string]string{"cert-manager.io/cluster-issuer": "my-issuer"},
		},
		{
			name: "tls-acme does not add cluster-issuer next to namespaced issuer",
			annotations: map[string]string{
				"kubernetes.io/tls-acme": "true",
				"cert-manager.io/issuer": "team-issuer",
			},
			defaultIssuer: "letsencrypt-prod",
			want:          map[string]string{"cert-manager.io/issuer": "team-issuer"},
		},
		{
			name:          "tls-acme false adds nothing",
			annotations:   map[string]string{"kubernetes.io/tls-acme": "false"},
			defaultIssuer: "letsencrypt-prod",
			want:          map[string]string{},
		},
		{
			name:          "tls-acme without configured default adds nothing",
			annotations:   map[string]string{"kubernetes.io/tls-acme": "true"},
			defaultIssuer: "",
			want:          map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ing := newIngress()
			ing.Annotations = tt.annotations
			got := listenerSetAnnotations(ing, tt.defaultIssuer)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("annotation %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestBuildListenerSet(t *testing.T) {
	ing := newIngress()
	ing.Spec.TLS = []netv1.IngressTLS{
		{Hosts: []string{"shop.example.com", "api.example.com"}, SecretName: "shop-tls"},
		{Hosts: []string{"shop.example.com"}, SecretName: "dup-tls"}, // duplicate host: first wins
		{SecretName: "catch-all-tls"},                                // no hosts: catch-all listener
		{Hosts: []string{"ignored.example.com"}},                     // no secret: skipped
	}

	ls := buildListenerSet(ing, testOpts)
	if ls == nil {
		t.Fatal("expected a ListenerSet")
	}
	if *ls.Name != "shop" || *ls.Namespace != "team-a" {
		t.Errorf("unexpected name/namespace: %s/%s", *ls.Namespace, *ls.Name)
	}

	pr := ls.Spec.ParentRef
	if string(*pr.Name) != "shared-gateway" || string(*pr.Namespace) != "infra" || string(*pr.Kind) != "Gateway" {
		t.Errorf("unexpected parentRef: %+v", pr)
	}

	// Each host yields an HTTPS listener and a plain HTTP one (for ACME).
	listeners := ls.Spec.Listeners
	if len(listeners) != 6 {
		t.Fatalf("expected 6 listeners, got %d", len(listeners))
	}
	for i, wantHost := range []string{"shop.example.com", "api.example.com", ""} {
		https, http := listeners[2*i], listeners[2*i+1]
		for _, l := range []gatewayv1ac.ListenerEntryApplyConfiguration{https, http} {
			if wantHost == "" {
				if l.Hostname != nil {
					t.Errorf("listener %q: expected no hostname, got %q", *l.Name, *l.Hostname)
				}
			} else if l.Hostname == nil || string(*l.Hostname) != wantHost {
				t.Errorf("listener %q: hostname = %v, want %q", *l.Name, l.Hostname, wantHost)
			}
		}
		if *https.Port != 443 || *https.Protocol != gatewayv1.HTTPSProtocolType {
			t.Errorf("listener %q: port/protocol = %v/%v", *https.Name, *https.Port, *https.Protocol)
		}
		if *https.TLS.Mode != gatewayv1.TLSModeTerminate {
			t.Errorf("listener %q: TLS mode = %v", *https.Name, *https.TLS.Mode)
		}
		if *http.Port != 80 || *http.Protocol != gatewayv1.HTTPProtocolType {
			t.Errorf("listener %q: port/protocol = %v/%v", *http.Name, *http.Port, *http.Protocol)
		}
		if http.TLS != nil {
			t.Errorf("listener %q: HTTP listener must not carry TLS config", *http.Name)
		}
	}
	if got := string(*listeners[0].TLS.CertificateRefs[0].Name); got != "shop-tls" {
		t.Errorf("listener 0 secret = %q, want shop-tls", got)
	}
	if got := string(*listeners[4].TLS.CertificateRefs[0].Name); got != "catch-all-tls" {
		t.Errorf("listener 4 secret = %q, want catch-all-tls", got)
	}

	owner := ls.OwnerReferences[0]
	if *owner.Kind != "Ingress" || *owner.Name != "shop" || !*owner.Controller {
		t.Errorf("unexpected owner reference: %+v", owner)
	}
	if ls.Labels[managedByLabelKey] != managedByLabelValue {
		t.Errorf("missing managed-by label: %v", ls.Labels)
	}
}

func TestBuildListenerSetNoTLS(t *testing.T) {
	if ls := buildListenerSet(newIngress(), testOpts); ls != nil {
		t.Fatalf("expected nil ListenerSet for Ingress without TLS, got %v", *ls.Name)
	}
}

func TestBuildHTTPRoutes(t *testing.T) {
	exact := netv1.PathTypeExact
	implSpecific := netv1.PathTypeImplementationSpecific

	ing := newIngress()
	ing.Spec.TLS = []netv1.IngressTLS{{Hosts: []string{"shop.example.com"}, SecretName: "shop-tls"}}
	ing.Spec.Rules = []netv1.IngressRule{
		{
			Host: "shop.example.com",
			IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{Paths: []netv1.HTTPIngressPath{
				{Path: "/", Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
					Name: "frontend", Port: netv1.ServiceBackendPort{Number: 8080}}}},
				{Path: "/api", PathType: &exact, Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
					Name: "api", Port: netv1.ServiceBackendPort{Name: "http"}}}},
			}}},
		},
		{
			Host: "admin.example.com",
			IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{Paths: []netv1.HTTPIngressPath{
				{PathType: &implSpecific, Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
					Name: "admin", Port: netv1.ServiceBackendPort{Number: 80}}}},
			}}},
		},
	}

	routes, err := buildHTTPRoutes(context.Background(), ing, testOpts, staticResolver(map[string]int32{"api/http": 9090}))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}

	shop := routes[0]
	if string(shop.Spec.Hostnames[0]) != "shop.example.com" {
		t.Errorf("hostnames = %v", shop.Spec.Hostnames)
	}
	if !strings.HasPrefix(*shop.Name, "shop-shop-example-com-") {
		t.Errorf("unexpected route name %q", *shop.Name)
	}
	rules := shop.Spec.Rules
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if m := rules[0].Matches[0].Path; *m.Type != gatewayv1.PathMatchPathPrefix || *m.Value != "/" {
		t.Errorf("rule 0 match = %v/%v, want PathPrefix /", *m.Type, *m.Value)
	}
	if m := rules[1].Matches[0].Path; *m.Type != gatewayv1.PathMatchExact || *m.Value != "/api" {
		t.Errorf("rule 1 match = %v/%v, want Exact /api", *m.Type, *m.Value)
	}
	if b := rules[1].BackendRefs[0]; string(*b.Name) != "api" || *b.Port != 9090 {
		t.Errorf("rule 1 backend = %v:%v, want api:9090 (named port resolved)", *b.Name, *b.Port)
	}
	// TLS host: attached to Gateway and ListenerSet.
	if len(shop.Spec.ParentRefs) != 2 {
		t.Fatalf("expected 2 parentRefs for TLS host, got %d", len(shop.Spec.ParentRefs))
	}
	if string(*shop.Spec.ParentRefs[1].Kind) != "ListenerSet" || string(*shop.Spec.ParentRefs[1].Name) != "shop" {
		t.Errorf("parentRef 1 = %+v, want ListenerSet shop", shop.Spec.ParentRefs[1])
	}

	admin := routes[1]
	// ImplementationSpecific defaults to Prefix, empty path to "/".
	if m := admin.Spec.Rules[0].Matches[0].Path; *m.Type != gatewayv1.PathMatchPathPrefix || *m.Value != "/" {
		t.Errorf("admin match = %v/%v, want PathPrefix /", *m.Type, *m.Value)
	}
	// Non-TLS host: only the Gateway parentRef.
	if len(admin.Spec.ParentRefs) != 1 {
		t.Fatalf("expected 1 parentRef for non-TLS host, got %d", len(admin.Spec.ParentRefs))
	}
	gw := admin.Spec.ParentRefs[0]
	if string(*gw.Kind) != "Gateway" || string(*gw.Name) != "shared-gateway" || string(*gw.Namespace) != "infra" {
		t.Errorf("gateway parentRef = %+v", gw)
	}
}

func TestBuildHTTPRoutesSkipsResourceBackendsAndNonHTTPRules(t *testing.T) {
	ing := newIngress()
	ing.Spec.Rules = []netv1.IngressRule{
		{Host: "no-http.example.com"},
		{
			Host: "resource.example.com",
			IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{Paths: []netv1.HTTPIngressPath{
				{Path: "/", Backend: netv1.IngressBackend{Resource: &corev1.TypedLocalObjectReference{
					Kind: "StorageBucket", Name: "assets"}}},
			}}},
		},
	}
	routes, err := buildHTTPRoutes(context.Background(), ing, testOpts, staticResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("expected no routes, got %d", len(routes))
	}
}

func TestBuildHTTPRoutesUnresolvablePortFails(t *testing.T) {
	ing := newIngress()
	ing.Spec.Rules = []netv1.IngressRule{{
		Host: "shop.example.com",
		IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{Paths: []netv1.HTTPIngressPath{
			{Path: "/", Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
				Name: "missing", Port: netv1.ServiceBackendPort{Name: "http"}}}},
		}}},
	}}
	if _, err := buildHTTPRoutes(context.Background(), ing, testOpts, staticResolver(nil)); err == nil {
		t.Fatal("expected error for unresolvable named port")
	}
}

func TestTLSCoversHost(t *testing.T) {
	ing := newIngress()
	ing.Spec.TLS = []netv1.IngressTLS{
		{Hosts: []string{"exact.example.com", "*.wild.example.com"}, SecretName: "tls"},
	}
	tests := []struct {
		host string
		want bool
	}{
		{"exact.example.com", true},
		{"other.example.com", false},
		{"foo.wild.example.com", true},
		{"a.b.wild.example.com", false}, // wildcard covers a single label only
		{"wild.example.com", false},
		{"*.wild.example.com", true},
	}
	for _, tt := range tests {
		if got := tlsCoversHost(ing, tt.host); got != tt.want {
			t.Errorf("tlsCoversHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestRouteNameStableAndBounded(t *testing.T) {
	longIngress := strings.Repeat("a", 250)
	name := routeName(longIngress, "very-long-host-name.subdomain.example.com")
	if len(name) > 253 {
		t.Errorf("route name exceeds 253 chars: %d", len(name))
	}
	if name != routeName(longIngress, "very-long-host-name.subdomain.example.com") {
		t.Error("route name is not stable")
	}
	if routeName("ing", "a.example.com") == routeName("ing", "b.example.com") {
		t.Error("different hosts must yield different route names")
	}
}
