package controller

import (
	"context"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	netv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	fieldManager = "ingress2gateway"

	// legacyIngressClassAnnotation predates spec.ingressClassName and is
	// still widely used.
	legacyIngressClassAnnotation = "kubernetes.io/ingress.class"

	// skipAnnotation excludes an individual Ingress from translation;
	// previously generated resources are cleaned up.
	skipAnnotation = "i2g.dev/skip"
)

// IngressReconciler translates Ingress resources of a given class into
// Gateway API ListenerSets and HTTPRoutes.
type IngressReconciler struct {
	client.Client
	Recorder record.EventRecorder

	IngressClass           string
	NamespaceSelector      labels.Selector
	GatewayName            string
	GatewayNamespace       string
	ListenerHTTPSPort      int32
	ListenerHTTPPort       int32
	UpdateIngressStatus    bool
	WarnAnnotationPrefixes []string
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses/status,verbs=patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=listenersets;httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ing := &netv1.Ingress{}
	if err := r.Get(ctx, req.NamespacedName, ing); err != nil {
		// On deletion the generated resources are cleaned up by garbage
		// collection via their owner references.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ing.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	matches, err := r.ingressMatches(ctx, ing)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !matches {
		// The Ingress changed class or its namespace no longer matches:
		// remove anything we created for it earlier, including our
		// ownership of status fields.
		if err := r.deleteGenerated(ctx, ing, nil); err != nil {
			return ctrl.Result{}, err
		}
		if r.UpdateIngressStatus {
			return ctrl.Result{}, r.applyIngressStatus(ctx, ing, nil)
		}
		return ctrl.Result{}, nil
	}

	// Surface dropped constructs as warning Events on the Ingress; the
	// recorder deduplicates repeats into a single event with a counter.
	if r.Recorder != nil {
		for _, warning := range translationWarnings(ing, r.WarnAnnotationPrefixes) {
			r.Recorder.Event(ing, corev1.EventTypeWarning, warning.Reason, warning.Message)
		}
	}

	opts := buildOptions{
		gatewayName:      r.GatewayName,
		gatewayNamespace: r.GatewayNamespace,
		httpsPort:        r.ListenerHTTPSPort,
		httpPort:         r.ListenerHTTPPort,
	}

	routes, err := buildHTTPRoutes(ctx, ing, opts, r.resolveBackendPort)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building HTTPRoutes: %w", err)
	}

	listenerSet := buildListenerSet(ing, opts)
	if listenerSet != nil {
		if err := r.Apply(ctx, listenerSet, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying ListenerSet: %w", err)
		}
	}

	desiredRoutes := map[string]bool{}
	for _, route := range routes {
		if err := r.Apply(ctx, route, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying HTTPRoute %s: %w", *route.Name, err)
		}
		desiredRoutes[*route.Name] = true
	}

	if err := r.deleteGenerated(ctx, ing, func(kind, name string) bool {
		if kind == "ListenerSet" {
			return listenerSet != nil
		}
		return desiredRoutes[name]
	}); err != nil {
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.reportGatewayStatus(ctx, ing, listenerSet != nil, desiredRoutes)
	}

	if r.UpdateIngressStatus {
		if err := r.syncIngressStatus(ctx, ing); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating Ingress status: %w", err)
		}
	}

	log.V(1).Info("reconciled", "httpRoutes", len(routes), "listenerSet", listenerSet != nil)
	return ctrl.Result{}, nil
}

// reportGatewayStatus reads back the status the Gateway implementation wrote
// on the generated resources and surfaces definitive failures as warning
// Events on the Ingress. A successful apply only proves the API server
// accepted the resources; whether they are actually served is reported
// asynchronously via status conditions. Status updates on owned resources
// re-trigger reconciliation, so this converges without extra watches.
func (r *IngressReconciler) reportGatewayStatus(ctx context.Context, ing *netv1.Ingress, expectListenerSet bool, desiredRoutes map[string]bool) {
	log := logf.FromContext(ctx)

	var listenerSet *gatewayv1.ListenerSet
	if expectListenerSet {
		listenerSet = &gatewayv1.ListenerSet{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ing.Namespace, Name: ing.Name}, listenerSet); err != nil {
			log.V(1).Info("cannot read back ListenerSet for status reporting", "error", err)
			listenerSet = nil
		}
	}

	var routes []*gatewayv1.HTTPRoute
	routeList := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, routeList,
		client.InNamespace(ing.Namespace),
		client.MatchingLabels(managedLabels())); err != nil {
		log.V(1).Info("cannot read back HTTPRoutes for status reporting", "error", err)
	} else {
		for i := range routeList.Items {
			route := &routeList.Items[i]
			if ownedBy(route, ing) && desiredRoutes[route.Name] {
				routes = append(routes, route)
			}
		}
	}

	for _, warning := range gatewayStatusWarnings(listenerSet, routes) {
		r.Recorder.Event(ing, corev1.EventTypeWarning, warning.Reason, warning.Message)
	}
}

