package render

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	placementv1 "go.goms.io/fleet/apis/placement/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	istioapi "istio.io/api/networking/v1alpha3"
	istiov1 "istio.io/client-go/pkg/apis/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	ManagedLabel     = "gateway.glb.azure.io/managed"
	SourceUIDLabel   = "gateway.glb.azure.io/source-uid"
	SourceAnnotation = "gateway.glb.azure.io/source"
)

func ChildName(name string, uid types.UID) string {
	// The UID suffix avoids collisions while preserving the DNS label limit.
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(uid)))[:5]
	maxBase := 63 - len(hash) - 1
	if len(name) > maxBase {
		name = strings.TrimRight(name[:maxBase], "-")
	}
	return name + "-" + hash
}

func RegionalPublicIPName(uid types.UID) string {
	return "glb-" + fmt.Sprintf("%x", sha256.Sum256([]byte(uid)))[:12]
}

func RegionalClass(globalClass string) (string, error) {
	if !strings.HasPrefix(globalClass, "global-") || len(globalClass) == len("global-") {
		return "", fmt.Errorf("GatewayClass %q must use global-<regional-class>", globalClass)
	}
	return strings.TrimPrefix(globalClass, "global-"), nil
}

func ChildGateway(source *gwv1.Gateway, regionalClass, publicIPName string) *gwv1.Gateway {
	// Preserve user listener intent, then replace global-only controller settings.
	child := &gwv1.Gateway{
		TypeMeta: metav1.TypeMeta{APIVersion: gwv1.GroupVersion.String(), Kind: "Gateway"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ChildName(source.Name, source.UID),
			Namespace: source.Namespace,
			Labels: map[string]string{
				ManagedLabel:   "true",
				SourceUIDLabel: string(source.UID),
			},
			Annotations: map[string]string{SourceAnnotation: source.Namespace + "/" + source.Name},
		},
		Spec: *source.Spec.DeepCopy(),
	}
	child.Spec.GatewayClassName = gwv1.ObjectName(regionalClass)
	child.Spec.Addresses = nil
	if child.Spec.Infrastructure == nil {
		child.Spec.Infrastructure = &gwv1.GatewayInfrastructure{}
	}
	if child.Spec.Infrastructure.Annotations == nil {
		child.Spec.Infrastructure.Annotations = map[gwv1.AnnotationKey]gwv1.AnnotationValue{}
	}
	// Bind a predictable regional IP and use non-floating AKS LB rules for GLB chaining.
	child.Spec.Infrastructure.Annotations[gwv1.AnnotationKey("service.beta.kubernetes.io/azure-pip-name")] = gwv1.AnnotationValue(publicIPName)
	child.Spec.Infrastructure.Annotations[gwv1.AnnotationKey("service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip")] = "true"
	return child
}

func AttachedRouteCopies(sourceGateway *gwv1.Gateway, routes []gwv1.HTTPRoute, childName string) []*gwv1.HTTPRoute {
	// Copy only routes attached to the source and retarget matching parent refs.
	result := make([]*gwv1.HTTPRoute, 0)
	for i := range routes {
		route := &routes[i]
		copy := route.DeepCopy()
		attached := false
		for j := range copy.Spec.ParentRefs {
			ref := &copy.Spec.ParentRefs[j]
			if parentMatches(*ref, sourceGateway) {
				ref.Name = gwv1.ObjectName(childName)
				ref.Namespace = nil
				attached = true
			}
		}
		if !attached {
			continue
		}
		// Strip server-owned identity and status before server-side apply.
		copy.TypeMeta = metav1.TypeMeta{APIVersion: gwv1.GroupVersion.String(), Kind: "HTTPRoute"}
		copy.Name = ChildName(route.Name, sourceGateway.UID)
		copy.ResourceVersion = ""
		copy.UID = ""
		copy.Generation = 0
		copy.ManagedFields = nil
		copy.Finalizers = nil
		copy.OwnerReferences = nil
		copy.Labels = map[string]string{ManagedLabel: "true", SourceUIDLabel: string(sourceGateway.UID)}
		copy.Annotations = map[string]string{SourceAnnotation: route.Namespace + "/" + route.Name}
		copy.Status = gwv1.HTTPRouteStatus{}
		result = append(result, copy)
	}
	return result
}

