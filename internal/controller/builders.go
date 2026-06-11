package controller

import (
	"context"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"
	"strings"

	netv1 "k8s.io/api/networking/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1ac "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"
)

const (
	managedByLabelKey   = "app.kubernetes.io/managed-by"
	managedByLabelValue = "ingress2gateway"

	certManagerAnnotationPrefix = "cert-manager.io/"
	clusterIssuerAnnotation     = "cert-manager.io/cluster-issuer"
	issuerAnnotation            = "cert-manager.io/issuer"
	tlsACMEAnnotation           = "kubernetes.io/tls-acme"

	sslRedirectAnnotation       = "nginx.ingress.kubernetes.io/ssl-redirect"
	sslRedirectAnnotationLegacy = "ingress.kubernetes.io/ssl-redirect"

	gatewayGroup = "gateway.networking.k8s.io"
)

// buildOptions carries the static (flag-derived) configuration needed to
// translate an Ingress into Gateway API resources.
type buildOptions struct {
	gatewayName          string
	gatewayNamespace     string
	httpsPort            int32
	httpPort             int32
	defaultClusterIssuer string
}

// portResolver resolves an Ingress service backend to a concrete port number,
// looking up the Service when the port is referenced by name.
type portResolver func(ctx context.Context, namespace string, backend *netv1.IngressServiceBackend) (gatewayv1.PortNumber, error)

func ownerReference(ing *netv1.Ingress) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion("networking.k8s.io/v1").
		WithKind("Ingress").
		WithName(ing.Name).
		WithUID(ing.UID).
		WithController(true).
		WithBlockOwnerDeletion(true)
}

func managedLabels() map[string]string {
	return map[string]string{managedByLabelKey: managedByLabelValue}
}

// listenerSetAnnotations returns the annotations to set on the generated
// ListenerSet: all cert-manager.io/* annotations of the Ingress, plus a
// default cluster-issuer when the Ingress opts into ACME via
// kubernetes.io/tls-acme without naming an issuer.
func listenerSetAnnotations(ing *netv1.Ingress, defaultClusterIssuer string) map[string]string {
	out := map[string]string{}
	for k, v := range ing.Annotations {
		if strings.HasPrefix(k, certManagerAnnotationPrefix) {
			out[k] = v
		}
	}
	_, hasClusterIssuer := out[clusterIssuerAnnotation]
	_, hasIssuer := out[issuerAnnotation]
	if !hasClusterIssuer && !hasIssuer &&
		ing.Annotations[tlsACMEAnnotation] == "true" && defaultClusterIssuer != "" {
		out[clusterIssuerAnnotation] = defaultClusterIssuer
	}
	return out
}

// buildListenerSet translates spec.tls of the Ingress into a ListenerSet with
// one HTTPS listener per TLS host, plus a plain HTTP listener per host so
// that e.g. ACME HTTP-01 challenges can be answered. It returns nil when the
// Ingress has no TLS configuration.
func buildListenerSet(ing *netv1.Ingress, opts buildOptions) *gatewayv1ac.ListenerSetApplyConfiguration {
	var listeners []*gatewayv1ac.ListenerEntryApplyConfiguration
	seen := map[gatewayv1.SectionName]bool{}

	addEntry := func(entry *gatewayv1ac.ListenerEntryApplyConfiguration, host string) {
		if seen[*entry.Name] {
			return
		}
		seen[*entry.Name] = true
		if host != "" {
			entry = entry.WithHostname(gatewayv1.Hostname(host))
		}
		listeners = append(listeners, entry)
	}

	addListeners := func(host, secretName string) {
		addEntry(gatewayv1ac.ListenerEntry().
			WithName(listenerName("https", host)).
			WithPort(gatewayv1.PortNumber(opts.httpsPort)).
			WithProtocol(gatewayv1.HTTPSProtocolType).
			WithTLS(gatewayv1ac.ListenerTLSConfig().
				WithMode(gatewayv1.TLSModeTerminate).
				WithCertificateRefs(gatewayv1ac.SecretObjectReference().
					WithName(gatewayv1.ObjectName(secretName)))), host)
		addEntry(gatewayv1ac.ListenerEntry().
			WithName(listenerName("http", host)).
			WithPort(gatewayv1.PortNumber(opts.httpPort)).
			WithProtocol(gatewayv1.HTTPProtocolType), host)
	}

	for _, tls := range ing.Spec.TLS {
		if tls.SecretName == "" {
			// Without a certificate Secret there is nothing a listener could
			// terminate TLS with.
			continue
		}
		if len(tls.Hosts) == 0 {
			addListeners("", tls.SecretName)
			continue
		}
		for _, host := range tls.Hosts {
			addListeners(host, tls.SecretName)
		}
	}

	if len(listeners) == 0 {
		return nil
	}

	parentRef := gatewayv1ac.ParentGatewayReference().
		WithGroup(gatewayGroup).
		WithKind("Gateway").
		WithName(gatewayv1.ObjectName(opts.gatewayName))
	if opts.gatewayNamespace != "" {
		parentRef = parentRef.WithNamespace(gatewayv1.Namespace(opts.gatewayNamespace))
	}

	return gatewayv1ac.ListenerSet(ing.Name, ing.Namespace).
		WithLabels(managedLabels()).
		WithAnnotations(listenerSetAnnotations(ing, opts.defaultClusterIssuer)).
		WithOwnerReferences(ownerReference(ing)).
		WithSpec(gatewayv1ac.ListenerSetSpec().
			WithParentRef(parentRef).
			WithListeners(listeners...))
}

