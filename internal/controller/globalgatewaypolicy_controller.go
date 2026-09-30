package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	api "github.com/olivermking/glb-gateway-controller/api/v1alpha1"
	"github.com/olivermking/glb-gateway-controller/internal/azure"
	"github.com/olivermking/glb-gateway-controller/internal/fleet"
	"github.com/olivermking/glb-gateway-controller/internal/render"
	clusterv1 "go.goms.io/fleet/apis/cluster/v1"
	placementv1 "go.goms.io/fleet/apis/placement/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	fieldManager             = "glb-gateway-controller"
	defaultRequeue           = 20 * time.Second
	policyTargetGatewayField = "gateway.glb.azure.io/target-gateway"
)

// +kubebuilder:rbac:groups=gateway.glb.azure.io,resources=globalgatewaypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.glb.azure.io,resources=globalgatewaypolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.glb.azure.io,resources=globalgatewaypolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses;gateways;httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status;httproutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=placement.kubernetes-fleet.io,resources=clusterresourceplacements;resourceplacements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.kubernetes-fleet.io,resources=memberclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=create;get;update
// +kubebuilder:rbac:groups="",resources=namespaces;services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

type GlobalGatewayPolicyReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Azure          azure.Manager
	FrontendProber RegionalFrontendProber
	Now            func() time.Time
}

