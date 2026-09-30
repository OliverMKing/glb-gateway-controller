package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
)

const (
	frontendName    = "global-frontend"
	backendPoolName = "regional-frontends"
)

// ARMManager reconciles the Azure resources needed by a global Gateway.
type ARMManager struct {
	credential azcore.TokenCredential
}

func NewARMManager(credential azcore.TokenCredential) (*ARMManager, error) {
	if credential == nil {
		var err error
		credential, err = azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("create default Azure credential: %w", err)
		}
	}
	return &ARMManager{credential: credential}, nil
}

func (m *ARMManager) EnsureRegionalPublicIP(ctx context.Context, member Member, name string, tags map[string]string) (RegionalPublicIP, error) {
	client, err := armnetwork.NewPublicIPAddressesClient(member.SubscriptionID, m.credential, nil)
	if err != nil {
		return RegionalPublicIP{}, fmt.Errorf("create Public IP client: %w", err)
	}
	// Avoid an ARM write when the immutable address shape and ownership tags match.
	desired := publicIPAddressResource(member.Location, armnetwork.PublicIPAddressSKUTierRegional, tags)
	existing, getErr := client.Get(ctx, member.NodeResourceGroup, name, nil)
	if getErr == nil && publicIPAddressReady(&existing.PublicIPAddress, member.Location, armnetwork.PublicIPAddressSKUTierRegional, tags) {
		return regionalPublicIPResult(existing.PublicIPAddress)
	}
	if getErr != nil && !isNotFound(getErr) {
		return RegionalPublicIP{}, fmt.Errorf("get regional Public IP: %w", getErr)
	}
	poller, err := client.BeginCreateOrUpdate(ctx, member.NodeResourceGroup, name, desired, nil)
	if err != nil {
		return RegionalPublicIP{}, err
	}
	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return RegionalPublicIP{}, err
	}
	return regionalPublicIPResult(result.PublicIPAddress)
}

func (m *ARMManager) DeleteRegionalPublicIP(ctx context.Context, member Member, name string) error {
	client, err := armnetwork.NewPublicIPAddressesClient(member.SubscriptionID, m.credential, nil)
	if err != nil {
		return fmt.Errorf("create Public IP client: %w", err)
	}
	poller, err := client.BeginDelete(ctx, member.NodeResourceGroup, name, nil)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (m *ARMManager) ResolveRegionalFrontend(ctx context.Context, member Member, publicIPID string, ports []int32) (RegionalFrontend, error) {
	client, err := armnetwork.NewLoadBalancersClient(member.SubscriptionID, m.credential, nil)
	if err != nil {
		return RegionalFrontend{}, fmt.Errorf("create Load Balancer client: %w", err)
	}
	// AKS owns the regional Load Balancer name, so locate it by its Public IP link.
	pager := client.NewListPager(member.NodeResourceGroup, nil)
	var matches []RegionalFrontend
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return RegionalFrontend{}, err
		}
		for _, lb := range page.Value {
			if lb == nil || lb.Properties == nil {
				continue
			}
			for _, frontend := range lb.Properties.FrontendIPConfigurations {
				if frontend == nil || frontend.ID == nil || frontend.Properties == nil || frontend.Properties.PublicIPAddress == nil {
					continue
				}
				if !equalResourceID(stringValue(frontend.Properties.PublicIPAddress.ID), publicIPID) {
					continue
				}
				covered := frontendPorts(lb, *frontend.ID)
				if !containsAllPorts(covered, ports) {
					return RegionalFrontend{}, fmt.Errorf("regional frontend %s does not expose every Gateway listener port; wanted %v, found %v", *frontend.ID, ports, covered)
				}
				matches = append(matches, RegionalFrontend{ID: *frontend.ID, Ports: covered})
			}
		}
	}
	if len(matches) == 0 {
		return RegionalFrontend{}, fmt.Errorf("Public IP %s is not attached to a regional Load Balancer frontend yet", publicIPID)
	}
	if len(matches) > 1 {
		return RegionalFrontend{}, fmt.Errorf("Public IP %s is attached to more than one regional Load Balancer frontend", publicIPID)
	}
	return matches[0], nil
}

