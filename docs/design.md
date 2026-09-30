# Design: Azure Global Load Balancer Gateway Controller

- Status: MVP implemented and Azure end-to-end demo validated
- Date: 2026-09-30
- Initial target: Azure Kubernetes Application Network ingress Gateway
- Controller name: `gateway.glb.azure.io/controller`
- API group: `gateway.glb.azure.io`

## Summary

The controller presents a normal Kubernetes Gateway API interface for a global,
multicluster entry point. A Gateway using a class named
`global-<regional-class>` causes the controller to:

1. Select a set of AKS clusters.
2. Create a child `Gateway` in each cluster using `<regional-class>`.
3. Optionally copy the attached `HTTPRoute` resources into each cluster, while
   leaving Service-to-pod routing to that cluster's regional Gateway
   implementation.
4. Discover the public Azure Standard Load Balancer frontend created for each
   child Gateway.
5. Create an Azure Global Load Balancer whose backend pool contains those
   regional load balancer frontends.
6. Publish the global public IP and per-member state in
   `GlobalGatewayPolicy.status`.

For the Azure Kubernetes Application Network MVP, the mapping is:

```text
global-istio -> istio
```

Application Network's documented ingress GatewayClass is `istio`. “AppLink” is
the Azure resource-provider name (`Microsoft.AppLink`), not the GatewayClass
name, so the global class is `global-istio`, not `global-applink-istio`.

Azure Global Load Balancer is a Layer 4 service. It selects a healthy region
and forwards the TCP connection to that region's Azure Load Balancer. HTTP,
host/path matching, redirects, TLS termination, and Service routing remain in
the regional Layer 7 proxy created by Application Network or another regional
GatewayClass. Application Network additionally makes selected Services global
across its members, so a regional proxy can reach healthy endpoints in another
region when its local endpoints are unavailable.

## Goals

- Expose a single global public IP for equivalent applications running in
  multiple AKS clusters.
- Use standard `Gateway`, `HTTPRoute`, `GatewayClass`, and route status wherever
  possible.
- Make `global-<regional-class>` a predictable mapping to an existing regional
  GatewayClass.
- Use AKS Fleet membership and placement as the only source of member-cluster
  inventory and grouping.
- Require selected Fleet members to belong to the same Azure Kubernetes
  Application Network resource.
- Propagate every regional Gateway and route through Fleet placement rather
  than direct member-cluster credentials.
- Use Application Network global Services and east-west gateways for
  cross-cluster backend fallback.
- Preserve each regional controller's ownership of its proxy Deployment,
  Service, health probes, TLS integration, and local route programming.
- Make cluster and Azure reconciliation declarative, idempotent, observable,
  and safe to retry.
- Support clusters in different regions and, when Azure permissions permit,
  different subscriptions.
- Design the core around a regional endpoint adapter, with
  Application Network's `istio` class as the first tested implementation.

## Non-goals for the MVP

- Implementing a new Layer 7 proxy or replacing the regional Gateway
  controller.
- Performing global host/path routing in Azure. The global load balancer is L4.
- Copying application Deployments, Services, Secrets, or arbitrary
  `ReferenceGrant` resources between clusters.
- Treating an HTTP `5xx` response as proof that an endpoint is unhealthy. The
  MVP depends on Kubernetes readiness and Application Network endpoint
  discovery; a pod that remains Ready while returning errors can still receive
  traffic.
- Supporting private regional Gateway frontends. Azure Global Load Balancer
  backends must be public regional load balancer frontends.
- Sharing one Azure Global Load Balancer between unrelated Gateway resources.
- Providing global session replication or guaranteeing that an existing TCP
  connection survives a regional failure.
- Managing Azure DNS zones in the MVP. The global IP in
  `Gateway.status.addresses` is intended to work with DNS tooling or a
  separately managed record.
- Introducing a separate Multicluster Services API `ServiceImport` path for the
  MVP; Application Network supplies its own managed multicluster discovery.
- Supporting hubless Fleets or clusters that are not joined to the controller's
  AKS Fleet.
- Supporting clusters that are not joined to the policy's Application Network,
  or Application Network members without east-west network reachability.

## Important compatibility boundary

The `global-*` naming convention is generic, but Azure Global Load Balancer
cannot target every possible Gateway implementation. A regional GatewayClass is
compatible when all of the following are true:

- It provisions a public `Service` or equivalent endpoint.
- The endpoint is a frontend IP configuration on an Azure Standard regional
  Load Balancer.
- It exposes the same TCP listener ports requested by the global Gateway.
- It reports enough status or metadata for the controller to associate the
  child Gateway with that Azure frontend, or the user supplies the frontend
  resource ID explicitly.
- The same derived regional class exists in every selected Fleet member.
- For the resilient-service profile, every selected member belongs to the same
  Application Network and every backend Service is marked global.

This means the prefix is an automatic control-plane mapping, not a promise that
an arbitrary data plane can be attached to Azure Global Load Balancer.

## Architecture

```text
                               AKS Fleet hub API

  GatewayClass              Gateway + HTTPRoute       GlobalGatewayPolicy
  global-istio               class: global-istio       references app placement
             \                       |                       /
              +----------------------+----------------------+
                                     |
                 glb-gateway-controller in one member cluster
                    +----------------+------------------+
                    |                |                  |
          Fleet ResourcePlacement   Azure ARM      status aggregation
                    |            PIPs + GLB              |
       +------------+---------+      |                  |
       |                      |      |                  |
       v                      v      v                  |
  AKS cluster A          AKS cluster B               |
  Gateway class:         Gateway class:              |
  istio                  istio                       |
       |                      |                       |
  Envoy/Istio gateway    Envoy/Istio gateway         |
       |                      |                       |
  regional Standard LB   regional Standard LB <------+ backend frontends
       |                      |
  global Service         global Service
       |                      |
       +----- Application Network east-west ----------+

Data path:
client -> global public IP -> selected regional Azure LB -> regional L7 proxy
       -> HTTPRoute match -> Application Network global Service
       -> healthy local pod, or east-west gateway -> healthy remote pod
```