func (r *GlobalGatewayPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	// Load the policy and install defaults used by both production and tests.
	policy := &api.GlobalGatewayPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.FrontendProber == nil {
		r.FrontendProber = NewTCPRegionalFrontendProber()
	}

	// The finalizer owns Azure cleanup while Kubernetes garbage-collects hub objects.
	if !policy.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, policy)
	}
	if !controllerutil.ContainsFinalizer(policy, api.PolicyFinalizer) {
		controllerutil.AddFinalizer(policy, api.PolicyFinalizer)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Surface any validation or provider failure on the policy before returning it.
	defer func() {
		if reconcileErr != nil {
			r.setCondition(policy, "Accepted", metav1.ConditionFalse, "ReconcileFailed", reconcileErr.Error())
			policy.Status.ObservedGeneration = policy.Generation
			_ = r.updateStatus(ctx, policy)
		}
	}()

	// Resolve only members where Fleet applied the application successfully.
	gateway, regionalClass, err := r.resolveGateway(ctx, policy)
	if err != nil {
		return ctrl.Result{}, err
	}
	selection, placement, err := r.resolvePlacement(ctx, policy)
	if err != nil {
		return ctrl.Result{}, err
	}
	members, err := r.resolveMembers(ctx, selection.Members)
	if err != nil {
		return ctrl.Result{}, err
	}
	minimumReady := policy.Spec.MinReadyClusters
	if minimumReady < 1 {
		minimumReady = 1
	}
	if int32(len(members)) < minimumReady {
		return ctrl.Result{}, fmt.Errorf("Fleet placement selected %d members, fewer than spec.minReadyClusters %d", len(members), minimumReady)
	}

	// Render one regional Gateway shape and the attached routes placed with it.
	publicIPName := render.RegionalPublicIPName(gateway.UID)
	tags := ownershipTags(policy, gateway)
	ports := render.ListenerPorts(gateway)
	if len(ports) == 0 {
		return ctrl.Result{}, errors.New("source Gateway must have at least one listener")
	}
	child := render.ChildGateway(gateway, regionalClass, publicIPName)
	routes, err := r.attachedRoutes(ctx, gateway)
	if err != nil {
		return ctrl.Result{}, err
	}
	routeCopies := render.AttachedRouteCopies(gateway, routes, child.Name)
	if err := r.validateGlobalBackends(ctx, routeCopies); err != nil {
		return ctrl.Result{}, err
	}

	// Reserve stable regional addresses before the generated Gateways request them.
	publicIPs := make(map[string]azure.RegionalPublicIP, len(members))
	for _, member := range members {
		publicIP, err := r.Azure.EnsureRegionalPublicIP(ctx, member, publicIPName, tags)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure regional public IP for %s: %w", member.Name, err)
		}
		publicIPs[member.Name] = publicIP
	}

	// Keep generated intent on the hub, then let Fleet distribute it to members.
	if err := controllerutil.SetControllerReference(policy, child, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.apply(ctx, child); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply child Gateway: %w", err)
	}

	routeNames := make([]string, 0, len(routeCopies))
	for _, route := range routeCopies {
		if err := controllerutil.SetControllerReference(policy, route, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.apply(ctx, route); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply child HTTPRoute %s: %w", route.Name, err)
		}
		routeNames = append(routeNames, route.Name)
	}

	placementName := child.Name
	companion := render.CompanionResourcePlacement(policy.Namespace, placementName, append([]string(nil), selection.Members...), child.Name, routeNames)
	companion.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: api.GroupVersion.String(), Kind: "GlobalGatewayPolicy", Name: policy.Name, UID: policy.UID,
		Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true),
	}})
	if err := r.apply(ctx, companion); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply companion ResourcePlacement: %w", err)
	}

	memberStatuses := make([]api.MemberStatus, 0, len(members))
	frontendIDs := make([]string, 0, len(members))
	overrides := endpointOverrides(policy)
	var pending []string
	// Resolve and probe each regional frontend before placing it in the GLB pool.
	for _, member := range members {
		publicIP := publicIPs[member.Name]
		frontendID := overrides[member.Name]
		memberStatus := api.MemberStatus{
			Name:                       member.Name,
			ChildGatewayRef:            api.NamespacedObjectReference{Namespace: gateway.Namespace, Name: child.Name},
			RegionalPublicIPAddressID:  publicIP.ID,
			RegionalFrontendResourceID: frontendID,
		}
		if frontendID == "" {
			frontend, err := r.Azure.ResolveRegionalFrontend(ctx, member, publicIP.ID, ports)
			if err != nil {
				pending = append(pending, fmt.Sprintf("%s: %v", member.Name, err))
			} else {
				frontendID = frontend.ID
				memberStatus.RegionalFrontendResourceID = frontendID
			}
		}
		if frontendID != "" {
			if err := r.FrontendProber.Probe(ctx, publicIP.IPAddress, ports); err != nil {
				pending = append(pending, fmt.Sprintf("%s: regional frontend is not reachable: %v", member.Name, err))
				meta.SetStatusCondition(&memberStatus.Conditions, metav1.Condition{
					Type: "Reachable", Status: metav1.ConditionFalse, Reason: "ProbeFailed", Message: err.Error(),
					ObservedGeneration: policy.Generation, LastTransitionTime: metav1.NewTime(r.Now()),
				})
			} else {
				frontendIDs = append(frontendIDs, frontendID)
				meta.SetStatusCondition(&memberStatus.Conditions, metav1.Condition{
					Type: "Reachable", Status: metav1.ConditionTrue, Reason: "ProbeSucceeded", Message: "all listener ports accept TCP connections",
					ObservedGeneration: policy.Generation, LastTransitionTime: metav1.NewTime(r.Now()),
				})
			}
		}
		memberStatuses = append(memberStatuses, memberStatus)
	}
	if int32(len(frontendIDs)) < minimumReady {
		return ctrl.Result{RequeueAfter: defaultRequeue}, fmt.Errorf("only %d regional frontends are ready; need %d; pending: %s", len(frontendIDs), minimumReady, strings.Join(pending, "; "))
	}

	// Reconcile the GLB only after the minimum number of regional paths is live.
	sort.Strings(frontendIDs)
	globalSpec := globalLoadBalancerSpec(policy, gateway, ports, frontendIDs, tags)
	global, err := r.Azure.EnsureGlobalLoadBalancer(ctx, globalSpec)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure global load balancer: %w", err)
	}
	if err := r.publishGatewayStatus(ctx, gateway, global.IPAddress); err != nil {
		return ctrl.Result{}, fmt.Errorf("publish global Gateway status: %w", err)
	}

	// Publish the Azure result and a per-member view for operators.
	policy.Status.ObservedGeneration = policy.Generation
	policy.Status.GlobalAddress = global.IPAddress
	policy.Status.LoadBalancerResourceID = global.LoadBalancerID
	policy.Status.Fleet = api.FleetStatus{
		WorkloadPlacementRef:  placement.GetName(),
		ObservedSnapshot:      selection.Snapshot,
		CompanionPlacementRef: placementName,
	}
	policy.Status.Members = memberStatuses
	r.setCondition(policy, "Accepted", metav1.ConditionTrue, "Accepted", "Gateway, Fleet placement, and global Service configuration are valid")
	if len(frontendIDs) == len(members) {
		r.setCondition(policy, "MembersReady", metav1.ConditionTrue, "MembersReady", fmt.Sprintf("all %d members have regional frontends", len(memberStatuses)))
	} else {
		r.setCondition(policy, "MembersReady", metav1.ConditionFalse, "MembersPending", fmt.Sprintf("%d of %d members have regional frontends: %s", len(frontendIDs), len(members), strings.Join(pending, "; ")))
	}
	r.setCondition(policy, "RoutesPropagated", metav1.ConditionTrue, "PlacementCreated", fmt.Sprintf("companion ResourcePlacement %s is reconciled", placementName))
	r.setCondition(policy, "AzureResourcesReady", metav1.ConditionTrue, "AzureResourcesReady", "global load balancer is reconciled")
	if err := r.updateStatus(ctx, policy); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

