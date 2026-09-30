# Reproduce the two-region AKS demo

This runbook creates the same end-to-end topology used to validate the MVP:

```text
Azure Global Load Balancer public IP
  -> eastus2 or westus3 regional Azure Load Balancer
  -> Application Network Istio ingress Gateway
  -> global Service and waypoint
  -> ready workload in either region
```

The demo uses AKS Fleet as the only cluster inventory and placement authority.
It does not use the App Routing Istio add-on. Joining each cluster to Azure
Kubernetes Application Network installs the `istio` GatewayClass used by the
controller-generated regional Gateways.

Application Network is an AKS preview feature. Use a test subscription and
recheck the current preview requirements before using this runbook.

## One-command deployment

From an Azure CLI-authenticated shell at the repository root, run:

```bash
RESOURCE_GROUP=kingoliver-demo-2 ./hack/demo/deploy.sh
```

The script is intentionally end to end. It:

1. Creates the resource group and registers the required preview/providers.
2. Creates two AKS clusters in `eastus2` and `westus3`.
3. Creates a hub-enabled Fleet and joins both clusters.
4. Creates Application Network and joins both Fleet members.
5. Builds the controller in ACR and runs it in the east member cluster.
6. Places the global Service, waypoint, and `hello from <region>` workloads.
7. Applies the global Gateway, HTTPRoute, and `GlobalGatewayPolicy`.
8. Waits for both regional frontends and the global VIP, then curls the VIP.

The command exits successfully only after the response is either
`hello from eastus2` or `hello from westus3`.

Common overrides are environment variables:

```bash
SUBSCRIPTION_ID=00000000-0000-0000-0000-000000000000 \
RESOURCE_GROUP=my-glb-demo \
EAST_REGION=eastus2 \
WEST_REGION=westus3 \
APPNET_REGION=westus2 \
NODE_COUNT=2 \
./hack/demo/deploy.sh
```

The script creates a resource-group-specific ACR and a tenant-level application
for the controller. The managed Fleet hub does not run user workloads. The
script runs the controller in the east member cluster and mounts a short-lived
hub kubeconfig. It projects the ServiceAccount token and sets the required
identity environment variables directly. It creates an Entra federated
credential that trusts the east cluster issuer and controller ServiceAccount
subject. No client secret or certificate is stored in the cluster. The script
prints the exact `az ad app delete` cleanup command.

The remaining sections document the same operations individually for
inspection and troubleshooting.

## 1. Prerequisites

Install Azure CLI 2.84.0 or later, `kubectl`, Go 1.25 or later, and the required
CLI extensions:

```bash
az extension add --name fleet --upgrade
az extension add --name appnet-preview --version 1.0.0b4
az extension add --name aks-preview --upgrade
```

The signed-in identity needs permission to create AKS, Fleet, Application
Network, public IP, and load balancer resources in the demo resource group and
the two AKS node resource groups. For an isolated test subscription,
`Contributor` over the subscription is the simplest setup. A production
deployment should replace that with scoped roles.

Set the demo variables. The Application Network resource itself is placed in a
supported region; its members can be in other supported regions.

```bash
export SUBSCRIPTION_ID="$(az account show --query id -o tsv)"
export RESOURCE_GROUP="kingoliver-glb-gateway-controller"
export FLEET_NAME="glb-fleet"
export EAST_CLUSTER="glb-east"
export WEST_CLUSTER="glb-west"
export EAST_MEMBER="east"
export WEST_MEMBER="west"
export EAST_REGION="eastus2"
export WEST_REGION="westus3"
export APPNET_REGION="westus2"
export APPNET_NAME="glb-appnet-wus2"
export HUB_KUBECONFIG="/tmp/glb-fleet-kubeconfig"
export EAST_KUBECONFIG="/tmp/glb-east-kubeconfig"
export WEST_KUBECONFIG="/tmp/glb-west-kubeconfig"
```

Register the Application Network preview and resource providers:

```bash
az feature register \
  --namespace Microsoft.AppLink \
  --name PublicPreview \
  --subscription "$SUBSCRIPTION_ID"

az feature show \
  --namespace Microsoft.AppLink \
  --name PublicPreview \
  --subscription "$SUBSCRIPTION_ID" \
  --query properties.state -o tsv

az provider register --namespace Microsoft.AppLink
az provider register --namespace Microsoft.ContainerService
az provider register --namespace Microsoft.Network
```

Wait until the feature reports `Registered` before continuing.

## 2. Create two AKS clusters

Both clusters need managed Microsoft Entra integration, OIDC, and the managed
Gateway API CRDs. Do not enable the AKS Istio service-mesh add-on; Application
Network manages the data plane used by this demo.

```bash
az group create \
  --name "$RESOURCE_GROUP" \
  --location "$EAST_REGION"

az aks create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$EAST_CLUSTER" \
  --location "$EAST_REGION" \
  --node-count 2 \
  --enable-aad \
  --enable-oidc-issuer \
  --enable-gateway-api \
  --generate-ssh-keys

az aks create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$WEST_CLUSTER" \
  --location "$WEST_REGION" \
  --node-count 2 \
  --enable-aad \
  --enable-oidc-issuer \
  --enable-gateway-api \
  --generate-ssh-keys
```

Capture their resource IDs:

```bash
export EAST_CLUSTER_ID="$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$EAST_CLUSTER" \
  --query id -o tsv)"

export WEST_CLUSTER_ID="$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$WEST_CLUSTER" \
  --query id -o tsv)"
```

## 3. Create the hub-enabled Fleet

The hub cluster is required because the controller reads Fleet placement APIs
and stages the regional Gateway bundle there.

```bash
az fleet create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$FLEET_NAME" \
  --location "$EAST_REGION" \
  --enable-hub

az fleet member create \
  --resource-group "$RESOURCE_GROUP" \
  --fleet-name "$FLEET_NAME" \
  --name "$EAST_MEMBER" \
  --member-cluster-id "$EAST_CLUSTER_ID" \
  --member-labels "region=$EAST_REGION topology=global-demo"

az fleet member create \
  --resource-group "$RESOURCE_GROUP" \
  --fleet-name "$FLEET_NAME" \
  --name "$WEST_MEMBER" \
  --member-cluster-id "$WEST_CLUSTER_ID" \
  --member-labels "region=$WEST_REGION topology=global-demo"
```

Get hub and member credentials. The controller only needs the hub kubeconfig;
the member kubeconfigs are used below to inspect and test the demo.

```bash
az fleet get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$FLEET_NAME" \
  --file "$HUB_KUBECONFIG" \
  --overwrite-existing

az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$EAST_CLUSTER" \
  --file "$EAST_KUBECONFIG" \
  --overwrite-existing

az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$WEST_CLUSTER" \
  --file "$WEST_KUBECONFIG" \
  --overwrite-existing
```

Wait for both Fleet members to report `Joined=True`:

```bash
KUBECONFIG="$HUB_KUBECONFIG" kubectl get memberclusters
```

## 4. Create Application Network and join both clusters

```bash
az appnet create \
  --resource-group "$RESOURCE_GROUP" \
  --appnet-name "$APPNET_NAME" \
  --location "$APPNET_REGION" \
  --identity-type SystemAssigned

az appnet member join \
  --resource-group "$RESOURCE_GROUP" \
  --appnet-name "$APPNET_NAME" \
  --member-name "$EAST_MEMBER" \
  --member-resource-id "$EAST_CLUSTER_ID" \
  --member-location "$EAST_REGION" \
  --upgrade-mode FullyManaged \
  --release-channel Stable

az appnet member join \
  --resource-group "$RESOURCE_GROUP" \
  --appnet-name "$APPNET_NAME" \
  --member-name "$WEST_MEMBER" \
  --member-resource-id "$WEST_CLUSTER_ID" \
  --member-location "$WEST_REGION" \
  --upgrade-mode FullyManaged \
  --release-channel Stable
```