The controller runs once in a Fleet member cluster with leader election. A
mounted kubeconfig connects it to the Fleet hub API. The hub is the control and
staging plane and does not run user workloads. Every traffic-serving cluster
must be joined to that Fleet. A Fleet hub is therefore a hard prerequisite.

## API design

The design uses Gateway API resources for traffic intent, Fleet APIs for
cluster membership and placement, and one small custom resource for
Azure/global placement.

### GatewayClass convention

The controller uses a class named `global-<regional-class>` to identify the
regional implementation that Fleet must provide on every selected member. The
installation creates the first supported class:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: global-istio
  annotations:
    gateway.glb.azure.io/regional-class: istio
spec:
  controllerName: gateway.glb.azure.io/controller
```

Class behavior:

- The controller installation creates `global-istio`. Additional
  global classes may be created explicitly with the regional-class annotation.
- The Fleet hub does not need the regional class. The controller validates that
  the derived regional class exists in every selected Fleet member through
  Fleet rollout and Azure provisioning signals.
- If `global-` plus the regional name exceeds the 63-character Kubernetes name
  limit, an explicitly named GatewayClass with the regional-class annotation is
  the escape hatch.
- The controller must not adopt or overwrite an existing GatewayClass unless
  its controller name and ownership annotations already identify this
  controller.

### AKS Fleet requirement

Azure Kubernetes Fleet Manager is the only cluster inventory and placement
provider. It answers two separate questions:

1. Which clusters belong to the administratively managed set? Fleet membership
   and `MemberCluster` objects answer this.
2. Which members should receive a particular application or global Gateway?
   Fleet placement policy and placement status answer this.

The controller imports Fleet's generated API types from `go.goms.io/fleet`.
It reads `ClusterResourcePlacement`, `ResourcePlacement`, and `MemberCluster`
through the stable `placement.kubernetes-fleet.io/v1` and
`cluster.kubernetes-fleet.io/v1` APIs; it does not use dynamic unstructured
objects for Fleet resources.

A Fleet is the outer cluster set, not automatically one global load-balancing
group. One Fleet can contain many applications, environments, and regional
groups. Each `GlobalGatewayPolicy` references the Fleet placement that defines
the exact member subset for that application.

The recommended policy form references the placement that deploys the
application workload:

```yaml
spec:
  fleet:
    workloadPlacementRef:
      group: placement.kubernetes-fleet.io
      kind: ClusterResourcePlacement
      name: store-workload
```

`ResourcePlacement` is also accepted for fine-grained namespaced workload
placement. Referencing actual placement status is safer than repeating a label
selector: the global controller links only clusters where Fleet reports that
the application placement was successfully applied.

The referenced workload placement is used for member selection and backend
dependency validation, but it must not propagate the hub-only global Gateway,
source HTTPRoutes, `GlobalGatewayPolicy`, or controller staging objects. A
fine-grained `ResourcePlacement` selecting application resources by label or
name is preferred. The controller rejects a placement snapshot that includes
its global control objects. Namespace-wide placement is safe only when global
control objects are kept in a separate hub-only namespace.

The workload placement may use `PickAll`, `PickN`, or `PickFixed`, required
member label/property selectors, and taint tolerations. The global controller
does not implement a second scheduler. It snapshots the successfully applied
member names from the referenced workload placement and creates a companion
Fleet `ResourcePlacement` with `PickFixed` for the generated regional Gateway
and route bundle.

Useful Fleet member labels include durable cluster traits, for example:

```yaml
environment: prod
traffic-group: us-public
gateway.glb.azure.io/application-network-istio: "true"
fleet.azure.com/location: eastus
```

Application-specific placement should normally come from the referenced Fleet
placement, while labels describe cluster capability and administrative intent.

For a policy, a cluster becomes an Azure Global Load Balancer backend only when
it is in this intersection:

```text
Fleet member with Joined=True
  AND selected by the observed Fleet placement snapshot
  AND placement reports Applied=True
  AND no untolerated maintenance/exclusion taint applies
  AND the regional GatewayClass is available
  AND the Application Network global-Service prerequisites are present
  AND a compatible public regional Azure LB frontend is resolved
