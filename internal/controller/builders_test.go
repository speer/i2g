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
	gatewayName:      "shared-gateway",
	gatewayNamespace: "infra",
	httpsPort:        443,
	httpPort:         80,
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
		name        string
		annotations map[string]string
		want        map[string]string
	}{
		{
			name: "maps prefixed annotations, ignores direct cert-manager and others",
			annotations: map[string]string{
				"cert-manager.i2g.dev/cluster-issuer":        "my-issuer",
				"cert-manager.i2g.dev/common-name":           "example.com",
				"cert-manager.io/cluster-issuer":             "ingress-shim-owned", // not copied: would double-trigger cert-manager
				"nginx.ingress.kubernetes.io/rewrite-target": "/",
				"kubernetes.io/ingress.class":                "nginx",
				"example.com/unrelated":                      "x",
			},
			want: map[string]string{
				"cert-manager.io/cluster-issuer": "my-issuer",
				"cert-manager.io/common-name":    "example.com",
			},
		},
		{
			name:        "legacy kubernetes.io/tls-acme produces nothing",
			annotations: map[string]string{"kubernetes.io/tls-acme": "true"},
			want:        map[string]string{},
		},
		{
			name:        "no annotations produce nothing",
			annotations: nil,
			want:        map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ing := newIngress()
			ing.Annotations = tt.annotations
			got := listenerSetAnnotations(ing)
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
	// App route + HTTPS redirect route for the TLS host, app route for the
	// plain host.
	if len(routes) != 3 {
		t.Fatalf("expected 3 routes, got %d", len(routes))
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
	// TLS host: attached to Gateway and, pinned to the HTTPS listener, the
	// ListenerSet (the HTTP listener is served by the redirect route).
	if len(shop.Spec.ParentRefs) != 2 {
		t.Fatalf("expected 2 parentRefs for TLS host, got %d", len(shop.Spec.ParentRefs))
	}
	lsRef := shop.Spec.ParentRefs[1]
	if string(*lsRef.Kind) != "ListenerSet" || string(*lsRef.Name) != "shop" {
		t.Errorf("parentRef 1 = %+v, want ListenerSet shop", lsRef)
	}
	if lsRef.SectionName == nil || *lsRef.SectionName != listenerName("https", "shop.example.com") {
		t.Errorf("app route sectionName = %v, want HTTPS listener", lsRef.SectionName)
	}

	redirect := routes[1]
	if *redirect.Name != redirectRouteName("shop", "shop.example.com") {
		t.Errorf("redirect route name = %q", *redirect.Name)
	}
	if string(redirect.Spec.Hostnames[0]) != "shop.example.com" {
		t.Errorf("redirect hostnames = %v", redirect.Spec.Hostnames)
	}
	if len(redirect.Spec.ParentRefs) != 1 {
		t.Fatalf("redirect route must only attach to the HTTP listener, got %d parentRefs", len(redirect.Spec.ParentRefs))
	}
	rRef := redirect.Spec.ParentRefs[0]
	if string(*rRef.Kind) != "ListenerSet" || rRef.SectionName == nil || *rRef.SectionName != listenerName("http", "shop.example.com") {
		t.Errorf("redirect parentRef = %+v, want ListenerSet HTTP listener", rRef)
	}
	filter := redirect.Spec.Rules[0].Filters[0]
	if *filter.Type != gatewayv1.HTTPRouteFilterRequestRedirect ||
		*filter.RequestRedirect.Scheme != "https" || *filter.RequestRedirect.StatusCode != 308 {
		t.Errorf("unexpected redirect filter: %+v", filter)
	}
	if filter.RequestRedirect.Port != nil {
		t.Errorf("redirect port must be omitted for 443, got %v", *filter.RequestRedirect.Port)
	}

	admin := routes[2]
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

func TestBuildHTTPRoutesSSLRedirectDisabled(t *testing.T) {
	ing := newIngress()
	ing.Annotations = map[string]string{"nginx.ingress.kubernetes.io/ssl-redirect": "false"}
	ing.Spec.TLS = []netv1.IngressTLS{{Hosts: []string{"shop.example.com"}, SecretName: "shop-tls"}}
	ing.Spec.Rules = []netv1.IngressRule{{
		Host: "shop.example.com",
		IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{Paths: []netv1.HTTPIngressPath{
			{Path: "/", Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
				Name: "frontend", Port: netv1.ServiceBackendPort{Number: 8080}}}},
		}}},
	}}

	routes, err := buildHTTPRoutes(context.Background(), ing, testOpts, staticResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 route without redirect, got %d", len(routes))
	}
	// Without a redirect the app route attaches to the whole ListenerSet,
	// serving both HTTP and HTTPS.
	if ref := routes[0].Spec.ParentRefs[1]; ref.SectionName != nil {
		t.Errorf("expected no sectionName, got %v", *ref.SectionName)
	}
}