Wait for both members and the managed regional GatewayClass:

```bash
az appnet member show \
  --resource-group "$RESOURCE_GROUP" \
  --appnet-name "$APPNET_NAME" \
  --member-name "$EAST_MEMBER" \
  --query properties.provisioningState -o tsv

az appnet member show \
  --resource-group "$RESOURCE_GROUP" \
  --appnet-name "$APPNET_NAME" \
  --member-name "$WEST_MEMBER" \
  --query properties.provisioningState -o tsv

KUBECONFIG="$EAST_KUBECONFIG" kubectl get gatewayclass istio
KUBECONFIG="$WEST_KUBECONFIG" kubectl get gatewayclass istio
```

Each provisioning state should be `Succeeded`.

## 5. Build and deploy the controller

The automated path builds the image with ACR Tasks. It creates a scoped service
principal with a federated identity credential. It installs the CRDs and RBAC
on the Fleet hub. It then deploys the controller into `glb-gateway-system` in
the east member cluster. The controller uses a mounted hub kubeconfig. The
service principal has these Azure permissions:

- `Reader` and `Network Contributor` on the demo resource group.
- `Network Contributor` on each AKS node resource group.

It also installs Gateway API v1.4.1 CRDs on the hub and grants lease access for
controller-runtime leader election. See `hack/demo/deploy.sh` for the exact,
idempotent commands.

For local controller development, the Azure CLI credential remains a useful
alternative:

```bash
make test
make build

KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -k config/crd
KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -k config/gatewayclass
```

Start the controller in a separate terminal from the repository root:

```bash
AZURE_TOKEN_CREDENTIALS=AzureCLICredential \
KUBECONFIG="$HUB_KUBECONFIG" \
./bin/controller \
  --leader-elect=false \
  --metrics-bind-address=0 \
  --health-probe-bind-address=0
```

The controller creates Azure resources in three scopes:

- The global Public IP and Global-tier load balancer in `RESOURCE_GROUP`.
- One deterministic regional Public IP in each AKS node resource group.
- The generated Istio `LoadBalancer` Service attaches each regional IP to the
  cluster's regional Standard load balancer.

## 6. Apply the demo

Apply backing resources first so Fleet proves the Service and workloads are
healthy before the global entry point is created:

```bash
sed \
  -e "s/MEMBER_CLUSTER_EAST/$EAST_MEMBER/g" \
  -e "s/MEMBER_CLUSTER_WEST/$WEST_MEMBER/g" \
  -e "s/REGION_EAST/$EAST_REGION/g" \
  -e "s/REGION_WEST/$WEST_REGION/g" \
  config/samples/demo_workload.yaml | \
  KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -f -
```

After both regional Deployments have two ready replicas, apply the global
Gateway, HTTPRoute, and policy:

```bash
sed \
  -e "s/SUBSCRIPTION_ID/$SUBSCRIPTION_ID/g" \
  -e "s/RESOURCE_GROUP/$RESOURCE_GROUP/g" \
  -e "s/REGION_GLOBAL/$EAST_REGION/g" \
  config/samples/demo_gateway.yaml | \
  KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -f -
```

The `global-demo-regions` Fleet `ResourceOverride` changes the common
Deployment's `REGION` environment variable per member. The application returns
one of these bodies:

```text
hello from eastus2
hello from westus3
```

Because `global-demo` is deliberately an Application Network global Service,
a request entering the east regional gateway can be served in west, and vice
versa. The response identifies the workload region, not necessarily the
ingress region.

Watch Fleet, Gateway, and policy status until both members are ready:

```bash
KUBECONFIG="$HUB_KUBECONFIG" kubectl get \
  clusterresourceplacement/global-demo-workload \
  -w

KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get \
  globalgatewaypolicy/global-demo \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status}{" "}{.message}{"\n"}{end}'

KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get \
  gateway/global-demo -w
```

