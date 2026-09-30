package fleet

import (
	"testing"

	clusterv1 "go.goms.io/fleet/apis/cluster/v1"
	placementv1 "go.goms.io/fleet/apis/placement/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSelectedMembers(t *testing.T) {
	placement := &placementv1.ClusterResourcePlacement{
		ObjectMeta: metav1.ObjectMeta{Name: "workloads"},
		Status: placementv1.PlacementStatus{
			ObservedResourceIndex: "4",
			PerClusterPlacementStatuses: []placementv1.PerClusterPlacementStatus{
				{ClusterName: "west", Conditions: []metav1.Condition{{Type: string(placementv1.PerClusterAppliedConditionType), Status: metav1.ConditionFalse}}},
				{ClusterName: "east", Conditions: []metav1.Condition{{Type: string(placementv1.PerClusterAppliedConditionType), Status: metav1.ConditionTrue}}},
			},
		},
	}
	selection, err := SelectedMembers(placement)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Members) != 1 || selection.Members[0] != "east" || selection.Snapshot != "4" {
		t.Fatalf("selection = %#v", selection)
	}

	placement.Status.PerClusterPlacementStatuses = []placementv1.PerClusterPlacementStatus{
		{ClusterName: "east", Conditions: []metav1.Condition{{Type: string(placementv1.PerClusterAppliedConditionType), Status: metav1.ConditionTrue}}},
	}
	selection, err = SelectedMembers(placement)
	if err != nil || len(selection.Members) != 1 || selection.Members[0] != "east" {
		t.Fatalf("v1 Applied selection = %#v, err = %v", selection, err)
	}
}

func TestSelectedMembersRetainsScheduledMemberDuringRollout(t *testing.T) {
	placement := &placementv1.ClusterResourcePlacement{
		ObjectMeta: metav1.ObjectMeta{Name: "workloads"},
		Status: placementv1.PlacementStatus{
			ObservedResourceIndex: "5",
			PerClusterPlacementStatuses: []placementv1.PerClusterPlacementStatus{
				{ClusterName: "east", Conditions: []metav1.Condition{{Type: string(placementv1.PerClusterAppliedConditionType), Status: metav1.ConditionTrue}}},
				{ClusterName: "west", Conditions: []metav1.Condition{{Type: string(placementv1.PerClusterScheduledConditionType), Status: metav1.ConditionTrue}}},
			},
		},
	}

	selection, err := SelectedMembers(placement, "east", "west")
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Members) != 2 || selection.Members[0] != "east" || selection.Members[1] != "west" {
		t.Fatalf("selection = %#v", selection)
	}

	placement.Status.PerClusterPlacementStatuses[1].Conditions = append(
		placement.Status.PerClusterPlacementStatuses[1].Conditions,
		metav1.Condition{Type: string(placementv1.PerClusterAppliedConditionType), Status: metav1.ConditionFalse},
	)
	selection, err = SelectedMembers(placement, "east", "west")
	if err != nil || len(selection.Members) != 1 || selection.Members[0] != "east" {
		t.Fatalf("failed rollout selection = %#v, err = %v", selection, err)
	}
}

func TestMemberFromObject(t *testing.T) {
	member := &clusterv1.MemberCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "east",
		Annotations: map[string]string{
			AnnotationAKSResourceID:     "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/east",
			AnnotationNodeResourceGroup: "MC_rg_east_eastus2",
			AnnotationLocation:          "eastus2",
		},
	}}
	got, err := MemberFromObject(member)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubscriptionID != "sub" || got.ResourceGroup != "rg" || got.NodeResourceGroup != "MC_rg_east_eastus2" {
		t.Fatalf("member = %#v", got)
	}
}