// gatewayStatusWarnings converts definitive failure conditions on generated
// resources into warnings. Only well-known condition types are considered,
// and Unknown is always skipped — it merely means the Gateway implementation
// has not processed the resource yet.
func gatewayStatusWarnings(listenerSet *gatewayv1.ListenerSet, routes []*gatewayv1.HTTPRoute) []translationWarning {
	var warnings []translationWarning

	if listenerSet != nil {
		for _, c := range listenerSet.Status.Conditions {
			if (c.Type == "Accepted" || c.Type == "Programmed") && c.Status == metav1.ConditionFalse {
				warnings = append(warnings, translationWarning{
					Reason:  "ListenerSetRejected",
					Message: fmt.Sprintf("ListenerSet %q: %s=False (%s): %s", listenerSet.Name, c.Type, c.Reason, c.Message),
				})
			}
		}
		for _, listener := range listenerSet.Status.Listeners {
			for _, c := range listener.Conditions {
				failed := (c.Type == "Conflicted" && c.Status == metav1.ConditionTrue) ||
					((c.Type == "Accepted" || c.Type == "Programmed" || c.Type == "ResolvedRefs") && c.Status == metav1.ConditionFalse)
				if failed {
					warnings = append(warnings, translationWarning{
						Reason:  "ListenerRejected",
						Message: fmt.Sprintf("listener %q of ListenerSet %q: %s=%s (%s): %s", listener.Name, listenerSet.Name, c.Type, c.Status, c.Reason, c.Message),
					})
				}
			}
		}
	}

	for _, route := range routes {
		for _, parent := range route.Status.Parents {
			for _, c := range parent.Conditions {
				if (c.Type == "Accepted" || c.Type == "ResolvedRefs") && c.Status == metav1.ConditionFalse {
					warnings = append(warnings, translationWarning{
						Reason:  "HTTPRouteRejected",
						Message: fmt.Sprintf("HTTPRoute %q parent %s: %s=False (%s): %s", route.Name, parentRefDescription(parent.ParentRef), c.Type, c.Reason, c.Message),
					})
				}
			}
		}
	}
	return warnings
}

func parentRefDescription(ref gatewayv1.ParentReference) string {
	kind := "Gateway"
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}
	name := string(ref.Name)
	if ref.Namespace != nil {
		name = string(*ref.Namespace) + "/" + name
	}
	if ref.SectionName != nil {
		return fmt.Sprintf("%s %q (listener %q)", kind, name, string(*ref.SectionName))
	}
	return fmt.Sprintf("%s %q", kind, name)
}

// syncIngressStatus mirrors the Gateway's addresses into the Ingress
// status.loadBalancer so that consumers like external-dns and kubectl keep
// working after the original ingress controller is gone.
func (r *IngressReconciler) syncIngressStatus(ctx context.Context, ing *netv1.Ingress) error {
	gatewayNamespace := r.GatewayNamespace
	if gatewayNamespace == "" {
		gatewayNamespace = ing.Namespace
	}
	gateway := &gatewayv1.Gateway{}
	err := r.Get(ctx, types.NamespacedName{Namespace: gatewayNamespace, Name: r.GatewayName}, gateway)
	switch {
	case apierrors.IsNotFound(err):
		// No Gateway, no addresses; clear what we own. The Gateway watch
		// re-triggers once it appears.
		logf.FromContext(ctx).Info("gateway not found, clearing Ingress load balancer status",
			"gateway", gatewayNamespace+"/"+r.GatewayName)
		return r.applyIngressStatus(ctx, ing, nil)
	case err != nil:
		return err
	}
	return r.applyIngressStatus(ctx, ing, ingressLoadBalancerEntries(gateway.Status.Addresses))
}