func (r *GlobalGatewayPolicyReconciler) updateStatus(ctx context.Context, desired *api.GlobalGatewayPolicy) error {
	// Refetch on conflicts so status never overwrites a newer spec or metadata edit.
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &api.GlobalGatewayPolicy{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(desired), latest); err != nil {
			return err
		}
		latest.Status = desired.Status
		return r.Status().Update(ctx, latest)
	})
}

func (r *GlobalGatewayPolicyReconciler) publishGatewayStatus(ctx context.Context, desired *gwv1.Gateway, address string) error {
	// Expose the global frontend through standard Gateway API status.
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &gwv1.Gateway{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(desired), latest); err != nil {
			return err
		}
		addressType := gwv1.IPAddressType
		latest.Status.Addresses = []gwv1.GatewayStatusAddress{{Type: &addressType, Value: address}}
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               string(gwv1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			Reason:             string(gwv1.GatewayReasonAccepted),
			Message:            "global Gateway configuration is accepted",
			ObservedGeneration: latest.Generation,
			LastTransitionTime: metav1.NewTime(r.Now()),
		})
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               string(gwv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionTrue,
			Reason:             string(gwv1.GatewayReasonProgrammed),
			Message:            "Azure Global Load Balancer is programmed",
			ObservedGeneration: latest.Generation,
			LastTransitionTime: metav1.NewTime(r.Now()),
		})
		return r.Status().Update(ctx, latest)
	})
}

func (r *GlobalGatewayPolicyReconciler) clearGatewayStatus(ctx context.Context, desired *gwv1.Gateway) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &gwv1.Gateway{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(desired), latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		latest.Status.Addresses = nil
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               string(gwv1.GatewayConditionAccepted),
			Status:             metav1.ConditionFalse,
			Reason:             string(gwv1.GatewayReasonPending),
			Message:            "no active GlobalGatewayPolicy",
			ObservedGeneration: latest.Generation,
			LastTransitionTime: metav1.NewTime(r.Now()),
		})
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               string(gwv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionFalse,
			Reason:             string(gwv1.GatewayReasonNoResources),
			Message:            "global load balancer management was removed",
			ObservedGeneration: latest.Generation,
			LastTransitionTime: metav1.NewTime(r.Now()),
		})
		return r.Status().Update(ctx, latest)
	})
}

func (r *GlobalGatewayPolicyReconciler) resolveGateway(ctx context.Context, policy *api.GlobalGatewayPolicy) (*gwv1.Gateway, string, error) {
	if policy.Spec.TargetRef.Kind != "Gateway" || policy.Spec.TargetRef.Name == "" {
		return nil, "", errors.New("spec.targetRef must reference a Gateway")
	}
	gateway := &gwv1.Gateway{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: policy.Namespace, Name: policy.Spec.TargetRef.Name}, gateway); err != nil {
		return nil, "", fmt.Errorf("get source Gateway: %w", err)
	}
	regionalClass, err := render.RegionalClass(string(gateway.Spec.GatewayClassName))
	if err != nil {
		return nil, "", err
	}
	if regionalClass != "istio" {
		return nil, "", fmt.Errorf("regional GatewayClass %q is not supported by the MVP", regionalClass)
	}
	return gateway, regionalClass, nil
}

func (r *GlobalGatewayPolicyReconciler) resolvePlacement(ctx context.Context, policy *api.GlobalGatewayPolicy) (fleet.Selection, placementv1.PlacementObj, error) {
	ref := policy.Spec.Fleet.WorkloadPlacementRef
	if ref.Name == "" || (ref.Kind != "ClusterResourcePlacement" && ref.Kind != "ResourcePlacement") {
		return fleet.Selection{}, nil, errors.New("spec.fleet.workloadPlacementRef must reference a ClusterResourcePlacement or ResourcePlacement")
	}
	var obj placementv1.PlacementObj
	key := types.NamespacedName{Name: ref.Name}
	switch ref.Kind {
	case "ClusterResourcePlacement":
		obj = &placementv1.ClusterResourcePlacement{}
	case "ResourcePlacement":
		key.Namespace = policy.Namespace
		obj = &placementv1.ResourcePlacement{}
	}
	if err := r.Get(ctx, key, obj); err != nil {
		return fleet.Selection{}, nil, fmt.Errorf("get workload placement: %w", err)
	}
	selection, err := fleet.SelectedMembers(obj)
	return selection, obj, err
}