```

The Gateway publishes the global IP and standard `Accepted` and `Programmed`
conditions. Policy status records the Azure resource IDs, Fleet placement
reference, placement snapshot/observed generation, and exact selected member
names. This makes the answer to "why is this cluster in the GLB?" auditable.

The controller stages deterministic child Gateway and HTTPRoute objects on the
Fleet hub and distributes them through a companion `ResourcePlacement`. It
uses placement status for workload-member selection and validates the resulting
regional Azure infrastructure directly. It never stores member-cluster
kubeconfigs or writes directly to a member Kubernetes API.

Fleet can report `Available=True` with `WorkNotTrackable` for custom resources.
The controller therefore does not rely on placement availability alone. It
also validates the Azure infrastructure created for every child Gateway.

To make Azure discovery deterministic without reading the generated Service
from the member API, the controller:

1. Resolves each selected Fleet member's AKS resource ID and node resource
   group.
2. Creates a Standard regional Public IP with the same UID-derived name in each
   member's node resource group.
3. Adds `service.beta.kubernetes.io/azure-pip-name` under the child Gateway's
   `spec.infrastructure.annotations` so the generated Service requests it.
4. Propagates the child Gateway through Fleet.
5. Queries Azure until the member's regional Standard Load Balancer has a
   frontend referencing that known Public IP and matching listener rules.

This keeps the control path Fleet-only while making the regional Azure
frontend discoverable through ARM.

Membership and placement changes are reconciled continuously. When Fleet moves
a `PickN` placement, the controller waits for the replacement cluster to become
ready and adds its Azure backend before removing the old cluster where the
Fleet rollout allows that ordering.

Fleet's existing public multicluster load-balancing feature uses Azure Traffic
Manager and DNS. That is a valid alternative when DNS-level failover is
acceptable, but it is different from this controller's single Global-tier
public IP and Azure Global Load Balancer data path.

### Azure Kubernetes Application Network requirement

Fleet and Application Network have complementary jobs:

- Fleet is the authoritative application placement and controller propagation
  mechanism.
- Application Network is the service data plane and cross-cluster endpoint
  discovery mechanism.

Every Fleet member selected by a `GlobalGatewayPolicy` must also be joined to
the same Application Network resource. This is an MVP deployment requirement.
The policy does not contain the Application Network ARM resource ID, and the
controller does not validate this membership through ARM. Fleet remains the
authority for which subset serves the application.

The workload placement must establish the documented Application Network data
plane contract in every selected member:

- The application namespace is enrolled in ambient mode with
  `istio.io/dataplane-mode=ambient`.
- Each cross-cluster backend Service has `istio.io/global="true"` and the same
  namespace, name, and compatible port definition in each member.
- The documented waypoint configuration is deployed when L7 service policy is
  required. When a waypoint Service is used, it is also marked global.
- East-west gateways have network reachability through external gateway
  addresses, VNet peering, VPN, or another supported connectivity design.
- Managed Gateway API is enabled and Application Network provides an accepted
  `istio` GatewayClass.

Application Network is currently an AKS preview feature. The public
documentation says preview features are not intended for production use and
do not carry a production SLA. This design is therefore suitable for an MVP
and validation environment; a production release gate requires the feature's
support status and limitations to be reassessed.

### GlobalGatewayPolicy

`GlobalGatewayPolicy` is namespaced and attaches directly to one Gateway. It
selects clusters and describes the Azure resource placement.

```yaml
apiVersion: gateway.glb.azure.io/v1alpha1
kind: GlobalGatewayPolicy
metadata:
  name: store-global
  namespace: store
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: store

  fleet:
    workloadPlacementRef:
      group: placement.kubernetes-fleet.io
      kind: ClusterResourcePlacement
      name: store-workload

  minReadyClusters: 1

  regionalEndpointOverrides:
    # Optional escape hatch for a class without an endpoint-discovery adapter.
    # - memberName: westus-prod
    #   frontendIPConfigurationID: /subscriptions/.../frontendIPConfigurations/...

  azure:
    subscriptionID: 00000000-0000-0000-0000-000000000000
    resourceGroup: global-networking-rg
    location: eastus2
    loadBalancerName: store-global
    publicIPAddressName: store-global
    ipVersion: IPv4
    deletionPolicy: Delete
```

Policy rules:

- Exactly one policy may target a Gateway.
- `fleet.workloadPlacementRef` is required and must resolve to a Fleet
  `ClusterResourcePlacement` or `ResourcePlacement` on the same hub. A newly
  selected and applied member is added; a no-longer-selected member is drained
  and removed.
- The controller records the referenced placement snapshot and exact member
  names used for every reconciliation.
- The controller always uses only members where Fleet reports `Applied=True`.
  The API does not expose a switch for this behavior.
- All selected members must use the same Application Network. This is an MVP
  deployment requirement. The policy does not contain its resource ID, and the
  controller does not validate Application Network membership through ARM.
- The controller always validates the ambient namespace labels, global Service
  labels, and waypoint resources used by each copied route.
- `minReadyClusters` controls whether the global Gateway can be considered
  usable. It does not hide partial failure; per-cluster state is always exposed
  on policy status.
- `regionalEndpointOverrides` supplies a regional load balancer frontend for a
  specific Fleet member when it cannot be discovered from the regional
  GatewayClass. The controller still verifies that the frontend is public,
  regional, port-compatible, and consistent with the member's AKS resource.
- The MVP creates a dedicated global load balancer and public IP per Gateway.
- `deletionPolicy: Retain` is available for break-glass recovery, but `Delete`
  is the normal default.
- Azure names are optional in the API implementation and can default to stable,
  UID-derived names. They are explicit above to show ownership clearly.

Policy status includes the Azure resource IDs, global IP, observed Gateway UID,
and an entry for every selected Fleet member:

```yaml
# Source Gateway
status:
  addresses:
    - type: IPAddress
      value: 203.0.113.10
  conditions:
    - type: Accepted
      status: "True"
    - type: Programmed
      status: "True"
```

The controller-specific policy view provides the underlying detail:

```yaml
status:
  globalAddress: 203.0.113.10
  loadBalancerResourceID: /subscriptions/.../loadBalancers/store-global
  fleet:
    workloadPlacementRef: store-workload
    observedSnapshot: 4
    companionPlacementRef: store-global-g7k2m
  members:
    - name: eastus-prod
      childGatewayRef:
        namespace: store
        name: store-g7k2m
      regionalFrontendResourceID: /subscriptions/.../frontendIPConfigurations/...
      conditions: []
  conditions: []
```

For an initial operational limit, one policy should select no more than 50
clusters so that detailed status remains comfortably below the Kubernetes
object-size limit.

### Source Gateway and HTTPRoute

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: store
  namespace: store
spec:
  gatewayClassName: global-istio
  listeners:
    - name: https
      protocol: HTTPS
      port: 443
      hostname: store.example.com
      tls:
        mode: Terminate
        certificateRefs:
          - kind: Secret
            name: store-tls
      allowedRoutes:
        namespaces:
          from: Same
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: store
  namespace: store
spec:
  parentRefs:
    - name: store
      sectionName: https
  hostnames:
    - store.example.com
  rules:
    - backendRefs:
        - name: store
          port: 8080
```