// buildHTTPRoutes translates spec.rules and spec.defaultBackend of the
// Ingress into one HTTPRoute per distinct host. Rules without an HTTP section
// or paths without a Service backend are skipped.
func buildHTTPRoutes(ctx context.Context, ing *netv1.Ingress, opts buildOptions, resolvePort portResolver) ([]*gatewayv1ac.HTTPRouteApplyConfiguration, error) {
	// Group paths by host, preserving rule order. Ingress allows the same
	// host to appear in multiple rules.
	var hosts []string
	pathsByHost := map[string][]netv1.HTTPIngressPath{}
	addPaths := func(host string, paths ...netv1.HTTPIngressPath) {
		if _, ok := pathsByHost[host]; !ok {
			hosts = append(hosts, host)
		}
		pathsByHost[host] = append(pathsByHost[host], paths...)
	}
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		addPaths(rule.Host, rule.HTTP.Paths...)
	}

	// The default backend catches everything no rule matches. It becomes a
	// hostname-less "/" prefix rule, appended last: across HTTPRoutes,
	// Gateway API gives host-specific routes precedence over routes without
	// hostnames, and within a route earlier rules win on equal matches.
	if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
		addPaths("", netv1.HTTPIngressPath{Path: "/", Backend: *ing.Spec.DefaultBackend})
	}

	var routes []*gatewayv1ac.HTTPRouteApplyConfiguration
	for _, host := range hosts {
		tlsHost, tlsCovered := coveringTLSHost(ing, host)
		redirect := tlsCovered && sslRedirectEnabled(ing)

		var rules []*gatewayv1ac.HTTPRouteRuleApplyConfiguration
		for _, path := range pathsByHost[host] {
			if path.Backend.Service == nil {
				// Resource backends are out of scope.
				continue
			}
			port, err := resolvePort(ctx, ing.Namespace, path.Backend.Service)
			if err != nil {
				return nil, fmt.Errorf("host %q path %q: %w", host, path.Path, err)
			}

			matchType := gatewayv1.PathMatchPathPrefix
			if path.PathType != nil && *path.PathType == netv1.PathTypeExact {
				matchType = gatewayv1.PathMatchExact
			}
			pathValue := path.Path
			if pathValue == "" {
				pathValue = "/"
			}

			rules = append(rules, gatewayv1ac.HTTPRouteRule().
				WithMatches(gatewayv1ac.HTTPRouteMatch().
					WithPath(gatewayv1ac.HTTPPathMatch().
						WithType(matchType).
						WithValue(pathValue))).
				WithBackendRefs(gatewayv1ac.HTTPBackendRef().
					WithName(gatewayv1.ObjectName(path.Backend.Service.Name)).
					WithPort(port)))
		}
		if len(rules) == 0 {
			continue
		}

		spec := gatewayv1ac.HTTPRouteSpec().
			WithParentRefs(routeParentRefs(ing, tlsHost, tlsCovered, redirect, opts)...).
			WithRules(rules...)
		if host != "" {
			spec = spec.WithHostnames(gatewayv1.Hostname(host))
		}

		routes = append(routes, gatewayv1ac.HTTPRoute(routeName(ing.Name, host), ing.Namespace).
			WithLabels(managedLabels()).
			WithOwnerReferences(ownerReference(ing)).
			WithSpec(spec))

		if redirect {
			routes = append(routes, buildRedirectRoute(ing, host, tlsHost, opts))
		}
	}
	return routes, nil
}

