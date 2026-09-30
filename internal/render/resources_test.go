package render

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestRegionalClass(t *testing.T) {
	tests := []struct {
		name      string
		want      string
		wantError bool
	}{
		{name: "global-istio", want: "istio"},
		{name: "global-approuting-istio", want: "approuting-istio"},
		{name: "istio", wantError: true},
		{name: "global-", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := RegionalClass(test.name)
			if (err != nil) != test.wantError {
				t.Fatalf("RegionalClass() error = %v, wantError %v", err, test.wantError)
			}
			if got != test.want {
				t.Fatalf("RegionalClass() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestChildGatewayAndRouteCopies(t *testing.T) {
	uid := types.UID("7d1a8f9f")
	source := &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", UID: uid},
		Spec: gwv1.GatewaySpec{
			GatewayClassName: "global-istio",
			Listeners:        []gwv1.Listener{{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80}},
		},
	}
	child := ChildGateway(source, "istio", "regional-pip")
	if child.Name == source.Name || child.Spec.GatewayClassName != "istio" {
		t.Fatalf("unexpected child Gateway identity: %#v", child)
	}
	if got := child.Spec.Infrastructure.Annotations["service.beta.kubernetes.io/azure-pip-name"]; got != "regional-pip" {
		t.Fatalf("public IP annotation = %q", got)
	}
	if got := child.Spec.Infrastructure.Annotations["service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip"]; got != "true" {
		t.Fatalf("disable floating IP annotation = %q", got)
	}
	if source.Spec.GatewayClassName != "global-istio" {
		t.Fatal("ChildGateway mutated the source")
	}

	route := gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", UID: "route-uid"},
		Spec:       gwv1.HTTPRouteSpec{CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "web"}}}},
	}
	copies := AttachedRouteCopies(source, []gwv1.HTTPRoute{route}, child.Name)
	if len(copies) != 1 {
		t.Fatalf("got %d route copies, want 1", len(copies))
	}
	if copies[0].Spec.ParentRefs[0].Name != gwv1.ObjectName(child.Name) || copies[0].Name == route.Name {
		t.Fatalf("route copy was not retargeted: %#v", copies[0])
	}
}

func TestCompanionResourcePlacementIsDeterministic(t *testing.T) {
	placement := CompanionResourcePlacement("apps", "web", []string{"west", "east"}, "web-child", []string{"route-b", "route-a"}, []string{"backend-locality"})
	clusters := placement.Spec.Policy.ClusterNames
	if len(clusters) != 2 || clusters[0] != "east" || clusters[1] != "west" {
		t.Fatalf("clusterNames = %v", clusters)
	}
	selectors := placement.Spec.ResourceSelectors
	if len(selectors) != 4 || selectors[3].Group != "networking.istio.io" || selectors[3].Kind != "DestinationRule" || selectors[3].Name != "backend-locality" {
		t.Fatalf("DestinationRule selector = %#v", selectors)
	}
}

func TestLocalityDestinationRules(t *testing.T) {
	uid := types.UID("7d1a8f9f")
	source := &gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", UID: uid}}
	backendNamespace := gwv1.Namespace("backends")
	routes := []*gwv1.HTTPRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "apps"},
		Spec: gwv1.HTTPRouteSpec{Rules: []gwv1.HTTPRouteRule{{BackendRefs: []gwv1.HTTPBackendRef{
			{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{Name: "store", Namespace: &backendNamespace}}},
			{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{Name: "store", Namespace: &backendNamespace}}},
		}}}},
	}}

	rules := LocalityDestinationRules(source, routes)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want one per unique Service", len(rules))
	}
	rule := rules[0]
	if rule.Namespace != "apps" || rule.Spec.Host != "store.backends.svc.cluster.local" {
		t.Fatalf("unexpected DestinationRule identity: %#v", rule)
	}
	policy := rule.Spec.TrafficPolicy
	if policy == nil || policy.LoadBalancer == nil || policy.LoadBalancer.LocalityLbSetting == nil || !policy.LoadBalancer.LocalityLbSetting.Enabled.GetValue() {
		t.Fatalf("locality policy is not enabled: %#v", policy)
	}
	if policy.OutlierDetection == nil || policy.OutlierDetection.Consecutive_5XxErrors.GetValue() != 5 || policy.OutlierDetection.MaxEjectionPercent != 100 {
		t.Fatalf("outlier detection = %#v", policy.OutlierDetection)
	}
	if got := policy.OutlierDetection.Interval.AsDuration(); got != 2*time.Second {
		t.Fatalf("outlier interval = %s", got)
	}
}