func (m *ARMManager) EnsureGlobalLoadBalancer(ctx context.Context, spec GlobalLoadBalancerSpec) (GlobalLoadBalancerResult, error) {
	if err := validateGlobalSpec(spec); err != nil {
		return GlobalLoadBalancerResult{}, err
	}

	// Reconcile the globally tiered Public IP without rewriting an unchanged resource.
	pipClient, err := armnetwork.NewPublicIPAddressesClient(spec.SubscriptionID, m.credential, nil)
	if err != nil {
		return GlobalLoadBalancerResult{}, fmt.Errorf("create global Public IP client: %w", err)
	}
	var pip armnetwork.PublicIPAddress
	existingPIP, getErr := pipClient.Get(ctx, spec.ResourceGroup, spec.PublicIPAddressName, nil)
	if getErr == nil && publicIPAddressReady(&existingPIP.PublicIPAddress, spec.Location, armnetwork.PublicIPAddressSKUTierGlobal, spec.Tags) {
		pip = existingPIP.PublicIPAddress
	} else {
		if getErr != nil && !isNotFound(getErr) {
			return GlobalLoadBalancerResult{}, fmt.Errorf("get global Public IP: %w", getErr)
		}
		pipPoller, err := pipClient.BeginCreateOrUpdate(ctx, spec.ResourceGroup, spec.PublicIPAddressName, publicIPAddressResource(spec.Location, armnetwork.PublicIPAddressSKUTierGlobal, spec.Tags), nil)
		if err != nil {
			return GlobalLoadBalancerResult{}, fmt.Errorf("create global Public IP: %w", err)
		}
		result, err := pipPoller.PollUntilDone(ctx, nil)
		if err != nil {
			return GlobalLoadBalancerResult{}, fmt.Errorf("wait for global Public IP: %w", err)
		}
		pip = result.PublicIPAddress
	}
	if pip.ID == nil {
		return GlobalLoadBalancerResult{}, errors.New("Azure returned a global Public IP without a resource ID")
	}

	// The parent owns listeners and rules; the child backend pool is handled below.
	lb := globalLoadBalancerResource(spec, *pip.ID)
	lbClient, err := armnetwork.NewLoadBalancersClient(spec.SubscriptionID, m.credential, nil)
	if err != nil {
		return GlobalLoadBalancerResult{}, fmt.Errorf("create global Load Balancer client: %w", err)
	}
	var created armnetwork.LoadBalancer
	existing, getErr := lbClient.Get(ctx, spec.ResourceGroup, spec.LoadBalancerName, nil)
	if getErr == nil && globalLoadBalancerParentReady(&existing.LoadBalancer, spec, *pip.ID) {
		created = existing.LoadBalancer
	} else {
		if getErr != nil && !isNotFound(getErr) {
			return GlobalLoadBalancerResult{}, fmt.Errorf("get global Load Balancer: %w", getErr)
		}
		lbPoller, err := lbClient.BeginCreateOrUpdate(ctx, spec.ResourceGroup, spec.LoadBalancerName, lb, nil)
		if err != nil {
			return GlobalLoadBalancerResult{}, fmt.Errorf("create global Load Balancer: %w", err)
		}
		result, err := lbPoller.PollUntilDone(ctx, nil)
		if err != nil {
			return GlobalLoadBalancerResult{}, fmt.Errorf("wait for global Load Balancer: %w", err)
		}
		created = result.LoadBalancer
	}
	if created.ID == nil {
		return GlobalLoadBalancerResult{}, errors.New("Azure returned a global Load Balancer without a resource ID")
	}

	// Azure accepts backend addresses embedded in the parent Load Balancer PUT,
	// but cross-region Load Balancer currently drops them. Reconcile the child
	// backend pool explicitly and verify the regional frontend references stuck.
	poolClient, err := armnetwork.NewLoadBalancerBackendAddressPoolsClient(spec.SubscriptionID, m.credential, nil)
	if err != nil {
		return GlobalLoadBalancerResult{}, fmt.Errorf("create global backend pool client: %w", err)
	}
	pool, poolGetErr := poolClient.Get(ctx, spec.ResourceGroup, spec.LoadBalancerName, backendPoolName, nil)
	if poolGetErr != nil && !isNotFound(poolGetErr) {
		return GlobalLoadBalancerResult{}, fmt.Errorf("get global backend pool: %w", poolGetErr)
	}
	if poolGetErr != nil || !sameResourceIDs(backendFrontendIDs(pool.Properties), spec.BackendFrontendIDs) {
		poolPoller, err := poolClient.BeginCreateOrUpdate(ctx, spec.ResourceGroup, spec.LoadBalancerName, backendPoolName, globalBackendPoolResource(spec), nil)
		if err != nil {
			return GlobalLoadBalancerResult{}, fmt.Errorf("configure global backend pool: %w", err)
		}
		if _, err = poolPoller.PollUntilDone(ctx, nil); err != nil {
			return GlobalLoadBalancerResult{}, fmt.Errorf("wait for global backend pool: %w", err)
		}
		pool, err = poolClient.Get(ctx, spec.ResourceGroup, spec.LoadBalancerName, backendPoolName, nil)
		if err != nil {
			return GlobalLoadBalancerResult{}, fmt.Errorf("verify global backend pool: %w", err)
		}
	}
	actualFrontendIDs := backendFrontendIDs(pool.Properties)
	if !sameResourceIDs(actualFrontendIDs, spec.BackendFrontendIDs) {
		return GlobalLoadBalancerResult{}, fmt.Errorf("global backend pool contains regional frontends %v, wanted %v", actualFrontendIDs, spec.BackendFrontendIDs)
	}
	return GlobalLoadBalancerResult{
		LoadBalancerID: *created.ID,
		PublicIPID:     *pip.ID,
		IPAddress:      publicIPAddressValue(pip.Properties),
	}, nil
}

