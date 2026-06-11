package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
)

// IngressReconciler translates Ingress resources of a given class into
// Gateway API ListenerSets and HTTPRoutes.
type IngressReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	IngressClass         string
	NamespaceSelector    labels.Selector
	GatewayName          string
	GatewayNamespace     string
	DefaultClusterIssuer string
	ListenerHTTPSPort    int32
	ListenerHTTPPort     int32
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=listenersets;httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;namespaces,verbs=get;list;watch

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
		// remove anything we created for it earlier.
		return ctrl.Result{}, r.deleteGenerated(ctx, ing, nil)
	}

	opts := buildOptions{
		gatewayName:          r.GatewayName,
		gatewayNamespace:     r.GatewayNamespace,
		httpsPort:            r.ListenerHTTPSPort,
		httpPort:             r.ListenerHTTPPort,
		defaultClusterIssuer: r.DefaultClusterIssuer,
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

	log.V(1).Info("reconciled", "httpRoutes", len(routes), "listenerSet", listenerSet != nil)
	return ctrl.Result{}, nil
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

// ingressMatches reports whether the Ingress belongs to the configured class
// and lives in a namespace matched by the namespace selector.
func (r *IngressReconciler) ingressMatches(ctx context.Context, ing *netv1.Ingress) (bool, error) {
	if !r.classMatches(ing) {
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
		if r.classMatches(ing) && ingressReferencesService(ing, obj.GetName()) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ing)})
		}
	}
	return reqs
}

func ingressReferencesService(ing *netv1.Ingress, serviceName string) bool {
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
		if r.classMatches(ing) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ing)})
		}
	}
	return reqs
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
	return b.Complete(r)
}