The workload placement creates the `store` namespace and Service in each
selected member, labels the namespace for ambient mode, and labels the Service
as global. It also deploys the workload, TLS Secret or external-secret
integration, waypoint configuration when used, and required authorization.
The controller does not copy credentials or application workloads.

## Child resource model

The controller stages one child Gateway manifest on the Fleet hub with a
deterministic name derived from the source Gateway UID, for example
`store-g7k2m`. A companion Fleet `ResourcePlacement` propagates the same
manifest to every selected member. It uses the source namespace and adds
ownership metadata:

```yaml
metadata:
  labels:
    gateway.glb.azure.io/managed: "true"
    gateway.glb.azure.io/source-uid: <source-gateway-uid>
  annotations:
    gateway.glb.azure.io/source: store/store
spec:
  gatewayClassName: istio
```

The controller copies listeners, `allowedRoutes`, TLS mode/options, and
regional infrastructure customization from the source, with these exceptions:

- `spec.addresses` is not copied. It describes the global frontend on the
  source and must not force all child Gateways to request that address.
- Status is never copied.
- Finalizers and ownership references are not copied across clusters.
- The controller adds the deterministic regional Public IP request under
  `spec.infrastructure.annotations`, using
  `service.beta.kubernetes.io/azure-pip-name`.
- TLS `certificateRefs` are copied, but the referenced Secret material is not.
  The Secret must exist in each member through the workload placement or an
  external certificate controller.

The companion Fleet placement uses server-side apply and partial comparison so
fields added by the managed Application Network/Istio implementation are not
overwritten. The Stage 0 spike must verify that Application Network propagates
Gateway infrastructure annotations to the generated LoadBalancer Service. If
that customization is not supported, explicit per-member frontend resource IDs
are the fallback; the controller must not guess from Azure resource names.

### Automatic route propagation

The controller always propagates routes attached to the source Gateway:

- Watch `HTTPRoute` resources attached to the source global Gateway.
- Stage a managed copy with a deterministic UID-derived name on the Fleet hub
  and propagate it to every selected member with the companion placement.
- Preserve hostnames, rules, filters, backendRefs, and matching section names.
- Replace only the parentRef that points to the global Gateway with a parentRef
  to the child Gateway.
- Do not copy route status or unrelated parentRefs.
- Require backend Services, ambient namespace labels, global Service labels,
  and cross-namespace dependencies to exist in the workload placement.

The MVP supports `HTTPRoute`. `GRPCRoute`, `TLSRoute`, `TCPRoute`, and
`UDPRoute` require explicit conformance and Azure port/protocol validation
before being added.

### Service selection and Application Network global Services

There are two independent routing decisions:

1. Azure Global Load Balancer selects a healthy regional load balancer and
   therefore an AKS cluster.
2. The regional Gateway evaluates its local copy of the `HTTPRoute` and
   resolves each backendRef.

The route continues to use a normal core Kubernetes Service backend:

```yaml
rules:
  - backendRefs:
      - name: store
        port: 8080
```

The workload placement creates the same namespaced Service in every member and
sets `istio.io/global="true"`. Application Network synchronizes discovery for
that Service across its members. The regional Gateway still resolves the local
Service identity. For each unique Service backend, the controller creates an
Istio `DestinationRule` that enables locality load balancing and outlier
detection. Istio prefers endpoints in the ingress cluster while they are
healthy. If local endpoints are not available, Application Network can select
remote endpoints. Cross-cluster requests traverse the source and destination
east-west gateways with mTLS.

The controller does not create a fixed failover region list. Istio uses the
locality metadata that Application Network supplies. This keeps the policy
valid when Fleet adds, removes, or replaces a region.

Users apply the global Gateway and HTTPRoute once on the Fleet hub. The
controller stages the regional child Gateway and renamed route copies, and
Fleet propagates them. The application workload, Service, namespace, identity,
and certificate prerequisites are also distributed by the referenced workload
placement. Users do not apply resources directly to Fleet members.

A member is eligible for the Azure global backend pool only after:

- The referenced workload placement reports `Applied=True` for that member.
- The companion Gateway placement reports `WorkSynchronized=True` and
  `Applied=True`.
- Every Service and ReferenceGrant dependency is in the referenced workload
  snapshot, each backend Service is marked global, and the namespace is
  enrolled in ambient mode.
- The selected AKS resource is a healthy member of the policy's Application
  Network and east-west connectivity has passed the deployment validation.
- Azure shows the deterministic regional Public IP attached to a regional
  Standard Load Balancer frontend with all required listener rules.
- Fleet Work status does not report an explicit Gateway or route rejection.

Fleet may report custom-resource availability as `WorkNotTrackable`; Azure
frontend validation is therefore the authoritative infrastructure-programming
signal. The policy exposes `RegionalRouteStatusUnknown` when Fleet cannot
surface detailed route conditions.

`ResolvedRefs=True` still proves only that the Service exists and the reference
is permitted. The important difference is that the Service's endpoint scope is
now the Application Network rather than a single cluster. When one member has
zero ready local endpoints, requests arriving at that member can be sent to
available endpoints in another member. The controller does not need to remove
the regional frontend from the global load balancer for this case.

This does not make every failure automatically recoverable. The generated
outlier policy can eject an endpoint after repeated `5xx` responses, but it
does not replace accurate startup, readiness, and liveness probes. Retry and
timeout behavior remain application or platform policy.

The recommended resilient edge-ingress model is:

```text
Azure global LB -> regional Application Network Gateway -> global Service
                -> preferred local endpoint
                -> east-west gateway -> remote endpoint when local is unavailable
```

The Multicluster Services API is not required for this MVP. Application Network
uses its managed multicluster discovery and the `istio.io/global` Service label
instead of requiring `ServiceExport` and `ServiceImport` objects. MCS remains a
future portability option for regional Gateway implementations that document
Gateway API `ServiceImport` backend support.