func publicIPAddressResource(location string, tier armnetwork.PublicIPAddressSKUTier, tags map[string]string) armnetwork.PublicIPAddress {
	return armnetwork.PublicIPAddress{
		Location: to.Ptr(location),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
			Tier: to.Ptr(tier),
		},
		Tags: pointerTags(tags),
	}
}

func publicIPAddressReady(publicIP *armnetwork.PublicIPAddress, location string, tier armnetwork.PublicIPAddressSKUTier, tags map[string]string) bool {
	if publicIP == nil || publicIP.ID == nil || publicIP.Properties == nil || publicIP.SKU == nil || publicIP.SKU.Name == nil || publicIP.SKU.Tier == nil {
		return false
	}
	if !strings.EqualFold(stringValue(publicIP.Location), location) || *publicIP.SKU.Name != armnetwork.PublicIPAddressSKUNameStandard || *publicIP.SKU.Tier != tier {
		return false
	}
	if publicIP.Properties.PublicIPAddressVersion == nil || *publicIP.Properties.PublicIPAddressVersion != armnetwork.IPVersionIPv4 || publicIP.Properties.PublicIPAllocationMethod == nil || *publicIP.Properties.PublicIPAllocationMethod != armnetwork.IPAllocationMethodStatic {
		return false
	}
	for key, value := range tags {
		if publicIP.Tags[key] == nil || *publicIP.Tags[key] != value {
			return false
		}
	}
	return true
}

func regionalPublicIPResult(publicIP armnetwork.PublicIPAddress) (RegionalPublicIP, error) {
	if publicIP.ID == nil {
		return RegionalPublicIP{}, errors.New("Azure returned a regional Public IP without a resource ID")
	}
	return RegionalPublicIP{ID: *publicIP.ID, IPAddress: publicIPAddressValue(publicIP.Properties)}, nil
}

func (m *ARMManager) DeleteGlobalLoadBalancer(ctx context.Context, spec GlobalLoadBalancerSpec) error {
	// Release the Load Balancer reference before deleting its Public IP.
	lbClient, err := armnetwork.NewLoadBalancersClient(spec.SubscriptionID, m.credential, nil)
	if err != nil {
		return fmt.Errorf("create global Load Balancer client: %w", err)
	}
	lbPoller, err := lbClient.BeginDelete(ctx, spec.ResourceGroup, spec.LoadBalancerName, nil)
	if err == nil {
		_, err = lbPoller.PollUntilDone(ctx, nil)
	}
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete global Load Balancer: %w", err)
	}

	pipClient, err := armnetwork.NewPublicIPAddressesClient(spec.SubscriptionID, m.credential, nil)
	if err != nil {
		return fmt.Errorf("create global Public IP client: %w", err)
	}
	pipPoller, err := pipClient.BeginDelete(ctx, spec.ResourceGroup, spec.PublicIPAddressName, nil)
	if err == nil {
		_, err = pipPoller.PollUntilDone(ctx, nil)
	}
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete global Public IP: %w", err)
	}
	return nil
}