// applyIngressStatus server-side applies the load balancer entries to the
// Ingress status. nil entries release the fields this controller owns
// without touching anything written by other managers.
func (r *IngressReconciler) applyIngressStatus(ctx context.Context, ing *netv1.Ingress, entries []*netv1ac.IngressLoadBalancerIngressApplyConfiguration) error {
	status := netv1ac.IngressStatus()
	if len(entries) > 0 {
		status = status.WithLoadBalancer(netv1ac.IngressLoadBalancerStatus().WithIngress(entries...))
	}
	return r.Status().Apply(ctx,
		netv1ac.Ingress(ing.Name, ing.Namespace).WithStatus(status),
		client.FieldOwner(fieldManager), client.ForceOwnership)
}

// ingressLoadBalancerEntries converts Gateway status addresses to Ingress
// load balancer entries. Address types other than IP and Hostname have no
// Ingress equivalent and are skipped.
func ingressLoadBalancerEntries(addresses []gatewayv1.GatewayStatusAddress) []*netv1ac.IngressLoadBalancerIngressApplyConfiguration {
	var entries []*netv1ac.IngressLoadBalancerIngressApplyConfiguration
	for _, addr := range addresses {
		addrType := gatewayv1.IPAddressType
		if addr.Type != nil {
			addrType = *addr.Type
		}
		switch addrType {
		case gatewayv1.IPAddressType:
			entries = append(entries, netv1ac.IngressLoadBalancerIngress().WithIP(addr.Value))
		case gatewayv1.HostnameAddressType:
			entries = append(entries, netv1ac.IngressLoadBalancerIngress().WithHostname(addr.Value))
		}
	}
	return entries
}

// deleteGenerated removes resources previously generated for the Ingress.
// keep, if non-nil, is consulted per resource; resources it returns true for
// survive. A nil keep deletes everything.
func (r *IngressReconciler) deleteGenerated(ctx context.Context, ing *netv1.Ingress, keep func(kind, name string) bool) error {
	log := logf.FromContext(ctx)

	if keep == nil || !keep("ListenerSet", ing.Name) {
		ls := &gatewayv1.ListenerSet{}
		err := r.Get(ctx, types.NamespacedName{Namespace: ing.Namespace, Name: ing.Name}, ls)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return err
		case ownedBy(ls, ing):
			log.Info("deleting ListenerSet", "name", ls.Name)
			if err := r.Delete(ctx, ls); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}

	routeList := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, routeList,
		client.InNamespace(ing.Namespace),
		client.MatchingLabels(managedLabels())); err != nil {
		return err
	}
	for i := range routeList.Items {
		route := &routeList.Items[i]
		if !ownedBy(route, ing) {
			continue
		}
		if keep != nil && keep("HTTPRoute", route.Name) {
			continue
		}
		log.Info("deleting HTTPRoute", "name", route.Name)
		if err := r.Delete(ctx, route); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func ownedBy(obj client.Object, ing *netv1.Ingress) bool {
	owner := metav1.GetControllerOf(obj)
	return owner != nil && owner.UID == ing.UID
}

// skipped reports whether the Ingress opted out of translation via the skip
// annotation.
func skipped(ing *netv1.Ingress) bool {
	return ing.Annotations[skipAnnotation] == "true"
}

// ingressMatches reports whether the Ingress belongs to the configured class,
// lives in a namespace matched by the namespace selector, and has not opted
// out via the skip annotation.
func (r *IngressReconciler) ingressMatches(ctx context.Context, ing *netv1.Ingress) (bool, error) {
	if skipped(ing) || !r.classMatches(ing) {
		return false, nil
	}
	if r.NamespaceSelector.Empty() {
		return true, nil
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: ing.Namespace}, ns); err != nil {
		return false, err
	}
	return r.NamespaceSelector.Matches(labels.Set(ns.Labels)), nil
}

func (r *IngressReconciler) classMatches(ing *netv1.Ingress) bool {
	// An empty IngressClass considers every Ingress regardless of its class.
	if r.IngressClass == "" {
		return true
	}
	if ing.Spec.IngressClassName != nil {
		return *ing.Spec.IngressClassName == r.IngressClass
	}
	return ing.Annotations[legacyIngressClassAnnotation] == r.IngressClass
}

