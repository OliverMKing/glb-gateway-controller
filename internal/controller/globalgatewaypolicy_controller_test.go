package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/olivermking/glb-gateway-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestPublishGatewayStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gwv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	gateway := &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "global", Namespace: "app", Generation: 3},
		Spec:       gwv1.GatewaySpec{GatewayClassName: "global-istio"},
	}
	reconciler := &GlobalGatewayPolicyReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&gwv1.Gateway{}).
			WithObjects(gateway).
			Build(),
		Now: func() time.Time { return time.Unix(123, 0) },
	}

	if err := reconciler.publishGatewayStatus(context.Background(), gateway, "203.0.113.10"); err != nil {
		t.Fatal(err)
	}
	updated := &gwv1.Gateway{}
	if err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(gateway), updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Status.Addresses) != 1 || updated.Status.Addresses[0].Value != "203.0.113.10" {
		t.Fatalf("Gateway addresses = %#v", updated.Status.Addresses)
	}
	for _, conditionType := range []string{string(gwv1.GatewayConditionAccepted), string(gwv1.GatewayConditionProgrammed)} {
		condition := findCondition(updated.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != 3 {
			t.Fatalf("Gateway condition %s = %#v", conditionType, condition)
		}
	}

	if err := reconciler.clearGatewayStatus(context.Background(), gateway); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(gateway), updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Status.Addresses) != 0 {
		t.Fatalf("Gateway addresses after clear = %#v", updated.Status.Addresses)
	}
	for _, conditionType := range []string{string(gwv1.GatewayConditionAccepted), string(gwv1.GatewayConditionProgrammed)} {
		condition := findCondition(updated.Status.Conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionFalse {
			t.Fatalf("Gateway condition %s after clear = %#v", conditionType, condition)
		}
	}
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func TestPoliciesForGateway(t *testing.T) {
	reconciler := newWatchTestReconciler(t,
		policyForGateway("app", "matching", "web"),
		policyForGateway("app", "other", "other"),
		policyForGateway("elsewhere", "same-name", "web"),
	)

	requests := reconciler.policiesForGateway(context.Background(), &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app"},
	})
	want := types.NamespacedName{Namespace: "app", Name: "matching"}
	if len(requests) != 1 || requests[0].NamespacedName != want {
		t.Fatalf("policiesForGateway() = %#v, want only %s", requests, want)
	}
}

func TestPoliciesForHTTPRoute(t *testing.T) {
	reconciler := newWatchTestReconciler(t,
		policyForGateway("app", "web-policy", "web"),
		policyForGateway("app", "other-policy", "other"),
	)

	otherGroup := gwv1.Group("example.com")
	requests := reconciler.policiesForHTTPRoute(context.Background(), &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app"},
		Spec: gwv1.HTTPRouteSpec{CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{
			{Name: "web"},
			{Name: "web"}, // Duplicate parents still produce one reconcile request.
			{Name: "other", Group: &otherGroup},
		}}},
	})
	want := types.NamespacedName{Namespace: "app", Name: "web-policy"}
	if len(requests) != 1 || requests[0].NamespacedName != want {
		t.Fatalf("policiesForHTTPRoute() = %#v, want only %s", requests, want)
	}
}

func newWatchTestReconciler(t *testing.T, objects ...client.Object) *GlobalGatewayPolicyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithIndex(&api.GlobalGatewayPolicy{}, policyTargetGatewayField, indexPolicyByTargetGateway).
		Build()
	return &GlobalGatewayPolicyReconciler{Client: client}
}

func policyForGateway(namespace, name, gatewayName string) *api.GlobalGatewayPolicy {
	return &api.GlobalGatewayPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: api.GlobalGatewayPolicySpec{TargetRef: api.LocalObjectReference{
			Group: gwv1.GroupName,
			Kind:  "Gateway",
			Name:  gatewayName,
		}},
	}
}

func TestValidateGlobalBackendsRequiresIngressWaypointPath(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*corev1.Namespace, *corev1.Service, *gwv1.Gateway)
		wantError string
	}{
		{name: "valid"},
		{
			name: "missing ingress waypoint opt in",
			mutate: func(_ *corev1.Namespace, service *corev1.Service, _ *gwv1.Gateway) {
				delete(service.Labels, "istio.io/ingress-use-waypoint")
			},
			wantError: "ingress-use-waypoint=true",
		},
		{
			name: "waypoint service is not global",
			mutate: func(_ *corev1.Namespace, _ *corev1.Service, waypoint *gwv1.Gateway) {
				delete(waypoint.Spec.Infrastructure.Labels, gwv1.LabelKey("istio.io/global"))
			},
			wantError: "spec.infrastructure.labels[istio.io/global]=true",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			namespace, service, waypoint, route := validGlobalBackendObjects()
			if test.mutate != nil {
				test.mutate(namespace, service, waypoint)
			}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := gwv1.Install(scheme); err != nil {
				t.Fatal(err)
			}
			objects := []client.Object{namespace, service, waypoint}
			reconciler := &GlobalGatewayPolicyReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
			err := reconciler.validateGlobalBackends(context.Background(), []*gwv1.HTTPRoute{route})
			if test.wantError == "" && err != nil {
				t.Fatalf("validateGlobalBackends() error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("validateGlobalBackends() error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func validGlobalBackendObjects() (*corev1.Namespace, *corev1.Service, *gwv1.Gateway, *gwv1.HTTPRoute) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "app",
		Labels: map[string]string{
			"istio.io/dataplane-mode": "ambient",
			"istio.io/use-waypoint":   "waypoint",
		},
	}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      "backend",
		Namespace: "app",
		Labels: map[string]string{
			"istio.io/global":               "true",
			"istio.io/ingress-use-waypoint": "true",
		},
	}}
	waypoint := &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "waypoint", Namespace: "app"},
		Spec: gwv1.GatewaySpec{
			GatewayClassName: "istio-waypoint",
			Infrastructure: &gwv1.GatewayInfrastructure{Labels: map[gwv1.LabelKey]gwv1.LabelValue{
				"istio.io/global": "true",
			}},
		},
	}
	route := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "app"},
		Spec: gwv1.HTTPRouteSpec{Rules: []gwv1.HTTPRouteRule{{BackendRefs: []gwv1.HTTPBackendRef{{
			BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{Name: "backend"}},
		}}}}},
	}
	return namespace, service, waypoint, route
}