The policy should report `MembersReady=True` and `AzureResourcesReady=True`.
The Gateway should report `Accepted=True` and `Programmed=True`. Get the global
address from standard Gateway API status:

```bash
export GLOBAL_IP="$(KUBECONFIG="$HUB_KUBECONFIG" kubectl \
  -n global-demo get gateway global-demo \
  -o jsonpath='{.status.addresses[0].value}')"

echo "$GLOBAL_IP"
curl --fail --show-error "http://$GLOBAL_IP"
```

The same address is retained in
`GlobalGatewayPolicy.status.globalAddress` for controller-specific diagnostics.

## 7. Verify the floating-IP setting

This is required for the chained global-to-regional load balancer path. Istio
normally creates the regional `LoadBalancer` Service with Azure floating IP
enabled. The controller forces this Service annotation on every child Gateway:

```yaml
spec:
  infrastructure:
    annotations:
      service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip: "true"
```

Verify that the managed Istio Service received it in both members:

```bash
KUBECONFIG="$EAST_KUBECONFIG" kubectl -n global-demo get service \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.service\.beta\.kubernetes\.io/azure-disable-load-balancer-floating-ip}{"\n"}{end}'

KUBECONFIG="$WEST_KUBECONFIG" kubectl -n global-demo get service \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.service\.beta\.kubernetes\.io/azure-disable-load-balancer-floating-ip}{"\n"}{end}'
```

For a lower-level check, list the AKS load-balancing rules and verify the
generated Gateway rules show `enableFloatingIP=false`:

```bash
export EAST_NODE_RG="$(az aks show -g "$RESOURCE_GROUP" -n "$EAST_CLUSTER" --query nodeResourceGroup -o tsv)"
export WEST_NODE_RG="$(az aks show -g "$RESOURCE_GROUP" -n "$WEST_CLUSTER" --query nodeResourceGroup -o tsv)"

az network lb rule list -g "$EAST_NODE_RG" --lb-name kubernetes \
  --query '[].[name,enableFloatingIP,frontendPort,backendPort]' -o table

az network lb rule list -g "$WEST_NODE_RG" --lb-name kubernetes \
  --query '[].[name,enableFloatingIP,frontendPort,backendPort]' -o table
```

If the annotation or rule is missing, the regional IP can work directly while
the Global Load Balancer IP hangs or times out.

## 8. Verify each regional entry point and the global entry point

Discover the two regional public addresses from policy status:

```bash
KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get \
  globalgatewaypolicy global-demo -o json | \
  jq -r '.status.members[] | [.name, .regionalPublicIPAddressID] | @tsv'
```

Use `az network public-ip show --ids <resource-id>` to resolve each ID to its
address, then test it. All three endpoints should return a region-labelled
response:

```bash
curl --fail --show-error "http://$EAST_REGIONAL_IP"
curl --fail --show-error "http://$WEST_REGIONAL_IP"
curl --fail --show-error "http://$GLOBAL_IP"
```

The controller also TCP-probes all listener ports before enrolling a regional
frontend. Per-member policy status should contain `Reachable=True`:

```bash
KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get \
  globalgatewaypolicy global-demo \
  -o jsonpath='{range .status.members[*]}{.name}{"\t"}{range .conditions[*]}{.type}={.status}{" "}{end}{"\n"}{end}'
```

## 9. Reproduce the zero-local-endpoint failover

Start a continuous global probe in one terminal:

```bash
while true; do
  date -u +%H:%M:%S
  curl --fail --show-error --max-time 8 "http://$GLOBAL_IP"
  sleep 1
done
```

In another terminal, apply the provided Fleet override. Fleet permits only one
override to select a resource. This file replaces the region-label override and
also changes the east member's `global-demo` Deployment to zero replicas:

```bash
sed \
  -e "s/MEMBER_CLUSTER_EAST/$EAST_MEMBER/g" \
  -e "s/MEMBER_CLUSTER_WEST/$WEST_MEMBER/g" \
  -e "s/REGION_EAST/$EAST_REGION/g" \
  -e "s/REGION_WEST/$WEST_REGION/g" \
  config/samples/failover-east-zero.yaml | \
  KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -f -
```

Wait until east has no pods and no ready local endpoints:

```bash
KUBECONFIG="$EAST_KUBECONFIG" kubectl -n global-demo get \
  deployment global-demo

KUBECONFIG="$EAST_KUBECONFIG" kubectl -n global-demo get \
  endpointslice -l kubernetes.io/service-name=global-demo -o yaml
```

Now both the east regional ingress IP and the global IP should continue to
work, and every response should be from the remaining west workload:

```bash
curl --fail --show-error "http://$EAST_REGIONAL_IP"
# hello from westus3

curl --fail --show-error "http://$GLOBAL_IP"
# hello from westus3
```

Restore the original region override:

```bash
sed \
  -e "s/MEMBER_CLUSTER_EAST/$EAST_MEMBER/g" \
  -e "s/MEMBER_CLUSTER_WEST/$WEST_MEMBER/g" \
  -e "s/REGION_EAST/$EAST_REGION/g" \
  -e "s/REGION_WEST/$WEST_REGION/g" \
  config/samples/demo_workload.yaml | \
  KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -f -

KUBECONFIG="$EAST_KUBECONFIG" kubectl -n global-demo rollout status \
  deployment/global-demo --timeout=10m
```

Application Network is a preview data plane. Wait for endpoint convergence
after workload rollouts before treating transient rollout errors as a global
load-balancer failure.

## 10. Results from the validated environment

The live validation on 2026-09-30 used two two-node AKS clusters in `eastus2`
and `westus3`, one Fleet hub, and one Application Network:

- Both regional Gateway Azure rules reported `enableFloatingIP=false`.
- The global public IP returned HTTP 200 repeatedly.
- With east at zero pods and zero ready EndpointSlice endpoints, the east
  regional IP returned 30/30 successful responses from west.
- During the same steady outage, the global IP returned 30/30 successful
  responses.
- Restoring east returned the environment to two ready replicas in each
  region.

## Troubleshooting

`curl` to a regional IP works but the global IP times out:

- Check the generated Istio Service annotation and confirm the Azure regional
  load-balancing rule has `enableFloatingIP=false`.
- Confirm the Global Load Balancer backend pool contains the regional frontend
  resource IDs, not public IP resource IDs.

The policy says `Reachable=False`:

- Curl the regional public IP directly.
- Check the generated Istio Gateway pod and Service, the AKS load-balancer
  health probe, and Network Security Group rules.
- The controller excludes a frontend that fails its TCP listener probe.

Ingress returns `503` when one member has no pods:

- Confirm the namespace has `istio.io/dataplane-mode=ambient` and
  `istio.io/use-waypoint=waypoint`.
- Confirm the Service has both `istio.io/global=true` and
  `istio.io/ingress-use-waypoint=true`.
- Confirm the waypoint Gateway has `istio.io/global=true` under
  `spec.infrastructure.labels`.
- Confirm both Application Network members are `Succeeded` and the remote
  cluster has ready endpoints.

Fleet applies one member but waits before the next:

- The default Fleet rolling update strategy includes an availability period.
  Watch the placement status; sequential convergence can take several minutes.

## References

- [Get started with Azure Kubernetes Application Network](https://learn.microsoft.com/en-us/azure/application-network/get-started)
- [Application Network traffic-management use cases](https://learn.microsoft.com/en-us/azure/application-network/traffic-management-use-cases#deploy-the-ingress-gateway)
- [Azure Kubernetes Fleet Manager CLI](https://learn.microsoft.com/en-us/cli/azure/fleet)
- [Azure cross-region Load Balancer overview](https://learn.microsoft.com/en-us/azure/load-balancer/cross-region-overview)