## Regional endpoint discovery

Gateway API deliberately does not standardize the Azure resource ID of the
infrastructure created by a Gateway controller. Endpoint discovery is therefore
an adapter interface:

```go
type RegionalEndpointResolver interface {
    Resolve(ctx context.Context, member FleetMember, gateway Gateway) (RegionalEndpoint, error)
}

type RegionalEndpoint struct {
    PublicIPAddress          netip.Addr
    FrontendIPConfigurationID string
    Ports                    []int32
    IPVersion                string
}
```

Resolution order:

1. Use the class-specific deterministic-public-IP adapter. The first adapter is
   Application Network `istio`.
2. Use an explicit frontend resource ID override for a Fleet member when the
   regional implementation cannot consume a controller-created Public IP.
3. Set a clear `EndpointNotResolved` condition rather than guessing.

The Application Network `istio` adapter performs the following discovery:

1. Resolve the selected Fleet member's AKS resource ID, Azure region, and node
   resource group.
2. Create or adopt only an owned Standard regional Public IP with the stable
   name `glb-<gateway-uid-hash>` in that node resource group.
3. Configure the staged child Gateway's infrastructure annotations so its
   generated Service requests that Public IP by name and sets
   `service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip=true`.
   Chained Global-to-regional Load Balancer forwarding timed out while the
   regional rule used floating IP; disabling it makes Azure program the rule's
   backend port as the Service NodePort and makes the global VIP usable.
4. Wait for the companion Fleet placement to report synchronized and applied
   for the member.
5. Query ARM until a regional Standard Load Balancer frontend references the
   exact owned Public IP resource ID.
6. Verify a regional load-balancing rule and health probe exist for every
   unique global listener port.
7. TCP-probe every listener through the regional public IP before adding that
   frontend to the Global Load Balancer backend pool.

Names alone are never sufficient proof of ownership. Discovery correlates the
Fleet member ARM ID, owned Public IP resource ID, regional load balancer
frontend reference, and matching load-balancing rules.

## Azure resource model

The Azure reconciler creates, per global Gateway:

- One owned Standard regional Public IP with the same deterministic name in
  each selected Fleet member's AKS node resource group.
- One Standard SKU, Global tier Public IP address.
- One Standard SKU, Global tier Azure Load Balancer.
- One frontend IP configuration using the global Public IP.
- One backend pool containing a backend address for each selected regional
  load balancer frontend IP configuration.
- One TCP load-balancing rule for each unique listener port.

HTTP, HTTPS, and TLS listeners are TCP at the global layer. TLS termination
continues to happen at the regional proxy. A global rule is created only after
all of its enrolled regional endpoints expose a matching frontend port.

Azure resources receive tags containing the Kubernetes cluster identity,
namespace, Gateway name, Gateway UID, and controller version. ARM updates use
ETags and deterministic child names so retries converge rather than duplicate
resources.

The controller adds a backend before removing an old backend during a cluster
or endpoint transition. A cluster is removed from the Azure pool before its
child route and Gateway resources are deleted.

### Health and failover semantics

Azure Global Load Balancer evaluates availability represented by the regional
load balancers. Regional health probes are owned by the regional Gateway
implementation. The global controller validates that applicable regional rules
and probes exist but does not replace them.

Consequences:

- A regional frontend that does not accept TCP connections on every listener
  is excluded from the desired global pool and reports `Reachable=False`.
- Loss of ready local Service endpoints does not require GLB removal: the
  regional proxy can use Application Network to reach ready remote endpoints.
- The generated outlier policy ejects endpoints after repeated `5xx` responses.
  Readiness probes remain the primary health signal.
- Cross-cluster fallback requires healthy east-west gateways and network
  reachability; it can add inter-region latency and data-transfer cost.
- Failover applies to new flows. Existing TCP connections can be interrupted.

### Failure detection and expected timing

The design has two failure units: Azure handles regional ingress
infrastructure, while Application Network handles Service endpoint selection.

| Failure | Removed from global rotation? | Expected behavior |
|---|---|---|
| Service does not exist before enrollment | Yes; it is never enrolled | The backend dependency cannot be proven in the applied workload placement snapshot. |
| Existing Service is deleted from Fleet placement | Eventually, through controller reconciliation | Fleet publishes a new snapshot/apply state, then the controller removes the Azure backend. This is a control-plane operation with no sub-second or fixed failover guarantee. |
| One member's Service has zero ready local endpoints | No; removal is unnecessary | Application Network routes to available endpoints in another member through the east-west data plane. In the validated two-region demo, east had zero pods and zero ready endpoints, its regional ingress returned a response from west, and the global VIP returned 30/30 successful responses from west. This measurement is not an SLA. |
| All members have zero ready endpoints | No | No healthy backend exists; the regional gateways can return `503` even though the ingress frontends remain healthy. |
| Pods remain Ready but return repeated `5xx` responses | No | The generated outlier policy can eject the endpoint after five consecutive errors. Fix the readiness probe because ejection is temporary. |
| East-west path fails while the selected region has no local endpoints | No | Cross-cluster fallback fails; surface Application Network degradation. |
| Regional gateway/Azure LB backend becomes unhealthy | Yes | With typical AKS defaults of a 5-second probe interval and two failed probes, regional detection is about 10 seconds. The global load balancer samples regional availability every 5 seconds, so new-flow failover is generally expected on the order of 10-20 seconds, not as a formal SLA. |
| Regional load balancer availability is already zero | Yes | The global layer's next 5-second availability check can remove it from rotation. |

Removing a regional frontend from the global backend pool by watching
EndpointSlices is deliberately avoided. It would turn ARM reconciliation into
a health loop and could remove every healthy route sharing that Gateway because
one Service is unavailable. Application Network keeps endpoint selection in
the data plane, where it belongs.

