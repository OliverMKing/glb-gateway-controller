package azure

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
)

func TestGlobalLoadBalancerResource(t *testing.T) {
	spec := GlobalLoadBalancerSpec{
		SubscriptionID: "sub", ResourceGroup: "rg", Location: "eastus2",
		LoadBalancerName: "global", PublicIPAddressName: "global-ip",
		Ports:              []int32{443, 80},
		BackendFrontendIDs: []string{"/regional/east", "/regional/west"},
		Tags:               map[string]string{"managed": "true"},
	}
	lb := globalLoadBalancerResource(spec, "/global/pip")
	if lb.SKU == nil || lb.SKU.Tier == nil || *lb.SKU.Tier != armnetwork.LoadBalancerSKUTierGlobal {
		t.Fatalf("load balancer is not Global tier: %#v", lb.SKU)
	}
	if len(lb.Properties.BackendAddressPools) != 1 || len(lb.Properties.BackendAddressPools[0].Properties.LoadBalancerBackendAddresses) != 0 {
		t.Fatalf("unexpected backend pools: %#v", lb.Properties.BackendAddressPools)
	}
	pool := globalBackendPoolResource(spec)
	ids := backendFrontendIDs(pool.Properties)
	if !sameResourceIDs(ids, spec.BackendFrontendIDs) {
		t.Fatalf("backend frontend IDs = %v, want %v", ids, spec.BackendFrontendIDs)
	}
	if len(lb.Properties.LoadBalancingRules) != 2 || *lb.Properties.LoadBalancingRules[0].Properties.FrontendPort != 80 {
		t.Fatalf("rules are not sorted by port: %#v", lb.Properties.LoadBalancingRules)
	}
	if len(lb.Properties.Probes) != 0 {
		t.Fatalf("global Load Balancer must use platform-managed health checks, got probes: %#v", lb.Properties.Probes)
	}
	for _, rule := range lb.Properties.LoadBalancingRules {
		if rule.Properties.Probe != nil {
			t.Fatalf("global Load Balancer rule must not reference a probe: %#v", rule.Properties)
		}
		if rule.Properties.EnableTCPReset != nil {
			t.Fatalf("global Load Balancer rule must not configure TCP reset: %#v", rule.Properties)
		}
	}
	if !globalLoadBalancerParentReady(&lb, spec, "/global/pip") {
		t.Fatal("rendered global Load Balancer should be ready")
	}
	lb.Properties.FrontendIPConfigurations[0].Properties.PublicIPAddress.ID = to.Ptr("/different/pip")
	if globalLoadBalancerParentReady(&lb, spec, "/global/pip") {
		t.Fatal("global Load Balancer with a different Public IP should not be ready")
	}
}

func TestFrontendPorts(t *testing.T) {
	frontendID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/gateway"
	lb := &armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{
		LoadBalancingRules: []*armnetwork.LoadBalancingRule{
			{Properties: &armnetwork.LoadBalancingRulePropertiesFormat{FrontendIPConfiguration: &armnetwork.SubResource{ID: to.Ptr(frontendID)}, FrontendPort: to.Ptr[int32](443)}},
			{Properties: &armnetwork.LoadBalancingRulePropertiesFormat{FrontendIPConfiguration: &armnetwork.SubResource{ID: to.Ptr(frontendID)}, FrontendPort: to.Ptr[int32](80)}},
		},
	}}
	ports := frontendPorts(lb, frontendID)
	if len(ports) != 2 || ports[0] != 80 || ports[1] != 443 || !containsAllPorts(ports, []int32{443}) {
		t.Fatalf("ports = %v", ports)
	}
}

func TestPublicIPAddressReady(t *testing.T) {
	tags := map[string]string{"managed": "true"}
	publicIP := publicIPAddressResource("eastus2", armnetwork.PublicIPAddressSKUTierGlobal, tags)
	publicIP.ID = to.Ptr("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip")
	if !publicIPAddressReady(&publicIP, "eastus2", armnetwork.PublicIPAddressSKUTierGlobal, tags) {
		t.Fatal("rendered Public IP should be ready")
	}
	publicIP.SKU.Tier = to.Ptr(armnetwork.PublicIPAddressSKUTierRegional)
	if publicIPAddressReady(&publicIP, "eastus2", armnetwork.PublicIPAddressSKUTierGlobal, tags) {
		t.Fatal("regional Public IP must not satisfy a global Public IP request")
	}
}