func globalLoadBalancerResource(spec GlobalLoadBalancerSpec, publicIPID string) armnetwork.LoadBalancer {
	lbID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s", spec.SubscriptionID, spec.ResourceGroup, spec.LoadBalancerName)
	frontendID := lbID + "/frontendIPConfigurations/" + frontendName
	poolID := lbID + "/backendAddressPools/" + backendPoolName

	// Give every Gateway listener a symmetric TCP rule through the regional GLBs.
	ports := append([]int32(nil), spec.Ports...)
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	rules := make([]*armnetwork.LoadBalancingRule, 0, len(ports))
	for _, port := range ports {
		rules = append(rules, &armnetwork.LoadBalancingRule{
			Name: to.Ptr(fmt.Sprintf("tcp-%d", port)),
			Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
				FrontendIPConfiguration: &armnetwork.SubResource{ID: to.Ptr(frontendID)},
				BackendAddressPool:      &armnetwork.SubResource{ID: to.Ptr(poolID)},
				Protocol:                to.Ptr(armnetwork.TransportProtocolTCP),
				FrontendPort:            to.Ptr(port),
				BackendPort:             to.Ptr(port),
				EnableFloatingIP:        to.Ptr(false),
				IdleTimeoutInMinutes:    to.Ptr[int32](4),
				LoadDistribution:        to.Ptr(armnetwork.LoadDistributionDefault),
			},
		})
	}

	return armnetwork.LoadBalancer{
		Location: to.Ptr(spec.Location),
		SKU: &armnetwork.LoadBalancerSKU{
			Name: to.Ptr(armnetwork.LoadBalancerSKUNameStandard),
			Tier: to.Ptr(armnetwork.LoadBalancerSKUTierGlobal),
		},
		Tags: pointerTags(spec.Tags),
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{
				Name: to.Ptr(frontendName),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{ID: to.Ptr(publicIPID)},
				},
			}},
			BackendAddressPools: []*armnetwork.BackendAddressPool{{
				Name:       to.Ptr(backendPoolName),
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{},
			}},
			LoadBalancingRules: rules,
		},
	}
}

func globalLoadBalancerParentReady(lb *armnetwork.LoadBalancer, spec GlobalLoadBalancerSpec, publicIPID string) bool {
	// Compare only fields owned by this controller; Azure populates the rest.
	if lb == nil || lb.Properties == nil || lb.SKU == nil || lb.SKU.Name == nil || lb.SKU.Tier == nil {
		return false
	}
	if *lb.SKU.Name != armnetwork.LoadBalancerSKUNameStandard || *lb.SKU.Tier != armnetwork.LoadBalancerSKUTierGlobal || !strings.EqualFold(stringValue(lb.Location), spec.Location) {
		return false
	}
	for key, value := range spec.Tags {
		if lb.Tags[key] == nil || *lb.Tags[key] != value {
			return false
		}
	}

	lbID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s", spec.SubscriptionID, spec.ResourceGroup, spec.LoadBalancerName)
	frontendID := lbID + "/frontendIPConfigurations/" + frontendName
	poolID := lbID + "/backendAddressPools/" + backendPoolName
	frontendFound := false
	for _, frontend := range lb.Properties.FrontendIPConfigurations {
		if frontend != nil && stringValue(frontend.Name) == frontendName && frontend.Properties != nil && frontend.Properties.PublicIPAddress != nil && equalResourceID(stringValue(frontend.Properties.PublicIPAddress.ID), publicIPID) {
			frontendFound = true
			break
		}
	}
	if !frontendFound {
		return false
	}
	poolFound := false
	for _, pool := range lb.Properties.BackendAddressPools {
		if pool != nil && stringValue(pool.Name) == backendPoolName {
			poolFound = true
			break
		}
	}
	if !poolFound || len(lb.Properties.LoadBalancingRules) != len(spec.Ports) {
		return false
	}
	wantedPorts := make(map[int32]struct{}, len(spec.Ports))
	for _, port := range spec.Ports {
		wantedPorts[port] = struct{}{}
	}
	for _, rule := range lb.Properties.LoadBalancingRules {
		if rule == nil || rule.Properties == nil || rule.Properties.Protocol == nil || *rule.Properties.Protocol != armnetwork.TransportProtocolTCP || rule.Properties.FrontendPort == nil || rule.Properties.BackendPort == nil || *rule.Properties.FrontendPort != *rule.Properties.BackendPort || rule.Properties.FrontendIPConfiguration == nil || rule.Properties.BackendAddressPool == nil {
			return false
		}
		if _, found := wantedPorts[*rule.Properties.FrontendPort]; !found || !equalResourceID(stringValue(rule.Properties.FrontendIPConfiguration.ID), frontendID) || !equalResourceID(stringValue(rule.Properties.BackendAddressPool.ID), poolID) {
			return false
		}
	}
	return true
}