func (r *GlobalGatewayPolicyReconciler) resolveMembers(ctx context.Context, names []string) ([]azure.Member, error) {
	members := make([]azure.Member, 0, len(names))
	for _, name := range names {
		obj := &clusterv1.MemberCluster{}
		if err := r.Get(ctx, types.NamespacedName{Name: name}, obj); err != nil {
			return nil, fmt.Errorf("get MemberCluster %s: %w", name, err)
		}
		member, err := fleet.MemberFromObject(obj)
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, nil
}

func (r *GlobalGatewayPolicyReconciler) attachedRoutes(ctx context.Context, gateway *gwv1.Gateway) ([]gwv1.HTTPRoute, error) {
	list := &gwv1.HTTPRouteList{}
	if err := r.List(ctx, list, client.InNamespace(gateway.Namespace)); err != nil {
		return nil, fmt.Errorf("list HTTPRoutes: %w", err)
	}
	return list.Items, nil
}

func (r *GlobalGatewayPolicyReconciler) validateGlobalBackends(ctx context.Context, routes []*gwv1.HTTPRoute) error {
	// AppNet failover requires ambient global Services reached through global waypoints.
	services := 0
	namespaces := map[string]*corev1.Namespace{}
	validatedWaypoints := map[types.NamespacedName]struct{}{}
	for _, route := range routes {
		for _, rule := range route.Spec.Rules {
			for _, backend := range rule.BackendRefs {
				if backend.Group != nil && string(*backend.Group) != "" {
					return fmt.Errorf("HTTPRoute %s/%s backend %s uses unsupported group %q; Application Network failover validation requires Services", route.Namespace, route.Name, backend.Name, *backend.Group)
				}
				if backend.Kind != nil && string(*backend.Kind) != "Service" {
					return fmt.Errorf("HTTPRoute %s/%s backend %s uses unsupported kind %q; Application Network failover validation requires Services", route.Namespace, route.Name, backend.Name, *backend.Kind)
				}
				namespace := route.Namespace
				if backend.Namespace != nil {
					namespace = string(*backend.Namespace)
				}
				ns := namespaces[namespace]
				if ns == nil {
					ns = &corev1.Namespace{}
					if err := r.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
						return fmt.Errorf("get backend namespace %s: %w", namespace, err)
					}
					if ns.Labels["istio.io/dataplane-mode"] != "ambient" {
						return fmt.Errorf("backend namespace %s must have label istio.io/dataplane-mode=ambient", namespace)
					}
					namespaces[namespace] = ns
				}
				service := &corev1.Service{}
				key := types.NamespacedName{Namespace: namespace, Name: string(backend.Name)}
				if err := r.Get(ctx, key, service); err != nil {
					return fmt.Errorf("get HTTPRoute backend Service %s: %w", key, err)
				}
				if service.Spec.Type == corev1.ServiceTypeExternalName {
					return fmt.Errorf("HTTPRoute backend Service %s is ExternalName and cannot be an Application Network global Service", key)
				}
				if service.Labels["istio.io/global"] != "true" {
					return fmt.Errorf("HTTPRoute backend Service %s must have label istio.io/global=true", key)
				}
				if service.Labels["istio.io/ingress-use-waypoint"] != "true" && ns.Labels["istio.io/ingress-use-waypoint"] != "true" {
					return fmt.Errorf("HTTPRoute backend Service %s or namespace %s must have label istio.io/ingress-use-waypoint=true", key, namespace)
				}
				waypointName := service.Labels["istio.io/use-waypoint"]
				if waypointName == "" {
					waypointName = ns.Labels["istio.io/use-waypoint"]
				}
				if waypointName == "" {
					return fmt.Errorf("HTTPRoute backend Service %s or namespace %s must select an Application Network waypoint with istio.io/use-waypoint", key, namespace)
				}
				waypointKey := types.NamespacedName{Namespace: namespace, Name: waypointName}
				if _, found := validatedWaypoints[waypointKey]; !found {
					waypoint := &gwv1.Gateway{}
					if err := r.Get(ctx, waypointKey, waypoint); err != nil {
						return fmt.Errorf("get Application Network waypoint Gateway %s: %w", waypointKey, err)
					}
					if waypoint.Spec.GatewayClassName != "istio-waypoint" {
						return fmt.Errorf("Application Network waypoint Gateway %s must use GatewayClass istio-waypoint", waypointKey)
					}
					if waypoint.Spec.Infrastructure == nil || waypoint.Spec.Infrastructure.Labels[gwv1.LabelKey("istio.io/global")] != "true" {
						return fmt.Errorf("Application Network waypoint Gateway %s must set spec.infrastructure.labels[istio.io/global]=true so its generated Service is global", waypointKey)
					}
					validatedWaypoints[waypointKey] = struct{}{}
				}
				services++
			}
		}
	}
	if services == 0 {
		return errors.New("no attached HTTPRoute has a Service backend")
	}
	return nil
}