## Reconciliation

### 1. GatewayClass installation

The installation applies the static `global-istio` GatewayClass. The controller
watches `GlobalGatewayPolicy` directly and indexes policies by their target
Gateway. Source Gateway changes and attached HTTPRoute changes enqueue the
affected policy immediately. It does not run a separate GatewayClass reconciler
or write GatewayClass status.

### 2. Fleet placement reconciler

1. Resolve the required workload `ClusterResourcePlacement` or
   `ResourcePlacement` reference.
2. Read its observed resource snapshot and per-member placement status.
3. Select only joined members for which the workload placement is applied.
4. Resolve each selected member's AKS resource ID, region, and node resource
   group from Fleet metadata.
5. Validate the ambient namespace label, global Service labels, and required
   waypoint resources in the workload placement snapshot.
6. Stage the child Gateway, route copies, and local-first `DestinationRule`
   resources on the Fleet hub.
7. Reconcile a companion `ResourcePlacement` using `PickFixed` with the exact
   selected member names.
8. Publish Fleet scheduling, synchronization, apply, availability, drift, and
   Azure-resolution conditions.
9. When the workload placement changes, keep old members long enough to add
   ready replacements where Fleet rollout ordering permits.

### 3. Global Gateway reconciler

1. Resolve the global GatewayClass and its regional class.
2. Resolve exactly one attached `GlobalGatewayPolicy`.
3. Validate listener protocols, ports, TLS references, Fleet selection, global
   Service configuration, and Azure placement.
4. Stage or patch the child Gateway and attached route copies on the Fleet hub.
5. Reconcile the companion Fleet placement to the selected members.
6. Wait for Fleet synchronization/application and validate the deterministic
   regional Public IP, load balancer frontend, listener rules, and probes.
7. Resolve and validate each eligible regional Azure frontend.
8. Reconcile the global Public IP, load balancer, backend pool, and rules.
9. Aggregate the global address, Azure IDs, Fleet snapshot, conditions, and
   per-member reachability onto `GlobalGatewayPolicy.status`.

The implementation is level-triggered. Indexed Gateway watches and HTTPRoute
parent-reference watches react to source intent changes without waiting for the
one-minute resync. Status-only Gateway and HTTPRoute updates are ignored to
avoid loops. The resync remains a backstop for missed events and external Azure
or Fleet drift. Work is keyed by source Gateway UID, and per-Gateway Azure
operations are serialized.

### 4. Deletion reconciler

A finalizer on `GlobalGatewayPolicy` prevents orphaning owned Azure resources.
Deletion proceeds in this order:

1. Stop accepting new desired backends.
2. Remove regional frontend references and global rules.
3. Delete or retain the global load balancer and Public IP according to policy.
4. Delete the companion Fleet placement and staged child resources, allowing
   Fleet to remove them from members.
5. Remove the policy finalizer.

If a Fleet member is unreachable, Fleet placement status reports the
synchronization or cleanup failure. A force-finalize workflow is not included
in the current MVP.

## Status and events

The source Gateway exposes the global IP through standard
`Gateway.status.addresses` and publishes `Accepted` and `Programmed`
conditions. `GlobalGatewayPolicy.status` contains the same global IP plus the
load balancer resource ID, Fleet snapshot, per-member frontend IDs, and
`Reachable` conditions. It also contains these policy condition types:

- `Accepted`
- `MembersReady`
- `AzureResourcesReady`
- `RoutesPropagated`

The current MVP does not emit custom Kubernetes Events or expose custom
Prometheus metrics. Those are hardening items; reconciliation errors and state
transitions are available in structured controller logs, Gateway conditions,
and policy conditions.

## Ownership and conflict handling

- Child Gateway, route, and placement names are deterministic and include the
  source Gateway UID suffix.
- Kubernetes staging resources use server-side apply with one controller field
  manager and owner references to the policy.
- Azure resources carry ownership tags for traceability, and unchanged public
  IPs/backend pools are not rewritten on every reconciliation.
- Strict rejection of pre-existing unowned Kubernetes or Azure name collisions
  is not implemented yet. The demo therefore uses a dedicated resource group;
  production hardening must add ownership checks before update or deletion.

## Security model

### Kubernetes access

The controller's hub RBAC includes Gateway API resources, Fleet
`MemberCluster`, `ClusterResourcePlacement`, `ResourcePlacement`, leader
election Leases, the `GlobalGatewayPolicy` CRD, and Event creation permission.

The controller has no member-cluster kubeconfig Secrets and no direct member
API write path. Fleet member agents apply the staged resources using Fleet's
existing authorization model. The controller does not read application
Secrets.

### Azure access

The automated demo uses a projected ServiceAccount token and an Entra federated
credential. The controller runs in the east member cluster and uses that
cluster's OIDC issuer. The deployment declares the token volume and identity
environment variables directly. `DefaultAzureCredential` reads this token. No
client secret or certificate is stored in the cluster. The controller needs
Network Contributor on the global networking resource group. It also needs
permission to manage the owned Public IP in each member node resource group. It
needs read access to the selected clusters and regional load balancers.
Cross-subscription members require the
corresponding permissions in each subscription. Before production, these
permissions should be reduced to a custom role containing only the Public IP,
load balancer, backend pool, join, and read actions the implementation calls.

The policy API should be constrained by admission or controller configuration
to an allowlist of subscriptions and resource groups. A namespace user who can
create a Gateway must not be able to make the controller mutate arbitrary Azure
resources.

### Secret and certificate handling

The controller never reads or copies TLS private key material. The workload
placement or an external certificate controller must provision the referenced
TLS Secret independently in every selected member. A Key Vault CSI or external
secrets integration may be used, but it is not owned by this controller.

## Observability

Recommended metrics include:

- Reconcile duration and errors by controller and reason.
- Selected, ready, degraded, and unreachable clusters per Gateway.
- Child Gateway and copied route readiness.
- Regional endpoint discovery duration and failures.
- ARM operation duration, throttling, conflicts, and provisioning failures.
- Global Gateways by condition and listener protocol.
- Time from source generation to globally programmed.

Logs carry source Gateway UID, policy, Fleet placement snapshot, member cluster,
child object, Azure correlation ID, and ARM resource ID. Credentials and
listener TLS options that might contain sensitive values are redacted.

## MVP scope

The first usable release includes:

- One AKS Fleet with a hub cluster and at least two joined traffic-serving AKS
  member clusters.
- A required workload `ClusterResourcePlacement` or `ResourcePlacement` that
  defines the exact member set for each global Gateway.
- One Azure Kubernetes Application Network with every selected Fleet cluster
  joined as a member and east-west reachability configured.
- `global-istio` mapped to Application Network's `istio` GatewayClass.
- Public IPv4.
- HTTP and HTTPS listeners on TCP ports, initially tested on 80 and 443.
- Automatic propagation of attached `HTTPRoute` resources.
- Automatic local-first `DestinationRule` generation for each unique Service
  backend, with remote endpoint fallback and no fixed region list.
- Core `Service` backends marked `istio.io/global="true"`, with application
  namespaces enrolled in ambient mode and documented waypoint configuration.
- Cross-cluster endpoint fallback when one member has no ready local endpoints.
- Dedicated Azure Global Load Balancer and global Public IP per Gateway.
- Regional frontend discovery for Application Network Istio gateways.
- Deterministic controller-owned regional Public IPs configured through the
  child Gateway infrastructure annotations.
- Non-floating regional Azure load-balancing rules for compatibility with the
  chained Global Load Balancer path.
- TCP reachability gating before a regional frontend enters the global pool.
- Regional TLS termination using standard Gateway API `certificateRefs`.
- Fleet-only propagation with no member-cluster kubeconfig Secrets.
- Policy conditions, structured logs, ownership tags, and policy-finalizer
  cleanup.
- A generic explicit-frontend-ID path for proving another regional
  GatewayClass without adding a class-specific adapter.

The generic `global-*` class mirroring and child resource logic should be part
of the initial architecture even though Application Network `istio` is the only
class-specific endpoint adapter required to pass the MVP release gate.

## Validation

The reproducible deployment is implemented by `hack/demo/deploy.sh` and
documented in `docs/demo.md`. The script creates the Azure resource group,
regional AKS clusters, Fleet hub and members, Application Network, ACR,
controller identity, hub deployment, backing services, global Gateway, and
HTTPRoute, then curls the published global VIP.

The validated 2026-09-30 environment proved:

- Both regional Istio Services carried the non-floating-IP annotation and
  their Azure rules reported `enableFloatingIP=false`.
- With both workloads healthy, the east ingress returned east for 40/40
  requests and the west ingress returned west for 40/40 requests.
- The global VIP returned one selected region for 40/40 requests from the
  validation client.
- The global VIP served the region-labelled echo workload.
- With east scaled to zero pods and zero ready EndpointSlice endpoints, the
  east ingress reached west through Application Network for 30/30 requests.
- The global VIP also returned 30/30 successful requests during that steady
  regional workload outage.

### Unit and envtest

- Prefix parsing, explicit class annotation, maximum-name behavior, and class
  ownership conflicts.
- Selection from Fleet placement snapshots, member apply status, labels,
  properties, taints, and tolerations.
- Companion `ResourcePlacement` generation with exact `PickFixed` membership.
- Child Gateway projection, especially omission of global addresses,
  infrastructure annotations, and TLS references.
- ParentRef rewriting and route copy ownership.
- Ambient namespace labels, global Service labels, waypoint configuration, and
  missing route dependencies.
- Listener-to-Azure-rule projection and duplicate-port handling.
- Status aggregation for zero, partial, and all clusters ready.
- Finalizer ordering and retain/delete policies.
- ARM idempotency, ETag conflict retry, and ownership-tag protection using a
  fake client.

### End-to-end MVP

1. Create two AKS clusters in different Azure regions with Managed Gateway API
   enabled, join both to one Application Network, and configure external
   east-west gateways or another supported network path.
2. Create an AKS Fleet with a hub and join both clusters as members.
3. Stage the test workload and global Service on the hub, including ambient and
   waypoint configuration, and deploy them with a Fleet placement.
4. Apply a global Gateway, policy referencing that placement, and HTTPRoute.
5. Verify each child Gateway uses `istio` and becomes Programmed.
6. Verify the companion Fleet placement is synchronized/applied, the
   deterministic regional Public IPs are attached, and the Azure global backend
   pool references both regional load balancer frontend resource IDs.
7. Verify `Gateway.status.addresses` and
   `GlobalGatewayPolicy.status.globalAddress` report the global public IP and
   that it serves HTTP through the regional proxies.
8. Scale the Service workload to zero in one member and verify requests entering
   that member reach healthy remote endpoints through Application Network.
9. Make one regional proxy unavailable and verify new connections fail over at
   the Azure global layer; restore it and rotate the regional TLS Secret.
10. Remove one member from the workload placement and verify the Azure backend
    is removed before Fleet child cleanup.
11. Delete the source resources and verify that owned Azure and Kubernetes
    resources are removed without touching application Services or workloads.

Negative tests cover an internal child Gateway, a missing regional class,
missing listener port, Fleet scheduling or apply failure, `WorkNotTrackable`,
route rejection when surfaced, unreachable Fleet member, insufficient ARM
permissions, and an unowned Azure name collision.

## Delivery stages

### Stage 0: Application Network and Azure GLB spike

- Prove the Application Network `istio` Gateway's generated Service behavior
  and support for `spec.infrastructure.annotations`.
