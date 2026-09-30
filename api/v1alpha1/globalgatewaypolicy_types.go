package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	PolicyFinalizer = "gateway.glb.azure.io/finalizer"
)

type LocalObjectReference struct {
	Group string `json:"group,omitempty"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
}

type FleetSpec struct {
	WorkloadPlacementRef LocalObjectReference `json:"workloadPlacementRef"`
}

type AzureSpec struct {
	SubscriptionID      string `json:"subscriptionID"`
	ResourceGroup       string `json:"resourceGroup"`
	Location            string `json:"location"`
	LoadBalancerName    string `json:"loadBalancerName,omitempty"`
	PublicIPAddressName string `json:"publicIPAddressName,omitempty"`
	// +kubebuilder:validation:Enum=IPv4
	// +kubebuilder:default=IPv4
	IPVersion string `json:"ipVersion,omitempty"`
	// +kubebuilder:validation:Enum=Delete;Retain
	// +kubebuilder:default=Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

type RegionalEndpointOverride struct {
	MemberName                string `json:"memberName"`
	FrontendIPConfigurationID string `json:"frontendIPConfigurationID"`
}

type GlobalGatewayPolicySpec struct {
	// TargetRef selects the global Gateway intent on the Fleet hub.
	TargetRef LocalObjectReference `json:"targetRef"`
	// Fleet selects the applied member clusters for this application.
	Fleet FleetSpec `json:"fleet"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MinReadyClusters int32 `json:"minReadyClusters,omitempty"`
	// RegionalEndpointOverrides is an escape hatch when ARM discovery is unavailable.
	RegionalEndpointOverrides []RegionalEndpointOverride `json:"regionalEndpointOverrides,omitempty"`
	// Azure controls the global Load Balancer and Public IP placement.
	Azure AzureSpec `json:"azure"`
}

type FleetStatus struct {
	WorkloadPlacementRef  string `json:"workloadPlacementRef,omitempty"`
	ObservedSnapshot      string `json:"observedSnapshot,omitempty"`
	CompanionPlacementRef string `json:"companionPlacementRef,omitempty"`
}

type NamespacedObjectReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type MemberStatus struct {
	Name                       string                    `json:"name"`
	ChildGatewayRef            NamespacedObjectReference `json:"childGatewayRef,omitempty"`
	RegionalPublicIPAddressID  string                    `json:"regionalPublicIPAddressID,omitempty"`
	RegionalFrontendResourceID string                    `json:"regionalFrontendResourceID,omitempty"`
	Conditions                 []metav1.Condition        `json:"conditions,omitempty"`
}

type GlobalGatewayPolicyStatus struct {
	ObservedGeneration     int64              `json:"observedGeneration,omitempty"`
	GlobalAddress          string             `json:"globalAddress,omitempty"`
	LoadBalancerResourceID string             `json:"loadBalancerResourceID,omitempty"`
	Fleet                  FleetStatus        `json:"fleet,omitempty"`
	Members                []MemberStatus     `json:"members,omitempty"`
	Conditions             []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ggp
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Global Address",type=string,JSONPath=`.status.globalAddress`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="AzureResourcesReady")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type GlobalGatewayPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GlobalGatewayPolicySpec   `json:"spec,omitempty"`
	Status GlobalGatewayPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type GlobalGatewayPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GlobalGatewayPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GlobalGatewayPolicy{}, &GlobalGatewayPolicyList{})
}