func (r *GlobalGatewayPolicyReconciler) apply(ctx context.Context, obj client.Object) error {
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership)
}

func (r *GlobalGatewayPolicyReconciler) finalize(ctx context.Context, policy *api.GlobalGatewayPolicy) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(policy, api.PolicyFinalizer) {
		return ctrl.Result{}, nil
	}
	gateway := &gwv1.Gateway{}
	err := r.Get(ctx, types.NamespacedName{Namespace: policy.Namespace, Name: policy.Spec.TargetRef.Name}, gateway)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	uid := gateway.UID
	if uid == "" {
		uid = types.UID(policy.Annotations["gateway.glb.azure.io/source-gateway-uid"])
	}
	ports := []int32{}
	if !apierrors.IsNotFound(err) {
		ports = render.ListenerPorts(gateway)
	}
	// Delete the GLB first so regional Public IPs are no longer referenced.
	if policy.Spec.Azure.DeletionPolicy != "Retain" {
		spec := globalLoadBalancerSpec(policy, gateway, ports, nil, nil)
		if err := r.Azure.DeleteGlobalLoadBalancer(ctx, spec); err != nil {
			return ctrl.Result{}, err
		}
		if uid != "" {
			members := make([]azure.Member, 0, len(policy.Status.Members))
			for _, status := range policy.Status.Members {
				obj := &clusterv1.MemberCluster{}
				if getErr := r.Get(ctx, types.NamespacedName{Name: status.Name}, obj); getErr == nil {
					if member, parseErr := fleet.MemberFromObject(obj); parseErr == nil {
						members = append(members, member)
					}
				}
			}
			for _, member := range members {
				if err := r.Azure.DeleteRegionalPublicIP(ctx, member, render.RegionalPublicIPName(uid)); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}
	if !apierrors.IsNotFound(err) {
		if err := r.clearGatewayStatus(ctx, gateway); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(policy, api.PolicyFinalizer)
	return ctrl.Result{}, r.Update(ctx, policy)
}

func (r *GlobalGatewayPolicyReconciler) setCondition(policy *api.GlobalGatewayPolicy, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type: conditionType, Status: status, Reason: reason, Message: message,
		ObservedGeneration: policy.Generation, LastTransitionTime: metav1.NewTime(r.Now()),
	})
}

func endpointOverrides(policy *api.GlobalGatewayPolicy) map[string]string {
	result := make(map[string]string, len(policy.Spec.RegionalEndpointOverrides))
	for _, override := range policy.Spec.RegionalEndpointOverrides {
		result[override.MemberName] = override.FrontendIPConfigurationID
	}
	return result
}

func ownershipTags(policy *api.GlobalGatewayPolicy, gateway *gwv1.Gateway) map[string]string {
	return map[string]string{
		"gateway.glb.azure.io-managed":     "true",
		"gateway.glb.azure.io-policy":      policy.Namespace + "/" + policy.Name,
		"gateway.glb.azure.io-gateway":     gateway.Namespace + "/" + gateway.Name,
		"gateway.glb.azure.io-gateway-uid": string(gateway.UID),
	}
}

