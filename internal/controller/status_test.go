package controller

import (
	"context"
	"testing"

	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestSyncIngressStatus exercises the full status writeback path against the
// fake client's server-side-apply implementation.
func TestSyncIngressStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))

	addrType := gatewayv1.IPAddressType
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-gateway", Namespace: "infra"},
		Status: gatewayv1.GatewayStatus{
			Addresses: []gatewayv1.GatewayStatusAddress{
				{Type: &addrType, Value: "192.0.2.10"},
			},
		},
	}
	ing := newIngress()

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ing, gateway).
		WithStatusSubresource(&netv1.Ingress{}).
		Build()

	r := &IngressReconciler{
		Client:              c,
		GatewayName:         "shared-gateway",
		GatewayNamespace:    "infra",
		UpdateIngressStatus: true,
	}

	if err := r.syncIngressStatus(context.Background(), ing); err != nil {
		t.Fatalf("syncIngressStatus: %v", err)
	}

	got := &netv1.Ingress{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ing), got); err != nil {
		t.Fatal(err)
	}
	lb := got.Status.LoadBalancer.Ingress
	if len(lb) != 1 || lb[0].IP != "192.0.2.10" {
		t.Fatalf("ingress status.loadBalancer = %+v, want IP 192.0.2.10", lb)
	}
}