// buildRedirectRoute returns an HTTPRoute that answers plain HTTP requests
// for a TLS-covered host with a permanent redirect to HTTPS. It attaches
// only to the host's HTTP listener in the ListenerSet; the app route is
// pinned to the HTTPS listener instead.
func buildRedirectRoute(ing *netv1.Ingress, host, tlsHost string, opts buildOptions) *gatewayv1ac.HTTPRouteApplyConfiguration {
	redirectFilter := gatewayv1ac.HTTPRequestRedirectFilter().
		WithScheme("https").
		WithStatusCode(308)
	if opts.httpsPort != 443 {
		redirectFilter = redirectFilter.WithPort(gatewayv1.PortNumber(opts.httpsPort))
	}

	spec := gatewayv1ac.HTTPRouteSpec().
		WithParentRefs(gatewayv1ac.ParentReference().
			WithGroup(gatewayGroup).
			WithKind("ListenerSet").
			WithName(gatewayv1.ObjectName(ing.Name)).
			WithSectionName(listenerName("http", tlsHost))).
		WithRules(gatewayv1ac.HTTPRouteRule().
			WithMatches(gatewayv1ac.HTTPRouteMatch().
				WithPath(gatewayv1ac.HTTPPathMatch().
					WithType(gatewayv1.PathMatchPathPrefix).
					WithValue("/"))).
			WithFilters(gatewayv1ac.HTTPRouteFilter().
				WithType(gatewayv1.HTTPRouteFilterRequestRedirect).
				WithRequestRedirect(redirectFilter)))
	if host != "" {
		spec = spec.WithHostnames(gatewayv1.Hostname(host))
	}

	return gatewayv1ac.HTTPRoute(redirectRouteName(ing.Name, host), ing.Namespace).
		WithLabels(managedLabels()).
		WithOwnerReferences(ownerReference(ing)).
		WithSpec(spec)
}

// routeParentRefs returns the parents the app HTTPRoute attaches to: always
// the configured Gateway, and additionally the generated ListenerSet when
// the Ingress terminates TLS for the host. With an HTTPS redirect in place
// the ListenerSet attachment is pinned to the HTTPS listener so that plain
// HTTP is left to the redirect route.
func routeParentRefs(ing *netv1.Ingress, tlsHost string, tlsCovered, redirect bool, opts buildOptions) []*gatewayv1ac.ParentReferenceApplyConfiguration {
	gatewayRef := gatewayv1ac.ParentReference().
		WithGroup(gatewayGroup).
		WithKind("Gateway").
		WithName(gatewayv1.ObjectName(opts.gatewayName))
	if opts.gatewayNamespace != "" {
		gatewayRef = gatewayRef.WithNamespace(gatewayv1.Namespace(opts.gatewayNamespace))
	}
	refs := []*gatewayv1ac.ParentReferenceApplyConfiguration{gatewayRef}

	if tlsCovered {
		listenerSetRef := gatewayv1ac.ParentReference().
			WithGroup(gatewayGroup).
			WithKind("ListenerSet").
			WithName(gatewayv1.ObjectName(ing.Name))
		if redirect {
			listenerSetRef = listenerSetRef.WithSectionName(listenerName("https", tlsHost))
		}
		refs = append(refs, listenerSetRef)
	}
	return refs
}

// coveringTLSHost returns the spec.tls host entry covering the given rule
// host, preferring exact matches over wildcards over catch-all entries. The
// returned TLS host names the listener the host's traffic terminates on
// ("" for an entry without hosts). ok is false when no TLS entry covers the
// host.
func coveringTLSHost(ing *netv1.Ingress, host string) (string, bool) {
	wildcard, catchAll := "", false
	for _, tls := range ing.Spec.TLS {
		if tls.SecretName == "" {
			continue
		}
		if len(tls.Hosts) == 0 {
			catchAll = true
			continue
		}
		for _, tlsHost := range tls.Hosts {
			if tlsHost == host {
				return tlsHost, true
			}
			// "*.example.com" covers exactly one additional leftmost label.
			if suffix, ok := strings.CutPrefix(tlsHost, "*"); ok && wildcard == "" {
				if prefix, found := strings.CutSuffix(host, suffix); found &&
					prefix != "" && !strings.Contains(prefix, ".") {
					wildcard = tlsHost
				}
			}
		}
	}
	if wildcard != "" {
		return wildcard, true
	}
	if catchAll {
		return "", true
	}
	return "", false
}