func globalLoadBalancerSpec(policy *api.GlobalGatewayPolicy, gateway *gwv1.Gateway, ports []int32, frontendIDs []string, tags map[string]string) azure.GlobalLoadBalancerSpec {
	uid := gateway.UID
	base := render.ChildName(policy.Name, uid)
	lbName := policy.Spec.Azure.LoadBalancerName
	if lbName == "" {
		lbName = base
	}
	pipName := policy.Spec.Azure.PublicIPAddressName
	if pipName == "" {
		pipName = base
	}
	return azure.GlobalLoadBalancerSpec{
		SubscriptionID:      policy.Spec.Azure.SubscriptionID,
		ResourceGroup:       policy.Spec.Azure.ResourceGroup,
		Location:            policy.Spec.Azure.Location,
		LoadBalancerName:    lbName,
		PublicIPAddressName: pipName,
		Ports:               ports,
		BackendFrontendIDs:  frontendIDs,
		Tags:                tags,
	}
}

func boolPtr(value bool) *bool { return &value }

func indexPolicyByTargetGateway(obj client.Object) []string {
	policy, ok := obj.(*api.GlobalGatewayPolicy)
	if !ok || policy.Spec.TargetRef.Kind != "Gateway" || policy.Spec.TargetRef.Name == "" {
		return nil
	}
	return []string{policy.Spec.TargetRef.Name}
}

func (r *GlobalGatewayPolicyReconciler) policiesForGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.policiesForGatewayNames(ctx, obj.GetNamespace(), []string{obj.GetName()})
}

func (r *GlobalGatewayPolicyReconciler) policiesForHTTPRoute(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gwv1.HTTPRoute)
	if !ok {
		return nil
	}

	// A route can name multiple Gateway parents; enqueue each policy only once.
	gatewayNames := make([]string, 0, len(route.Spec.ParentRefs))
	seen := map[string]struct{}{}
	for _, parent := range route.Spec.ParentRefs {
		if parent.Group != nil && string(*parent.Group) != gwv1.GroupName {
			continue
		}
		if parent.Kind != nil && string(*parent.Kind) != "Gateway" {
			continue
		}
		if parent.Namespace != nil && string(*parent.Namespace) != route.Namespace {
			continue
		}
		name := string(parent.Name)
		if _, found := seen[name]; found {
			continue
		}
		seen[name] = struct{}{}
		gatewayNames = append(gatewayNames, name)
	}
	return r.policiesForGatewayNames(ctx, route.Namespace, gatewayNames)
}

func (r *GlobalGatewayPolicyReconciler) policiesForGatewayNames(ctx context.Context, namespace string, gatewayNames []string) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(gatewayNames))
	seen := map[types.NamespacedName]struct{}{}
	for _, gatewayName := range gatewayNames {
		policies := &api.GlobalGatewayPolicyList{}
		if err := r.List(ctx, policies,
			client.InNamespace(namespace),
			client.MatchingFields{policyTargetGatewayField: gatewayName},
		); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "list policies for Gateway", "namespace", namespace, "gateway", gatewayName)
			continue
		}
		for i := range policies.Items {
			key := client.ObjectKeyFromObject(&policies.Items[i])
			if _, found := seen[key]; found {
				continue
			}
			seen[key] = struct{}{}
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
	}
	return requests
}

func (r *GlobalGatewayPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index policy targets so secondary watches only fan out affected work.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&api.GlobalGatewayPolicy{},
		policyTargetGatewayField,
		indexPolicyByTargetGateway,
	); err != nil {
		return fmt.Errorf("index policies by target Gateway: %w", err)
	}

	// Ignore status-only updates while reacting immediately to source intent changes.
	policyChanges := predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
		oldPolicy, oldOK := update.ObjectOld.(*api.GlobalGatewayPolicy)
		newPolicy, newOK := update.ObjectNew.(*api.GlobalGatewayPolicy)
		if !oldOK || !newOK {
			return true
		}
		return oldPolicy.Generation != newPolicy.Generation ||
			!reflect.DeepEqual(oldPolicy.Annotations, newPolicy.Annotations) ||
			(oldPolicy.DeletionTimestamp.IsZero() != newPolicy.DeletionTimestamp.IsZero())
	}}
	sourceChanges := predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
		return update.ObjectOld.GetGeneration() != update.ObjectNew.GetGeneration() ||
			(update.ObjectOld.GetDeletionTimestamp() == nil) != (update.ObjectNew.GetDeletionTimestamp() == nil)
	}}
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.GlobalGatewayPolicy{}, builder.WithPredicates(policyChanges)).
		Watches(
			&gwv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.policiesForGateway),
			builder.WithPredicates(sourceChanges),
		).
		Watches(
			&gwv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.policiesForHTTPRoute),
			builder.WithPredicates(sourceChanges),
		).
		Named("globalgatewaypolicy").
		Complete(r)
}
