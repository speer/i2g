package controller

import (
	"context"
	"fmt"
	"hash/fnv"
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
			WithParentRefs(routeParentRefs(ing, host, opts)...).
			WithRules(rules...)
		if host != "" {
			spec = spec.WithHostnames(gatewayv1.Hostname(host))
		}

		routes = append(routes, gatewayv1ac.HTTPRoute(routeName(ing.Name, host), ing.Namespace).
			WithLabels(managedLabels()).
			WithOwnerReferences(ownerReference(ing)).
			WithSpec(spec))
	}
	return routes, nil
}

// routeParentRefs returns the parents the HTTPRoute for the given host
// attaches to: always the configured Gateway, and additionally the generated
// ListenerSet when the Ingress terminates TLS for that host.
func routeParentRefs(ing *netv1.Ingress, host string, opts buildOptions) []*gatewayv1ac.ParentReferenceApplyConfiguration {
	gatewayRef := gatewayv1ac.ParentReference().
		WithGroup(gatewayGroup).
		WithKind("Gateway").
		WithName(gatewayv1.ObjectName(opts.gatewayName))
	if opts.gatewayNamespace != "" {
		gatewayRef = gatewayRef.WithNamespace(gatewayv1.Namespace(opts.gatewayNamespace))
	}
	refs := []*gatewayv1ac.ParentReferenceApplyConfiguration{gatewayRef}

	if tlsCoversHost(ing, host) {
		refs = append(refs, gatewayv1ac.ParentReference().
			WithGroup(gatewayGroup).
			WithKind("ListenerSet").
			WithName(gatewayv1.ObjectName(ing.Name)))
	}
	return refs
}

// tlsCoversHost reports whether any spec.tls entry of the Ingress covers the
// given host, taking wildcard TLS hosts into account. A TLS entry without
// hosts covers everything.
func tlsCoversHost(ing *netv1.Ingress, host string) bool {
	for _, tls := range ing.Spec.TLS {
		if tls.SecretName == "" {
			continue
		}
		if len(tls.Hosts) == 0 {
			return true
		}
		for _, tlsHost := range tls.Hosts {
			if tlsHost == host {
				return true
			}
			// "*.example.com" covers exactly one additional leftmost label.
			if suffix, ok := strings.CutPrefix(tlsHost, "*"); ok {
				if prefix, found := strings.CutSuffix(host, suffix); found &&
					prefix != "" && !strings.Contains(prefix, ".") {
					return true
				}
			}
		}
	}
	return false
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