// tlsCoversHost reports whether any spec.tls entry of the Ingress covers the
// given host.
func tlsCoversHost(ing *netv1.Ingress, host string) bool {
	_, ok := coveringTLSHost(ing, host)
	return ok
}

// sslRedirectEnabled reports whether HTTP traffic for TLS-covered hosts
// should be redirected to HTTPS. Enabled by default (matching
// ingress-nginx), disabled via the ssl-redirect annotations.
func sslRedirectEnabled(ing *netv1.Ingress) bool {
	for _, key := range []string{sslRedirectAnnotation, sslRedirectAnnotationLegacy} {
		if v, ok := ing.Annotations[key]; ok {
			return v == "true"
		}
	}
	return true
}

// routeName derives a stable, unique HTTPRoute name from the Ingress name and
// the rule host. The hash suffix disambiguates hosts that sanitize to the
// same string and keeps names stable across reconciles.
func routeName(ingressName, host string) string {
	suffix := sanitizeHost(host) + "-" + hashString(host)
	maxPrefix := 253 - len(suffix) - 1
	if len(ingressName) > maxPrefix {
		ingressName = ingressName[:maxPrefix]
	}
	return ingressName + "-" + suffix
}

// redirectRouteName derives the name of the HTTP-to-HTTPS redirect route for
// a host, sharing the routeName scheme with a fixed suffix.
func redirectRouteName(ingressName, host string) string {
	const suffix = "-redirect"
	name := routeName(ingressName, host)
	if len(name) > 253-len(suffix) {
		name = name[:253-len(suffix)]
	}
	return name + suffix
}

// listenerName derives a stable listener name from the scheme and TLS host.
func listenerName(scheme, host string) gatewayv1.SectionName {
	return gatewayv1.SectionName(scheme + "-" + sanitizeHost(host) + "-" + hashString(host))
}

// sanitizeHost converts a hostname into a DNS-label-safe fragment, bounded to
// 30 characters.
func sanitizeHost(host string) string {
	if host == "" {
		return "all-hosts"
	}
	host = strings.ToLower(host)
	host = strings.ReplaceAll(host, "*", "wildcard")
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 30 {
		s = strings.Trim(s[:30], "-")
	}
	if s == "" {
		s = "host"
	}
	return s
}

func hashString(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// translationWarning describes a part of the Ingress the translation drops,
// surfaced as a warning Event on the Ingress.
type translationWarning struct {
	Reason  string
	Message string
}

// translationWarnings inspects the Ingress for constructs the translation
// skips: non-Service backends, TLS entries without a Secret, rules without
// an http section, and untranslated annotations of the configured
// ingress-controller prefixes. Annotations outside those prefixes (Helm,
// GitOps tooling, ...) never warn — they carry no traffic semantics.
func translationWarnings(ing *netv1.Ingress, warnAnnotationPrefixes []string) []translationWarning {
	var warnings []translationWarning

	for i, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			warnings = append(warnings, translationWarning{
				Reason:  "SkippedRule",
				Message: fmt.Sprintf("spec.rules[%d] (host %q) has no http section and was skipped", i, rule.Host),
			})
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service == nil {
				warnings = append(warnings, translationWarning{
					Reason:  "SkippedResourceBackend",
					Message: fmt.Sprintf("spec.rules[%d] (host %q) path %q uses a non-Service backend and was skipped", i, rule.Host, path.Path),
				})
			}
		}
	}
	if db := ing.Spec.DefaultBackend; db != nil && db.Service == nil {
		warnings = append(warnings, translationWarning{
			Reason:  "SkippedResourceBackend",
			Message: "spec.defaultBackend uses a non-Service backend and was skipped",
		})
	}
	for i, tls := range ing.Spec.TLS {
		if tls.SecretName == "" {
			warnings = append(warnings, translationWarning{
				Reason:  "SkippedTLSEntry",
				Message: fmt.Sprintf("spec.tls[%d] has no secretName and was skipped; configure a default certificate on the Gateway instead", i),
			})
		}
	}

	handled := map[string]bool{
		sslRedirectAnnotation:       true,
		sslRedirectAnnotationLegacy: true,
	}
	for _, key := range slices.Sorted(maps.Keys(ing.Annotations)) {
		if handled[key] {
			continue
		}
		for _, prefix := range warnAnnotationPrefixes {
			if prefix != "" && strings.HasPrefix(key, prefix) {
				warnings = append(warnings, translationWarning{
					Reason:  "UnsupportedAnnotation",
					Message: fmt.Sprintf("annotation %q is not translated; its behavior is dropped", key),
				})
				break
			}
		}
	}
	return warnings
}
