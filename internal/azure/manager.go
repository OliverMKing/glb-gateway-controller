package azure

import (
	"context"
)

type Member struct {
	Name              string
	AKSResourceID     string
	SubscriptionID    string
	ResourceGroup     string
	NodeResourceGroup string
	Location          string
}

type RegionalPublicIP struct {
	ID        string
	IPAddress string
}

type RegionalFrontend struct {
	ID    string
	Ports []int32
}

type GlobalLoadBalancerSpec struct {
	SubscriptionID      string
	ResourceGroup       string
	Location            string
	LoadBalancerName    string
	PublicIPAddressName string
	Ports               []int32
	BackendFrontendIDs  []string
	Tags                map[string]string
}

type GlobalLoadBalancerResult struct {
	LoadBalancerID string
	PublicIPID     string
	IPAddress      string
}

// Manager isolates reconciliation from ARM transport details and supports tests.
type Manager interface {
	EnsureRegionalPublicIP(ctx context.Context, member Member, name string, tags map[string]string) (RegionalPublicIP, error)
	DeleteRegionalPublicIP(ctx context.Context, member Member, name string) error
	ResolveRegionalFrontend(ctx context.Context, member Member, publicIPID string, ports []int32) (RegionalFrontend, error)
	EnsureGlobalLoadBalancer(ctx context.Context, spec GlobalLoadBalancerSpec) (GlobalLoadBalancerResult, error)
	DeleteGlobalLoadBalancer(ctx context.Context, spec GlobalLoadBalancerSpec) error
}