func TestBuildHTTPRoutesSSLRedirectWildcardAndCustomPort(t *testing.T) {
	opts := testOpts
	opts.httpsPort = 8443

	ing := newIngress()
	ing.Spec.TLS = []netv1.IngressTLS{{Hosts: []string{"*.example.com"}, SecretName: "wild-tls"}}
	ing.Spec.Rules = []netv1.IngressRule{{
		Host: "shop.example.com",
		IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{Paths: []netv1.HTTPIngressPath{
			{Path: "/", Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
				Name: "frontend", Port: netv1.ServiceBackendPort{Number: 8080}}}},
		}}},
	}}

	routes, err := buildHTTPRoutes(context.Background(), ing, opts, staticResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("expected app + redirect route, got %d", len(routes))
	}
	// The section names must reference the wildcard TLS host's listeners,
	// not the rule host's.
	if ref := routes[0].Spec.ParentRefs[1]; *ref.SectionName != listenerName("https", "*.example.com") {
		t.Errorf("app sectionName = %v, want wildcard HTTPS listener", *ref.SectionName)
	}
	if ref := routes[1].Spec.ParentRefs[0]; *ref.SectionName != listenerName("http", "*.example.com") {
		t.Errorf("redirect sectionName = %v, want wildcard HTTP listener", *ref.SectionName)
	}
	// Non-default HTTPS port must be part of the redirect.
	if port := routes[1].Spec.Rules[0].Filters[0].RequestRedirect.Port; port == nil || *port != 8443 {
		t.Errorf("redirect port = %v, want 8443", port)
	}
}

func TestBuildHTTPRoutesDefaultBackend(t *testing.T) {
	ing := newIngress()
	// Named port: resolved against the Service like any rule backend.
	ing.Spec.DefaultBackend = &netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
		Name: "fallback", Port: netv1.ServiceBackendPort{Name: "http"}}}

	routes, err := buildHTTPRoutes(context.Background(), ing, testOpts, staticResolver(map[string]int32{"fallback/http": 8080}))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	route := routes[0]
	if len(route.Spec.Hostnames) != 0 {
		t.Errorf("default backend route must not have hostnames, got %v", route.Spec.Hostnames)
	}
	if m := route.Spec.Rules[0].Matches[0].Path; *m.Type != gatewayv1.PathMatchPathPrefix || *m.Value != "/" {
		t.Errorf("match = %v/%v, want PathPrefix /", *m.Type, *m.Value)
	}
	if b := route.Spec.Rules[0].BackendRefs[0]; string(*b.Name) != "fallback" || *b.Port != 8080 {
		t.Errorf("backend = %v:%v, want fallback:8080", *b.Name, *b.Port)
	}
}