// resolveBackendPort returns the numeric port of an Ingress service backend,
// resolving named ports against the Service spec.
func (r *IngressReconciler) resolveBackendPort(ctx context.Context, namespace string, backend *netv1.IngressServiceBackend) (gatewayv1.PortNumber, error) {
	if backend.Port.Number != 0 {
		return gatewayv1.PortNumber(backend.Port.Number), nil
	}
	if backend.Port.Name == "" {
		return 0, fmt.Errorf("backend service %q specifies neither port number nor port name", backend.Name)
	}
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: backend.Name}, svc); err != nil {
		return 0, fmt.Errorf("looking up service %q to resolve port name %q: %w", backend.Name, backend.Port.Name, err)
	}
	for _, port := range svc.Spec.Ports {
		if port.Name == backend.Port.Name {
			return gatewayv1.PortNumber(port.Port), nil
		}
	}
	return 0, fmt.Errorf("service %q has no port named %q", backend.Name, backend.Port.Name)
}

// ingressesForService enqueues the Ingresses in the Service's namespace that
// reference it, so HTTPRoutes are re-rendered when named ports change.
func (r *IngressReconciler) ingressesForService(ctx context.Context, obj client.Object) []reconcile.Request {
	ingList := &netv1.IngressList{}
	if err := r.List(ctx, ingList, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "listing Ingresses for Service", "service", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range ingList.Items {
		ing := &ingList.Items[i]
		if r.classMatches(ing) && !skipped(ing) && ingressReferencesService(ing, obj.GetName()) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ing)})
		}
	}
	return reqs
}

func ingressReferencesService(ing *netv1.Ingress, serviceName string) bool {
	if db := ing.Spec.DefaultBackend; db != nil && db.Service != nil && db.Service.Name == serviceName {
		return true
	}
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service != nil && path.Backend.Service.Name == serviceName {
				return true
			}
		}
	}
	return false
}

// ingressesForNamespace enqueues all matching-class Ingresses of a Namespace,
// so that namespace label changes (de)select them.
func (r *IngressReconciler) ingressesForNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	ingList := &netv1.IngressList{}
	if err := r.List(ctx, ingList, client.InNamespace(obj.GetName())); err != nil {
		logf.FromContext(ctx).Error(err, "listing Ingresses for Namespace", "namespace", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range ingList.Items {
		ing := &ingList.Items[i]
		if r.classMatches(ing) && !skipped(ing) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ing)})
		}
	}
	return reqs
}

// ingressesForGateway enqueues all matching Ingresses when the configured
// Gateway's addresses change, so their status stays in sync.
func (r *IngressReconciler) ingressesForGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != r.GatewayName {
		return nil
	}
	var listOpts []client.ListOption
	if r.GatewayNamespace != "" {
		if obj.GetNamespace() != r.GatewayNamespace {
			return nil
		}
	} else {
		// Without a fixed Gateway namespace the Gateway serves the
		// Ingresses of its own namespace.
		listOpts = append(listOpts, client.InNamespace(obj.GetNamespace()))
	}
	ingList := &netv1.IngressList{}
	if err := r.List(ctx, ingList, listOpts...); err != nil {
		logf.FromContext(ctx).Error(err, "listing Ingresses for Gateway", "gateway", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range ingList.Items {
		ing := &ingList.Items[i]
		if r.classMatches(ing) && !skipped(ing) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ing)})
		}
	}
	return reqs
}

// gatewayAddressesChanged filters Gateway events down to address changes.
func gatewayAddressesChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldGw, okOld := e.ObjectOld.(*gatewayv1.Gateway)
			newGw, okNew := e.ObjectNew.(*gatewayv1.Gateway)
			if !okOld || !okNew {
				return true
			}
			return !reflect.DeepEqual(oldGw.Status.Addresses, newGw.Status.Addresses)
		},
	}
}

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		Named("ingress2gateway").
		For(&netv1.Ingress{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Owns(&gatewayv1.ListenerSet{}).
		Owns(&gatewayv1.HTTPRoute{}).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.ingressesForService))

	if !r.NamespaceSelector.Empty() {
		b = b.Watches(&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.ingressesForNamespace),
			builder.WithPredicates(predicate.LabelChangedPredicate{}))
	}
	if r.UpdateIngressStatus {
		b = b.Watches(&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.ingressesForGateway),
			builder.WithPredicates(gatewayAddressesChanged()))
	}
	return b.Complete(r)
}
