package fleet

import (
	"fmt"
	"sort"
	"strings"

	"github.com/olivermking/glb-gateway-controller/internal/azure"
	clusterv1 "go.goms.io/fleet/apis/cluster/v1"
	placementv1 "go.goms.io/fleet/apis/placement/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	AnnotationAKSResourceID     = "gateway.glb.azure.io/aks-resource-id"
	AnnotationNodeResourceGroup = "gateway.glb.azure.io/node-resource-group"
	AnnotationLocation          = "gateway.glb.azure.io/location"
)

type Selection struct {
	Members  []string
	Snapshot string
}

func SelectedMembers(obj placementv1.PlacementObj, previousMembers ...string) (Selection, error) {
	status := obj.GetPlacementStatus()
	if status == nil || len(status.PerClusterPlacementStatuses) == 0 {
		return Selection{}, fmt.Errorf("placement %s has no status.placementStatuses", obj.GetName())
	}

	// Keep serving from a prior member while Fleet rolls an update to that member.
	previous := make(map[string]struct{}, len(previousMembers))
	for _, name := range previousMembers {
		previous[name] = struct{}{}
	}
	members := make([]string, 0, len(status.PerClusterPlacementStatuses))
	for i := range status.PerClusterPlacementStatuses {
		clusterStatus := &status.PerClusterPlacementStatuses[i]
		if clusterStatus.ClusterName == "" {
			continue
		}
		applied, appliedReported := conditionState(clusterStatus.Conditions, string(placementv1.PerClusterAppliedConditionType))
		_, wasPreviouslySelected := previous[clusterStatus.ClusterName]
		rollingPriorMember := wasPreviouslySelected && !appliedReported &&
			conditionTrue(clusterStatus.Conditions, string(placementv1.PerClusterScheduledConditionType))
		if !applied && !rollingPriorMember {
			continue
		}
		members = append(members, clusterStatus.ClusterName)
	}
	if len(members) == 0 {
		return Selection{}, fmt.Errorf("placement %s has no eligible members", obj.GetName())
	}
	sort.Strings(members)
	return Selection{Members: members, Snapshot: status.ObservedResourceIndex}, nil
}

func conditionTrue(conditions []metav1.Condition, conditionType string) bool {
	status, found := conditionState(conditions, conditionType)
	return found && status
}

func conditionState(conditions []metav1.Condition, conditionType string) (bool, bool) {
	for i := range conditions {
		condition := &conditions[i]
		if condition.Type == conditionType {
			return condition.Status == metav1.ConditionTrue, true
		}
	}
	return false, false
}

func MemberFromObject(obj *clusterv1.MemberCluster) (azure.Member, error) {
	// Fleet stores identity; annotations add the ARM coordinates the controller needs.
	annotations := obj.GetAnnotations()
	aksID := annotations[AnnotationAKSResourceID]
	nodeRG := annotations[AnnotationNodeResourceGroup]
	location := annotations[AnnotationLocation]
	if aksID == "" || nodeRG == "" || location == "" {
		return azure.Member{}, fmt.Errorf("MemberCluster %s must have annotations %s, %s, and %s", obj.GetName(), AnnotationAKSResourceID, AnnotationNodeResourceGroup, AnnotationLocation)
	}
	// Parse the canonical AKS ID instead of duplicating subscription and RG metadata.
	parts := strings.Split(strings.Trim(aksID, "/"), "/")
	if len(parts) < 8 || !strings.EqualFold(parts[0], "subscriptions") || !strings.EqualFold(parts[2], "resourceGroups") || !strings.EqualFold(parts[4], "providers") || !strings.EqualFold(parts[5], "Microsoft.ContainerService") || !strings.EqualFold(parts[6], "managedClusters") {
		return azure.Member{}, fmt.Errorf("MemberCluster %s has invalid AKS resource ID %q", obj.GetName(), aksID)
	}
	return azure.Member{
		Name:              obj.GetName(),
		AKSResourceID:     aksID,
		SubscriptionID:    parts[1],
		ResourceGroup:     parts[3],
		NodeResourceGroup: nodeRG,
		Location:          location,
	}, nil
}