func TestBuildHTTPRoutesDefaultBackendMergedLast(t *testing.T) {
	// A host-less rule and the default backend share the catch-all HTTPRoute;
	// the default backend rule must come last so explicit paths win.
	ing := newIngress()
	ing.Spec.DefaultBackend = &netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
		Name: "fallback", Port: netv1.ServiceBackendPort{Number: 8080}}}
	ing.Spec.Rules = []netv1.IngressRule{{
		IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{Paths: []netv1.HTTPIngressPath{
			{Path: "/api", Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
				Name: "api", Port: netv1.ServiceBackendPort{Number: 9090}}}},
		}}},
	}}

	routes, err := buildHTTPRoutes(context.Background(), ing, testOpts, staticResolver(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 merged route, got %d", len(routes))
	}
	rules := routes[0].Spec.Rules
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if b := rules[0].BackendRefs[0]; string(*b.Name) != "api" {
		t.Errorf("rule 0 backend = %v, want api (explicit rule first)", *b.Name)
	}
	if b := rules[1].BackendRefs[0]; string(*b.Name) != "fallback" {
		t.Errorf("rule 1 backend = %v, want fallback (default backend last)", *b.Name)
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

func TestTranslationWarnings(t *testing.T) {
	prefixes := []string{"nginx.ingress.kubernetes.io/", "ingress.kubernetes.io/"}

	ing := newIngress()
	ing.Annotations = map[string]string{
		"nginx.ingress.kubernetes.io/rewrite-target": "/",     // warns: untranslated traffic semantics
		"nginx.ingress.kubernetes.io/ssl-redirect":   "false", // handled, no warning
		"meta.helm.sh/release-name":                  "shop",  // tooling, no warning
		"cert-manager.io/cluster-issuer":             "le",    // warns: ingress-shim trigger, rename hint
		"cert-manager.i2g.dev/common-name":           "x",     // mapped, no warning
		"kubernetes.io/tls-acme":                     "true",  // ignored entirely, no warning
		"argocd.argoproj.io/tracking-id":             "x",     // tooling, no warning
	}
	ing.Spec.DefaultBackend = &netv1.IngressBackend{Resource: &corev1.TypedLocalObjectReference{Kind: "StorageBucket", Name: "assets"}}
	ing.Spec.TLS = []netv1.IngressTLS{{Hosts: []string{"shop.example.com"}}} // no secretName
	ing.Spec.Rules = []netv1.IngressRule{
		{Host: "tcp.example.com"}, // no http section
		{Host: "shop.example.com", IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{Paths: []netv1.HTTPIngressPath{
			{Path: "/assets", Backend: netv1.IngressBackend{Resource: &corev1.TypedLocalObjectReference{Kind: "StorageBucket", Name: "assets"}}},
			{Path: "/", Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "ok", Port: netv1.ServiceBackendPort{Number: 80}}}},
		}}}},
	}

	got := translationWarnings(ing, prefixes)
	wantReasons := []string{
		"SkippedRule", "SkippedResourceBackend", "SkippedResourceBackend", "SkippedTLSEntry",
		"CertManagerAnnotationIgnored", // cert-manager.io/cluster-issuer
		"UnsupportedAnnotation",        // nginx rewrite-target
	}
	if len(got) != len(wantReasons) {
		t.Fatalf("expected %d warnings, got %d: %+v", len(wantReasons), len(got), got)
	}
	for i, reason := range wantReasons {
		if got[i].Reason != reason {
			t.Errorf("warning %d reason = %s, want %s (%s)", i, got[i].Reason, reason, got[i].Message)
		}
	}
	if !strings.Contains(got[4].Message, `"cert-manager.i2g.dev/cluster-issuer"`) {
		t.Errorf("cert-manager warning should hint at the rename: %s", got[4].Message)
	}
	if !strings.Contains(got[5].Message, "rewrite-target") {
		t.Errorf("annotation warning should name the annotation: %s", got[5].Message)
	}

	if ws := translationWarnings(newIngress(), prefixes); len(ws) != 0 {
		t.Errorf("clean Ingress must produce no warnings, got %+v", ws)
	}
}

func TestCoveringTLSHost(t *testing.T) {
	ing := newIngress()
	ing.Spec.TLS = []netv1.IngressTLS{
		{Hosts: []string{"exact.example.com", "*.wild.example.com"}, SecretName: "tls"},
	}
	tests := []struct {
		host        string
		wantTLSHost string
		wantCovered bool
	}{
		{"exact.example.com", "exact.example.com", true},
		{"other.example.com", "", false},
		{"foo.wild.example.com", "*.wild.example.com", true},
		{"a.b.wild.example.com", "", false}, // wildcard covers a single label only
		{"wild.example.com", "", false},
		{"*.wild.example.com", "*.wild.example.com", true},
	}
	for _, tt := range tests {
		tlsHost, covered := coveringTLSHost(ing, tt.host)
		if covered != tt.wantCovered || tlsHost != tt.wantTLSHost {
			t.Errorf("coveringTLSHost(%q) = (%q, %v), want (%q, %v)",
				tt.host, tlsHost, covered, tt.wantTLSHost, tt.wantCovered)
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