func LocalityDestinationRules(sourceGateway *gwv1.Gateway, routes []*gwv1.HTTPRoute) []*istiov1.DestinationRule {
	// Give each unique Service backend one local-first policy shared by all route copies.
	type backendKey struct {
		namespace string
		name      string
	}
	backends := map[backendKey]struct{}{}
	for _, route := range routes {
		for _, rule := range route.Spec.Rules {
			for _, backend := range rule.BackendRefs {
				if backend.Group != nil && string(*backend.Group) != "" {
					continue
				}
				if backend.Kind != nil && string(*backend.Kind) != "Service" {
					continue
				}
				namespace := route.Namespace
				if backend.Namespace != nil {
					namespace = string(*backend.Namespace)
				}
				backends[backendKey{namespace: namespace, name: string(backend.Name)}] = struct{}{}
			}
		}
	}

	keys := make([]backendKey, 0, len(backends))
	for key := range backends {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].namespace == keys[j].namespace {
			return keys[i].name < keys[j].name
		}
		return keys[i].namespace < keys[j].namespace
	})

	result := make([]*istiov1.DestinationRule, 0, len(keys))
	for _, key := range keys {
		backendHash := fmt.Sprintf("%x", sha256.Sum256([]byte(key.namespace+"/"+key.name)))[:5]
		result = append(result, &istiov1.DestinationRule{
			TypeMeta: metav1.TypeMeta{APIVersion: istiov1.SchemeGroupVersion.String(), Kind: "DestinationRule"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      ChildName(key.name+"-locality-"+backendHash, sourceGateway.UID),
				Namespace: sourceGateway.Namespace,
				Labels: map[string]string{
					ManagedLabel:   "true",
					SourceUIDLabel: string(sourceGateway.UID),
				},
				Annotations: map[string]string{SourceAnnotation: sourceGateway.Namespace + "/" + sourceGateway.Name},
			},
			Spec: istioapi.DestinationRule{
				Host: key.name + "." + key.namespace + ".svc.cluster.local",
				TrafficPolicy: &istioapi.TrafficPolicy{
					LoadBalancer: &istioapi.LoadBalancerSettings{LocalityLbSetting: &istioapi.LocalityLoadBalancerSetting{
						Enabled: wrapperspb.Bool(true),
					}},
					OutlierDetection: &istioapi.OutlierDetection{
						Consecutive_5XxErrors: wrapperspb.UInt32(5),
						Interval:              durationpb.New(2 * time.Second),
						BaseEjectionTime:      durationpb.New(30 * time.Second),
						MaxEjectionPercent:    100,
					},
				},
			},
		})
	}
	return result
}

func parentMatches(ref gwv1.ParentReference, gateway *gwv1.Gateway) bool {
	if ref.Name != gwv1.ObjectName(gateway.Name) {
		return false
	}
	if ref.Namespace != nil && string(*ref.Namespace) != gateway.Namespace {
		return false
	}
	if ref.Kind != nil && string(*ref.Kind) != "Gateway" {
		return false
	}
	if ref.Group != nil && string(*ref.Group) != gwv1.GroupName {
		return false
	}
	return true
}

func CompanionResourcePlacement(namespace, name string, memberNames []string, gatewayName string, routeNames, destinationRuleNames []string) *placementv1.ResourcePlacement {
	// Pin generated networking objects to the workload placement's exact members.
	sort.Strings(memberNames)
	selectors := []placementv1.ResourceSelectorTerm{
		{Group: gwv1.GroupName, Version: "v1", Kind: "Gateway", Name: gatewayName},
	}
	for _, routeName := range routeNames {
		selectors = append(selectors, placementv1.ResourceSelectorTerm{Group: gwv1.GroupName, Version: "v1", Kind: "HTTPRoute", Name: routeName})
	}
	for _, ruleName := range destinationRuleNames {
		selectors = append(selectors, placementv1.ResourceSelectorTerm{Group: istiov1.GroupName, Version: "v1", Kind: "DestinationRule", Name: ruleName})
	}
	return &placementv1.ResourcePlacement{
		TypeMeta: metav1.TypeMeta{APIVersion: placementv1.GroupVersion.String(), Kind: "ResourcePlacement"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Labels: map[string]string{ManagedLabel: "true"},
		},
		Spec: placementv1.PlacementSpec{
			ResourceSelectors: selectors,
			Policy: &placementv1.PlacementPolicy{
				PlacementType: placementv1.PickFixedPlacementType,
				ClusterNames:  memberNames,
			},
		},
	}
}

func ListenerPorts(gateway *gwv1.Gateway) []int32 {
	seen := map[int32]struct{}{}
	for _, listener := range gateway.Spec.Listeners {
		seen[int32(listener.Port)] = struct{}{}
	}
	ports := make([]int32, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports
}
