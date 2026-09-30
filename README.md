# Azure Global Load Balancer Gateway Controller

This repository contains a working MVP controller. The full Azure path has been
validated with a two-member AKS Fleet: the controller creates the regional and
global load balancers, the global VIP serves HTTP, and Application Network
keeps serving when one region has zero ready application endpoints.

The controller turns the Gateway API class `global-istio` into a multicluster
Azure Global Load Balancer. A regional `istio` Gateway provides L7 routing in
each selected AKS cluster:

```text
global-istio -> istio
```

The controller requires an AKS Fleet hub. A referenced Fleet workload placement
is the authoritative member set, and Fleet propagates all controller-generated
regional Gateway resources; the controller never stores member kubeconfigs.
The implementation uses Fleet's generated Go types from `go.goms.io/fleet` and
requires the stable `placement.kubernetes-fleet.io/v1` and
`cluster.kubernetes-fleet.io/v1` APIs.
Every selected AKS cluster must also be joined to the same Application Network.
Services labeled `istio.io/global=true` can fall back through the managed
east-west data plane when one member has no ready local endpoints.
The demo uses external Application Network east-west gateways so the two AKS
virtual networks do not need peering. Application Network protects this path
with mutual TLS.
The policy does not contain an Application Network resource ID. The MVP assumes
that all selected Fleet members use the same Application Network. It always
uses only members where Fleet reports that the workload was applied.

When Azure programming succeeds, the controller publishes the global IP in
standard `Gateway.status.addresses` and sets the Gateway `Accepted` and
`Programmed` conditions. `GlobalGatewayPolicy.status` provides the deeper Fleet,
Azure resource, and per-member diagnostics.

Documentation:

- [Visual HTML architecture guide](docs/index.html)
- [Detailed controller design](docs/design.md)
- [Step-by-step demo runbook](docs/demo.md)

Run the complete demo from an Azure CLI-authenticated shell:

```bash
RESOURCE_GROUP=kingoliver-demo-2 ./hack/demo/deploy.sh
```

The script creates the resource group, two AKS clusters, a hub-enabled Fleet,
Application Network, ACR, scoped controller identity, and Azure networking. It
installs the controller API and RBAC on the Fleet hub, runs the controller in
one member cluster, places region-labelled backing services, applies the global
Gateway and HTTPRoute, and exits only after the global public IP returns
`hello from eastus2` or `hello from westus3`.
The controller Deployment uses the exact ACR image digest, and the script waits
for the policy `Accepted`, `MembersReady`, and `AzureResourcesReady` conditions.

The managed Fleet hub does not run user workloads. The script runs the
controller in the east member cluster and mounts a short-lived hub kubeconfig.
It projects a ServiceAccount token into the controller pod and creates an Entra
federated credential for the member-cluster issuer. No client secret or
certificate is stored in the cluster. Delete the script-created application
after the demo.

Build and test:

```bash
make test
make build
docker build -t glb-gateway-controller:dev .
```