func globalBackendPoolResource(spec GlobalLoadBalancerSpec) armnetwork.BackendAddressPool {
	// Stable ordering keeps generated backend names deterministic across reconciles.
	frontendIDs := append([]string(nil), spec.BackendFrontendIDs...)
	sort.Slice(frontendIDs, func(i, j int) bool { return strings.ToLower(frontendIDs[i]) < strings.ToLower(frontendIDs[j]) })
	backendAddresses := make([]*armnetwork.LoadBalancerBackendAddress, 0, len(frontendIDs))
	for index, id := range frontendIDs {
		backendAddresses = append(backendAddresses, &armnetwork.LoadBalancerBackendAddress{
			Name: to.Ptr(fmt.Sprintf("regional-frontend-%d", index)),
			Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
				LoadBalancerFrontendIPConfiguration: &armnetwork.SubResource{ID: to.Ptr(id)},
			},
		})
	}
	return armnetwork.BackendAddressPool{
		Name: to.Ptr(backendPoolName),
		Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
			LoadBalancerBackendAddresses: backendAddresses,
		},
	}
}

func backendFrontendIDs(properties *armnetwork.BackendAddressPoolPropertiesFormat) []string {
	if properties == nil {
		return nil
	}
	ids := make([]string, 0, len(properties.LoadBalancerBackendAddresses))
	for _, address := range properties.LoadBalancerBackendAddresses {
		if address == nil || address.Properties == nil || address.Properties.LoadBalancerFrontendIPConfiguration == nil || address.Properties.LoadBalancerFrontendIPConfiguration.ID == nil {
			continue
		}
		ids = append(ids, *address.Properties.LoadBalancerFrontendIPConfiguration.ID)
	}
	return ids
}

func sameResourceIDs(actual, wanted []string) bool {
	if len(actual) != len(wanted) {
		return false
	}
	actualSet := make(map[string]int, len(actual))
	for _, id := range actual {
		actualSet[strings.ToLower(strings.TrimSuffix(id, "/"))]++
	}
	for _, id := range wanted {
		key := strings.ToLower(strings.TrimSuffix(id, "/"))
		if actualSet[key] == 0 {
			return false
		}
		actualSet[key]--
	}
	return true
}

func frontendPorts(lb *armnetwork.LoadBalancer, frontendID string) []int32 {
	seen := map[int32]struct{}{}
	for _, rule := range lb.Properties.LoadBalancingRules {
		if rule == nil || rule.Properties == nil || rule.Properties.FrontendIPConfiguration == nil || rule.Properties.FrontendPort == nil {
			continue
		}
		if equalResourceID(stringValue(rule.Properties.FrontendIPConfiguration.ID), frontendID) {
			seen[*rule.Properties.FrontendPort] = struct{}{}
		}
	}
	ports := make([]int32, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports
}

func containsAllPorts(actual, wanted []int32) bool {
	set := make(map[int32]struct{}, len(actual))
	for _, port := range actual {
		set[port] = struct{}{}
	}
	for _, port := range wanted {
		if _, found := set[port]; !found {
			return false
		}
	}
	return true
}

func validateGlobalSpec(spec GlobalLoadBalancerSpec) error {
	if spec.SubscriptionID == "" || spec.ResourceGroup == "" || spec.Location == "" || spec.LoadBalancerName == "" || spec.PublicIPAddressName == "" {
		return errors.New("global Load Balancer subscription, resource group, location, Load Balancer name, and Public IP name are required")
	}
	if len(spec.Ports) == 0 {
		return errors.New("global Load Balancer needs at least one listener port")
	}
	if len(spec.BackendFrontendIDs) == 0 {
		return errors.New("global Load Balancer needs at least one regional frontend")
	}
	return nil
}

func pointerTags(tags map[string]string) map[string]*string {
	if len(tags) == 0 {
		return nil
	}
	result := make(map[string]*string, len(tags))
	for key, value := range tags {
		result[key] = to.Ptr(value)
	}
	return result
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func publicIPAddressValue(properties *armnetwork.PublicIPAddressPropertiesFormat) string {
	if properties == nil {
		return ""
	}
	return stringValue(properties.IPAddress)
}

func normalizeResourceID(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "/"))
}

func equalResourceID(left, right string) bool {
	return normalizeResourceID(left) == normalizeResourceID(right)
}

func isNotFound(err error) bool {
	var responseErr *azcore.ResponseError
	return errors.As(err, &responseErr) && responseErr.StatusCode == http.StatusNotFound
}