- Prove a controller-owned public IP can be bound to each generated Gateway
  Service and resolved to the regional load balancer frontend through ARM.
- Prove a request entering cluster A reaches cluster B when A has no ready
  local endpoints for an `istio.io/global="true"` Service.
- Attach two AKS regional frontends to a hand-created Azure Global Load
  Balancer and validate failover on ports 80 and 443.
- Confirm cross-subscription behavior if it is required for the first release.

### Stage 1: Fleet hub and one-member path

- GatewayClass installation, policy validation, Fleet placement observation,
  staged child resources, deterministic regional Public IP, ARM reconciliation,
  status, and deletion against one Fleet member.

### Stage 2: Fleet multicluster

- Multiple Fleet members, route propagation, companion placements,
  per-member status, placement snapshot changes, add-before-remove backend
  rollout, and failure isolation.

### Stage 3: Hardening

- Workload Identity, least-privilege roles, admission allowlists, metrics,
  scale tests, throttling behavior, upgrade compatibility, and recovery
  tooling.

### Stage 4: Broader regional Gateway support

- Validate the generic Service resolver.
- Add adapters only where implementations do not expose enough standard
  metadata.
- Add more route kinds based on Gateway API conformance and Azure protocol
  support.

## Alternatives considered

### Azure Front Door

Front Door is a global Layer 7 service and can use public endpoints as origins.
It provides HTTP-aware health and routing, but it would duplicate or move part
of the Gateway API Layer 7 behavior out of the regional Gateway and introduces
a different TLS, hostname, and route projection model. It is a reasonable
future provider mode, not the Azure Global Load Balancer MVP described here.

### Azure Traffic Manager

Traffic Manager provides DNS-based distribution and can target regional public
IPs, but clients and recursive resolvers cache DNS results. It does not provide
the requested single global load balancer frontend or connection-level proxying.

### Standalone cluster inventory and kubeconfigs

A custom cluster registry would duplicate Fleet membership, placement,
rollout, taints, drift detection, and per-member apply status while introducing
long-lived credential lifecycle. The controller therefore requires a Fleet hub
and has no standalone inventory or direct-member mode.

### One independent global controller per cluster

Peer controllers would require leader election or another coordination system
across clusters to avoid concurrent Azure writes and inconsistent membership.
A single Fleet-hub controller has a simpler ownership and status model.

### Sharing one global load balancer

Sharing reduces Azure resources but creates cross-namespace port conflicts,
larger failure domains, complicated ownership, and deletion hazards. A
dedicated global load balancer per Gateway is the safer MVP. Pooling can be
added later behind an explicit class or policy.

## Key risks

- Application Network is Preview and its APIs, supported regions, or behavior
  may change; it is not a production-SLA dependency today.
- The managed Istio Gateway's generated Service metadata may change. Discovery
  must validate resource relationships across supported Application Network
  versions.
- Cross-cluster endpoint fallback depends on accurate readiness and healthy
  east-west connectivity; ready-but-broken pods can still fail requests.
- Fleet can report custom resources as `WorkNotTrackable`; Azure resource
  validation must remain the authoritative regional infrastructure signal, and
  detailed route status may be unavailable without future Fleet feedback.
- Regional Public IP creation in AKS node resource groups requires explicit
  cross-subscription permissions and careful ownership tagging.
- Different regional Gateway controllers can mutate Gateway spec in different
  ways. Field ownership tests are required before declaring a class supported.
- ARM eventual consistency and throttling can make an otherwise healthy
  Gateway take minutes to become globally programmed. Status must distinguish
  progressing from failed.
- A global public IP or load balancer name collision is security-sensitive;
  unowned Azure resources must never be adopted implicitly.

## References

- [Azure Kubernetes Application Network overview](https://learn.microsoft.com/en-us/azure/application-network/overview)
- [Azure Kubernetes Application Network architecture](https://learn.microsoft.com/en-us/azure/application-network/architecture)
- [Application Network traffic management and ingress Gateway](https://learn.microsoft.com/en-us/azure/application-network/traffic-management-use-cases#deploy-the-ingress-gateway)
- [Create and join an Azure Kubernetes Application Network](https://learn.microsoft.com/en-us/azure/application-network/get-started)
- [Azure Global Load Balancer overview](https://learn.microsoft.com/en-us/azure/load-balancer/cross-region-overview)
- [Azure Load Balancer health probes](https://learn.microsoft.com/en-us/azure/load-balancer/load-balancer-custom-probe-overview)
- [Cross-subscription Azure Load Balancer](https://learn.microsoft.com/en-us/azure/load-balancer/cross-subscription-overview)
- [AKS public Standard Load Balancer](https://learn.microsoft.com/en-us/azure/aks/load-balancer-standard)
- [Azure Kubernetes Fleet Manager overview](https://learn.microsoft.com/en-us/azure/kubernetes-fleet/overview)
- [Fleet cluster resource placement](https://learn.microsoft.com/en-us/azure/kubernetes-fleet/quickstart-resource-propagation)
- [Fleet multicluster networking and DNS load balancing](https://learn.microsoft.com/en-us/azure/kubernetes-fleet/concepts-multi-cluster-networking-overview)
- [Multicluster Services API](https://multicluster.sigs.k8s.io/concepts/multicluster-services-api/)
- [Gateway API interaction with MCS](https://gateway-api.sigs.k8s.io/geps/gep-1748/)
- [Kubernetes Gateway API: GatewayClass](https://gateway-api.sigs.k8s.io/api-types/gatewayclass/)
- [Kubernetes Gateway API: Gateway](https://gateway-api.sigs.k8s.io/api-types/gateway/)
- [Kubernetes Gateway API: HTTPRoute](https://gateway-api.sigs.k8s.io/api-types/httproute/)
- [Kubernetes EndpointSlices](https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/)
