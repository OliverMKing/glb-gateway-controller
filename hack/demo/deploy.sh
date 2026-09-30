#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

# Small logging and validation helpers keep the main flow readable.
log() {
  printf '\n[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

# Pin preview extensions whose API contract is part of this reproducible demo.
ensure_extension_version() {
  local name="$1"
  local version="$2"
  local installed
  installed="$(az extension show --name "$name" --query version -o tsv 2>/dev/null || true)"
  if [[ "$installed" == "$version" ]]; then
    return 0
  fi
  if [[ -n "$installed" ]]; then
    az extension remove --name "$name"
  fi
  az extension add --name "$name" --version "$version" --yes -o none
}

# Azure feature registration is asynchronous and must finish before AppNet use.
wait_for_feature() {
  local namespace="$1"
  local feature="$2"
  local state
  for _ in $(seq 1 120); do
    state="$(az feature show --namespace "$namespace" --name "$feature" --query properties.state -o tsv 2>/dev/null || true)"
    if [[ "$state" == "Registered" ]]; then
      return 0
    fi
    printf 'waiting for feature %s/%s; state=%s\n' "$namespace" "$feature" "${state:-unknown}"
    sleep 15
  done
  die "feature $namespace/$feature did not become Registered"
}

# Role creation is idempotent; an existing assignment is success.
ensure_role_assignment() {
  local principal_id="$1"
  local role="$2"
  local scope="$3"
  local principal_type="${4:-ServicePrincipal}"
  local output
  if output="$(az role assignment create \
      --assignee-object-id "$principal_id" \
      --assignee-principal-type "$principal_type" \
      --role "$role" \
      --scope "$scope" \
      -o none 2>&1)"; then
    return 0
  fi
  if [[ "$output" == *"RoleAssignmentExists"* ]]; then
    return 0
  fi
  printf '%s\n' "$output" >&2
  return 1
}

# Fleet member creation is safe to rerun after a partial deployment.
ensure_fleet_member() {
  local name="$1"
  local cluster_id="$2"
  local labels="$3"
  if az fleet member show \
      --resource-group "$RESOURCE_GROUP" \
      --fleet-name "$FLEET_NAME" \
      --name "$name" \
      -o none 2>/dev/null; then
    log "Fleet member $name already exists"
    return 0
  fi
  az fleet member create \
    --resource-group "$RESOURCE_GROUP" \
    --fleet-name "$FLEET_NAME" \
    --name "$name" \
    --member-cluster-id "$cluster_id" \
    --member-labels "$labels" \
    -o none
}

# Start missing AKS clusters in parallel, then wait for both later.
ensure_aks_cluster() {
  local name="$1"
  local location="$2"
  if az aks show --resource-group "$RESOURCE_GROUP" --name "$name" -o none 2>/dev/null; then
    log "AKS cluster $name already exists"
    return 0
  fi
  log "Starting AKS cluster $name in $location"
  az aks create \
    --resource-group "$RESOURCE_GROUP" \
    --name "$name" \
    --location "$location" \
    --node-count "$NODE_COUNT" \
    --enable-aad \
    --enable-oidc-issuer \
    --enable-gateway-api \
    --generate-ssh-keys \
    --no-wait
}

# Wait until the public east-west gateway can carry cross-cluster traffic.
wait_for_external_east_west_gateway() {
  local kubeconfig="$1"
  local member_name="$2"
  local address programmed
  for _ in $(seq 1 180); do
    address="$(KUBECONFIG="$kubeconfig" kubectl -n applink-system get gateway istio-eastwestgateway \
      -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)"
    programmed="$(KUBECONFIG="$kubeconfig" kubectl -n applink-system get gateway istio-eastwestgateway \
      -o jsonpath='{range .status.conditions[?(@.type=="Programmed")]}{.status}{end}' 2>/dev/null || true)"
    printf 'Application Network member %s east-west address=%s programmed=%s\n' \
      "$member_name" "${address:-pending}" "${programmed:-pending}"
    if [[ "$programmed" == "True" && -n "$address" ]] && \
       curl --fail --silent --show-error --connect-timeout 5 --max-time 10 \
         "http://$address:15021/healthz/ready" >/dev/null 2>&1; then
      return 0
    fi
    sleep 20
  done
  KUBECONFIG="$kubeconfig" kubectl -n applink-system describe gateway istio-eastwestgateway || true
  die "Application Network east-west gateway for $member_name did not become externally reachable"
}

# Join an existing cluster to Application Network and wait for its data plane.
ensure_appnet_member() {
  local member_name="$1"
  local cluster_id="$2"
  local member_location="$3"
  local kubeconfig="$4"
  local output state gateway_visibility joined=false
  if az appnet member show \
      --resource-group "$RESOURCE_GROUP" \
      --appnet-name "$APPNET_NAME" \
      --member-name "$member_name" \
      -o none 2>/dev/null; then
    log "Application Network member $member_name already exists"
    gateway_visibility="$(az appnet member show \
      --resource-group "$RESOURCE_GROUP" \
      --appnet-name "$APPNET_NAME" \
      --member-name "$member_name" \
      --query properties.connectivityProfile.eastWestGateway.visibility -o tsv)"
    if [[ "$gateway_visibility" != "External" ]]; then
      log "Making the $member_name east-west gateway externally reachable"
      az appnet member update \
        --resource-group "$RESOURCE_GROUP" \
        --appnet-name "$APPNET_NAME" \
        --member-name "$member_name" \
        --east-west-gateway External \
        --no-wait \
        -o none
    fi
    joined=true
  else
    log "Joining $member_name to Application Network"
    # Newly-created preview resources can briefly reject an otherwise valid join.
    for _ in $(seq 1 12); do
      if output="$(az appnet member join \
          --resource-group "$RESOURCE_GROUP" \
          --appnet-name "$APPNET_NAME" \
          --member-name "$member_name" \
          --member-resource-id "$cluster_id" \
          --member-location "$member_location" \
          --east-west-gateway External \
          --upgrade-mode FullyManaged \
          --release-channel Stable \
          --no-wait 2>&1)"; then
        joined=true
        break
      fi
      if [[ "$output" != *"ResourceCreationValidateFailed"* ]]; then
        printf '%s\n' "$output" >&2
        return 1
      fi
      printf 'Application Network validation is not ready for %s; retrying\n' "$member_name"
      sleep 30
    done
  fi
  [[ "$joined" == true ]] || die "Application Network member $member_name was not accepted"

  # The preview RP can briefly report Failed while the external gateway rolls out.
  for _ in $(seq 1 180); do
    state="$(az appnet member show \
      --resource-group "$RESOURCE_GROUP" \
      --appnet-name "$APPNET_NAME" \
      --member-name "$member_name" \
      --query properties.provisioningState -o tsv 2>/dev/null || true)"
    gateway_visibility="$(az appnet member show \
      --resource-group "$RESOURCE_GROUP" \
      --appnet-name "$APPNET_NAME" \
      --member-name "$member_name" \
      --query properties.connectivityProfile.eastWestGateway.visibility -o tsv 2>/dev/null || true)"
    printf 'Application Network member %s state=%s east-west=%s\n' \
      "$member_name" "${state:-pending}" "${gateway_visibility:-pending}"
    if [[ "$gateway_visibility" == "External" ]]; then
      wait_for_external_east_west_gateway "$kubeconfig" "$member_name"
      return 0
    fi
    case "$state" in
      Failed|Canceled|Cancelled)
        az appnet member show \
          --resource-group "$RESOURCE_GROUP" \
          --appnet-name "$APPNET_NAME" \
          --member-name "$member_name" -o yaml || true
        die "Application Network member $member_name entered state $state"
        ;;
    esac
    sleep 20
  done
  die "Application Network member $member_name did not finish provisioning"
}

# Application Network supplies these classes after member installation.
wait_for_gateway_class() {
  local kubeconfig="$1"
  local cluster_name="$2"
  for _ in $(seq 1 120); do
    if KUBECONFIG="$kubeconfig" kubectl get gatewayclass istio -o name >/dev/null 2>&1 && \
       KUBECONFIG="$kubeconfig" kubectl get gatewayclass istio-waypoint -o name >/dev/null 2>&1; then
      return 0
    fi
    printf 'waiting for Application Network GatewayClasses in %s\n' "$cluster_name"
    sleep 15
  done
  die "Application Network GatewayClasses did not appear in $cluster_name"
}

# Fleet rolls the workload through the two members sequentially.
wait_for_regional_workloads() {
  local east_ready west_ready east_region_value west_region_value
  for _ in $(seq 1 240); do
    east_ready="$(KUBECONFIG="$EAST_KUBECONFIG" kubectl -n global-demo get deployment global-demo -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    west_ready="$(KUBECONFIG="$WEST_KUBECONFIG" kubectl -n global-demo get deployment global-demo -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    east_region_value="$(KUBECONFIG="$EAST_KUBECONFIG" kubectl -n global-demo get deployment global-demo -o jsonpath='{.spec.template.spec.containers[0].env[0].value}' 2>/dev/null || true)"
    west_region_value="$(KUBECONFIG="$WEST_KUBECONFIG" kubectl -n global-demo get deployment global-demo -o jsonpath='{.spec.template.spec.containers[0].env[0].value}' 2>/dev/null || true)"
    printf 'workloads east=%s/%s west=%s/%s\n' \
      "${east_ready:-0}" "${east_region_value:-pending}" \
      "${west_ready:-0}" "${west_region_value:-pending}"
    if [[ "$east_ready" == "2" && "$west_ready" == "2" && \
          "$east_region_value" == "$EAST_REGION" && "$west_region_value" == "$WEST_REGION" ]]; then
      return 0
    fi
    sleep 15
  done
  die "regional demo workloads did not become ready"
}

# The Gateway publishes the VIP after the controller programs Azure.
wait_for_global_address() {
  local address programmed accepted azure_ready members_ready
  for _ in $(seq 1 240); do
    address="$(KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get gateway global-demo -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)"
    programmed="$(KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get gateway global-demo -o json 2>/dev/null | jq -r '[.status.conditions[]? | select(.type=="Programmed") | .status] | last // "False"' || true)"
    accepted="$(KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get globalgatewaypolicy global-demo -o json 2>/dev/null | jq -r '[.status.conditions[]? | select(.type=="Accepted") | .status] | last // "False"' || true)"
    azure_ready="$(KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get globalgatewaypolicy global-demo -o json 2>/dev/null | jq -r '[.status.conditions[]? | select(.type=="AzureResourcesReady") | .status] | last // "False"' || true)"
    members_ready="$(KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get globalgatewaypolicy global-demo -o json 2>/dev/null | jq -r '[.status.conditions[]? | select(.type=="MembersReady") | .status] | last // "False"' || true)"
    printf 'global gateway address=%s programmed=%s accepted=%s azure=%s members=%s\n' \
      "${address:-pending}" "$programmed" "$accepted" "$azure_ready" "$members_ready" >&2
    if [[ -n "$address" && "$programmed" == "True" && "$accepted" == "True" && \
          "$azure_ready" == "True" && "$members_ready" == "True" ]]; then
      printf '%s' "$address"
      return 0
    fi
    sleep 15
  done
  KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo describe globalgatewaypolicy global-demo || true
  die "global address did not become ready"
}

# Confirm that each healthy regional ingress uses its local application pods.
wait_for_local_first() {
  local member_name="$1"
  local expected_region="$2"
  local public_ip_id address response local_count
  for _ in $(seq 1 120); do
    public_ip_id="$(KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get \
      globalgatewaypolicy global-demo -o json | jq -r \
      --arg member "$member_name" '.status.members[]? | select(.name == $member) | .regionalPublicIPAddressID' 2>/dev/null || true)"
    address=""
    if [[ -n "$public_ip_id" ]]; then
      address="$(az network public-ip show --ids "$public_ip_id" --query ipAddress -o tsv 2>/dev/null || true)"
    fi

    local_count=0
    if [[ -n "$address" ]]; then
      for _ in $(seq 1 10); do
        response="$(curl --silent --show-error --connect-timeout 5 --max-time 10 \
          "http://$address" 2>/dev/null || true)"
        if [[ "$response" != "hello from $expected_region" ]]; then
          break
        fi
        local_count=$((local_count + 1))
      done
    fi

    printf 'regional locality member=%s address=%s local-responses=%s/10\n' \
      "$member_name" "${address:-pending}" "$local_count"
    if [[ "$local_count" == "10" ]]; then
      return 0
    fi
    sleep 10
  done
  die "regional frontend for $member_name did not prefer $expected_region"
}

# Keep one upstream in every pool, including the ingress's one-waypoint pool.
wait_for_safe_outlier_policy() {
  local hub_policy east_policy west_policy
  for _ in $(seq 1 120); do
    hub_policy="$(KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo get destinationrules -o json 2>/dev/null | jq -r '[.items[0].spec.trafficPolicy.outlierDetection.maxEjectionPercent, (.items[0].spec.workloadSelector == null)] | @tsv' || true)"
    east_policy="$(KUBECONFIG="$EAST_KUBECONFIG" kubectl -n global-demo get destinationrules -o json 2>/dev/null | jq -r '[.items[0].spec.trafficPolicy.outlierDetection.maxEjectionPercent, (.items[0].spec.workloadSelector == null)] | @tsv' || true)"
    west_policy="$(KUBECONFIG="$WEST_KUBECONFIG" kubectl -n global-demo get destinationrules -o json 2>/dev/null | jq -r '[.items[0].spec.trafficPolicy.outlierDetection.maxEjectionPercent, (.items[0].spec.workloadSelector == null)] | @tsv' || true)"
    printf 'safe outlier policy hub=%q east=%q west=%q\n' \
      "${hub_policy:-pending}" "${east_policy:-pending}" "${west_policy:-pending}"
    if [[ "$hub_policy" == $'99\ttrue' && "$east_policy" == $'99\ttrue' && "$west_policy" == $'99\ttrue' ]]; then
      return 0
    fi
    sleep 10
  done
  die "locality policy did not preserve one healthy upstream"
}

require_command az
require_command curl
require_command kubectl
require_command jq
require_command sed

# All names and locations can be overridden with environment variables.
SUBSCRIPTION_ID="${SUBSCRIPTION_ID:-$(az account show --query id -o tsv)}"
RESOURCE_GROUP="${RESOURCE_GROUP:-kingoliver-glb-gateway-controller}"
FLEET_NAME="${FLEET_NAME:-glb-fleet}"
EAST_CLUSTER="${EAST_CLUSTER:-glb-east}"
WEST_CLUSTER="${WEST_CLUSTER:-glb-west}"
EAST_MEMBER="${EAST_MEMBER:-east}"
WEST_MEMBER="${WEST_MEMBER:-west}"
EAST_REGION="${EAST_REGION:-eastus2}"
WEST_REGION="${WEST_REGION:-westus3}"
APPNET_REGION="${APPNET_REGION:-westus2}"
APPNET_NAME="${APPNET_NAME:-glb-appnet-wus2}"
NODE_COUNT="${NODE_COUNT:-2}"
HUB_KUBECONFIG="${HUB_KUBECONFIG:-/tmp/${FLEET_NAME}-kubeconfig}"
EAST_KUBECONFIG="${EAST_KUBECONFIG:-/tmp/${EAST_CLUSTER}-kubeconfig}"
WEST_KUBECONFIG="${WEST_KUBECONFIG:-/tmp/${WEST_CLUSTER}-kubeconfig}"
# Derive a deterministic, Azure-valid ACR name unless the caller supplies one.
ACR_SUFFIX="$(printf '%s' "$SUBSCRIPTION_ID" | tr -d '-' | cut -c1-8)"
ACR_RG_TOKEN="$(printf '%s' "$RESOURCE_GROUP" | tr '[:upper:]' '[:lower:]' | tr -cd '[:alnum:]' | cut -c1-30)"
ACR_NAME="${ACR_NAME:-${ACR_RG_TOKEN}acr${ACR_SUFFIX}}"
CONTROLLER_IMAGE_TAG="${CONTROLLER_IMAGE_TAG:-demo}"
CONTROLLER_SP_NAME="${CONTROLLER_SP_NAME:-${RESOURCE_GROUP}-controller}"
SKIP_CONTROLLER_IMAGE_BUILD="${SKIP_CONTROLLER_IMAGE_BUILD:-false}"
APPNET_EXTENSION_VERSION="${APPNET_EXTENSION_VERSION:-1.0.0b4}"
ISTIO_CRD_VERSION="${ISTIO_CRD_VERSION:-1.29.8}"
SERVICE_MANAGEMENT_REFERENCE="${SERVICE_MANAGEMENT_REFERENCE:-}"

[[ "$SKIP_CONTROLLER_IMAGE_BUILD" == "true" || "$SKIP_CONTROLLER_IMAGE_BUILD" == "false" ]] || \
  die "SKIP_CONTROLLER_IMAGE_BUILD must be true or false"

# Microsoft tenants can require new applications to reference their owning service.
if [[ -z "$SERVICE_MANAGEMENT_REFERENCE" ]]; then
  mapfile -t OWNED_SERVICE_REFERENCES < <(az ad app list --show-mine \
    --query '[?serviceManagementReference != null].serviceManagementReference' \
    -o tsv | sort -u)
  if [[ "${#OWNED_SERVICE_REFERENCES[@]}" -eq 1 ]]; then
    SERVICE_MANAGEMENT_REFERENCE="${OWNED_SERVICE_REFERENCES[0]}"
  fi
fi

az account set --subscription "$SUBSCRIPTION_ID"

# Install preview tooling and register the required Azure resource providers.
log "Installing Azure CLI extensions"
az extension add --name fleet --upgrade --yes -o none
ensure_extension_version appnet-preview "$APPNET_EXTENSION_VERSION"
az extension add --name aks-preview --upgrade --yes -o none

log "Registering Azure providers and Application Network preview"
az feature register --namespace Microsoft.AppLink --name PublicPreview -o none
wait_for_feature Microsoft.AppLink PublicPreview
az provider register --namespace Microsoft.AppLink --wait -o none
az provider register --namespace Microsoft.ContainerService --wait -o none
az provider register --namespace Microsoft.Network --wait -o none

log "Creating resource group $RESOURCE_GROUP"
az group create --name "$RESOURCE_GROUP" --location "$EAST_REGION" -o none

# Create the two regional data-plane clusters.
ensure_aks_cluster "$EAST_CLUSTER" "$EAST_REGION"
ensure_aks_cluster "$WEST_CLUSTER" "$WEST_REGION"

log "Waiting for both AKS clusters"
az aks wait --resource-group "$RESOURCE_GROUP" --name "$EAST_CLUSTER" --created --interval 30 --timeout 3600
az aks wait --resource-group "$RESOURCE_GROUP" --name "$WEST_CLUSTER" --created --interval 30 --timeout 3600

EAST_CLUSTER_ID="$(az aks show -g "$RESOURCE_GROUP" -n "$EAST_CLUSTER" --query id -o tsv)"
WEST_CLUSTER_ID="$(az aks show -g "$RESOURCE_GROUP" -n "$WEST_CLUSTER" --query id -o tsv)"
EAST_NODE_RG="$(az aks show -g "$RESOURCE_GROUP" -n "$EAST_CLUSTER" --query nodeResourceGroup -o tsv)"
WEST_NODE_RG="$(az aks show -g "$RESOURCE_GROUP" -n "$WEST_CLUSTER" --query nodeResourceGroup -o tsv)"

# Fleet is the authoritative cluster inventory and placement plane.
log "Creating hub-enabled Fleet $FLEET_NAME"
if ! az fleet show -g "$RESOURCE_GROUP" -n "$FLEET_NAME" -o none 2>/dev/null; then
  az fleet create \
    --resource-group "$RESOURCE_GROUP" \
    --name "$FLEET_NAME" \
    --location "$EAST_REGION" \
    --enable-hub \
    -o none
fi

FLEET_ID="$(az fleet show -g "$RESOURCE_GROUP" -n "$FLEET_NAME" --query id -o tsv)"
ACCOUNT_TYPE="$(az account show --query user.type -o tsv)"
if [[ "$ACCOUNT_TYPE" == "user" ]]; then
  OPERATOR_PRINCIPAL_ID="$(az ad signed-in-user show --query id -o tsv)"
  OPERATOR_PRINCIPAL_TYPE="User"
else
  OPERATOR_CLIENT_ID="$(az account show --query user.name -o tsv)"
  OPERATOR_PRINCIPAL_ID="$(az ad sp show --id "$OPERATOR_CLIENT_ID" --query id -o tsv)"
  OPERATOR_PRINCIPAL_TYPE="ServicePrincipal"
fi

# Hub kubeconfig authentication is Azure RBAC-backed.
log "Granting the current principal Fleet hub cluster-admin access"
ensure_role_assignment \
  "$OPERATOR_PRINCIPAL_ID" \
  "Azure Kubernetes Fleet Manager RBAC Cluster Admin" \
  "$FLEET_ID" \
  "$OPERATOR_PRINCIPAL_TYPE"
ensure_role_assignment \
  "$OPERATOR_PRINCIPAL_ID" \
  "Azure Kubernetes Fleet Manager ABAC Custom Resources Reader" \
  "$FLEET_ID" \
  "$OPERATOR_PRINCIPAL_TYPE"
ensure_role_assignment \
  "$OPERATOR_PRINCIPAL_ID" \
  "Azure Kubernetes Fleet Manager ABAC Custom Resources Writer" \
  "$FLEET_ID" \
  "$OPERATOR_PRINCIPAL_TYPE"

ensure_fleet_member "$EAST_MEMBER" "$EAST_CLUSTER_ID" "region=$EAST_REGION topology=global-demo"
ensure_fleet_member "$WEST_MEMBER" "$WEST_CLUSTER_ID" "region=$WEST_REGION topology=global-demo"

log "Getting Fleet hub and AKS validation kubeconfigs"
az fleet get-credentials -g "$RESOURCE_GROUP" -n "$FLEET_NAME" -f "$HUB_KUBECONFIG" --overwrite-existing -o none
az aks get-credentials -g "$RESOURCE_GROUP" -n "$EAST_CLUSTER" -f "$EAST_KUBECONFIG" --admin --overwrite-existing -o none
az aks get-credentials -g "$RESOURCE_GROUP" -n "$WEST_CLUSTER" -f "$WEST_KUBECONFIG" --admin --overwrite-existing -o none

# Allow time for a newly-created Fleet RBAC assignment to reach the hub API.
for _ in $(seq 1 60); do
  if KUBECONFIG="$HUB_KUBECONFIG" kubectl get membercluster "$EAST_MEMBER" -o name >/dev/null 2>&1; then
    break
  fi
  printf 'waiting for Fleet hub RBAC propagation\n'
  sleep 10
done
KUBECONFIG="$HUB_KUBECONFIG" kubectl get membercluster "$EAST_MEMBER" -o name >/dev/null

KUBECONFIG="$HUB_KUBECONFIG" kubectl wait --for=condition=Joined "membercluster/$EAST_MEMBER" --timeout=20m
KUBECONFIG="$HUB_KUBECONFIG" kubectl wait --for=condition=Joined "membercluster/$WEST_MEMBER" --timeout=20m

# These annotations let the controller resolve Azure resources without member kubeconfigs.
log "Annotating Fleet member inventory with Azure infrastructure metadata"
KUBECONFIG="$HUB_KUBECONFIG" kubectl annotate membercluster "$EAST_MEMBER" \
  gateway.glb.azure.io/aks-resource-id="$EAST_CLUSTER_ID" \
  gateway.glb.azure.io/node-resource-group="$EAST_NODE_RG" \
  gateway.glb.azure.io/location="$EAST_REGION" \
  --overwrite

KUBECONFIG="$HUB_KUBECONFIG" kubectl annotate membercluster "$WEST_MEMBER" \
  gateway.glb.azure.io/aks-resource-id="$WEST_CLUSTER_ID" \
  gateway.glb.azure.io/node-resource-group="$WEST_NODE_RG" \
  gateway.glb.azure.io/location="$WEST_REGION" \
  --overwrite

# Application Network provides Istio ingress, waypoints, and cross-cluster endpoints.
log "Creating Application Network $APPNET_NAME"
if ! az appnet show -g "$RESOURCE_GROUP" -n "$APPNET_NAME" -o none 2>/dev/null; then
  az appnet create \
    --resource-group "$RESOURCE_GROUP" \
    --appnet-name "$APPNET_NAME" \
    --location "$APPNET_REGION" \
    --identity-type SystemAssigned \
    -o none
fi

ensure_appnet_member "$EAST_MEMBER" "$EAST_CLUSTER_ID" "$EAST_REGION" "$EAST_KUBECONFIG"
ensure_appnet_member "$WEST_MEMBER" "$WEST_CLUSTER_ID" "$WEST_REGION" "$WEST_KUBECONFIG"
wait_for_gateway_class "$EAST_KUBECONFIG" "$EAST_CLUSTER"
wait_for_gateway_class "$WEST_KUBECONFIG" "$WEST_CLUSTER"

# ACR build avoids requiring a local Docker daemon.
log "Creating ACR $ACR_NAME and building the controller image"
if ! az acr show -g "$RESOURCE_GROUP" -n "$ACR_NAME" -o none 2>/dev/null; then
  az acr create \
    --resource-group "$RESOURCE_GROUP" \
    --name "$ACR_NAME" \
    --sku Basic \
    --admin-enabled true \
    -o none
else
  az acr update -g "$RESOURCE_GROUP" -n "$ACR_NAME" --admin-enabled true -o none
fi

if [[ "$SKIP_CONTROLLER_IMAGE_BUILD" == "true" ]]; then
  log "Reusing controller image glb-gateway-controller:$CONTROLLER_IMAGE_TAG"
  az acr repository show \
    --name "$ACR_NAME" \
    --image "glb-gateway-controller:$CONTROLLER_IMAGE_TAG" \
    -o none
else
  az acr build \
    --registry "$ACR_NAME" \
    --image "glb-gateway-controller:$CONTROLLER_IMAGE_TAG" \
    .
fi

ACR_SERVER="$(az acr show -g "$RESOURCE_GROUP" -n "$ACR_NAME" --query loginServer -o tsv)"
ACR_USERNAME="$(az acr credential show -g "$RESOURCE_GROUP" -n "$ACR_NAME" --query username -o tsv)"
ACR_PASSWORD="$(az acr credential show -g "$RESOURCE_GROUP" -n "$ACR_NAME" --query 'passwords[0].value' -o tsv)"
CONTROLLER_IMAGE_DIGEST="$(az acr repository show \
  --name "$ACR_NAME" \
  --image "glb-gateway-controller:$CONTROLLER_IMAGE_TAG" \
  --query digest -o tsv)"
[[ "$CONTROLLER_IMAGE_DIGEST" == sha256:* ]] || die "controller image digest was not available"
CONTROLLER_IMAGE="$ACR_SERVER/glb-gateway-controller@$CONTROLLER_IMAGE_DIGEST"

# Run the controller on a member cluster and project its Azure token explicitly.
log "Creating the controller service principal"
CONTROLLER_CLIENT_ID="$(az ad sp list --display-name "$CONTROLLER_SP_NAME" --query '[0].appId' -o tsv)"
if [[ -z "$CONTROLLER_CLIENT_ID" ]]; then
  CONTROLLER_CLIENT_ID="$(az ad app list --display-name "$CONTROLLER_SP_NAME" --query '[0].appId' -o tsv)"
fi
if [[ -z "$CONTROLLER_CLIENT_ID" ]]; then
  CONTROLLER_APP_ARGS=(--display-name "$CONTROLLER_SP_NAME" -o json)
  if [[ -n "$SERVICE_MANAGEMENT_REFERENCE" ]]; then
    CONTROLLER_APP_ARGS+=(--service-management-reference "$SERVICE_MANAGEMENT_REFERENCE")
  fi
  CONTROLLER_CLIENT_ID="$(az ad app create "${CONTROLLER_APP_ARGS[@]}" --query appId -o tsv)"
fi
if ! az ad sp show --id "$CONTROLLER_CLIENT_ID" -o none 2>/dev/null; then
  az ad sp create --id "$CONTROLLER_CLIENT_ID" -o none
fi

CONTROLLER_TENANT_ID="$(az account show --query tenantId -o tsv)"
CONTROLLER_PRINCIPAL_ID="$(az ad sp show --id "$CONTROLLER_CLIENT_ID" --query id -o tsv)"
CONTROLLER_SERVICE_ACCOUNT="glb-gateway-controller"
CONTROLLER_NAMESPACE="glb-gateway-system"
CONTROLLER_FEDERATED_CREDENTIAL_NAME="controller-runtime"
CONTROLLER_SUBJECT="system:serviceaccount:${CONTROLLER_NAMESPACE}:${CONTROLLER_SERVICE_ACCOUNT}"
RUNTIME_OIDC_ISSUER="$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$EAST_CLUSTER" \
  --query oidcIssuerProfile.issuerUrl \
  -o tsv)"
[[ -n "$RUNTIME_OIDC_ISSUER" ]] || die "$EAST_CLUSTER did not publish an OIDC issuer"

# Bind the controller ServiceAccount subject to the Entra application.
CONTROLLER_FEDERATED_CREDENTIAL_ID="$(az ad app federated-credential list \
  --id "$CONTROLLER_CLIENT_ID" \
  --query "[?name=='$CONTROLLER_FEDERATED_CREDENTIAL_NAME'].id | [0]" \
  -o tsv)"
CONTROLLER_FEDERATED_CREDENTIAL="$(jq -cn \
  --arg name "$CONTROLLER_FEDERATED_CREDENTIAL_NAME" \
  --arg issuer "$RUNTIME_OIDC_ISSUER" \
  --arg subject "$CONTROLLER_SUBJECT" \
  '{name:$name,issuer:$issuer,subject:$subject,audiences:["api://AzureADTokenExchange"]}')"
if [[ -z "$CONTROLLER_FEDERATED_CREDENTIAL_ID" ]]; then
  az ad app federated-credential create \
    --id "$CONTROLLER_CLIENT_ID" \
    --parameters "$CONTROLLER_FEDERATED_CREDENTIAL" \
    -o none
else
  az ad app federated-credential update \
    --id "$CONTROLLER_CLIENT_ID" \
    --federated-credential-id "$CONTROLLER_FEDERATED_CREDENTIAL_ID" \
    --parameters "$CONTROLLER_FEDERATED_CREDENTIAL" \
    -o none
fi

# Limit network writes to the demo RG and the two AKS-managed node RGs.
MAIN_RG_SCOPE="/subscriptions/$SUBSCRIPTION_ID/resourceGroups/$RESOURCE_GROUP"
EAST_NODE_RG_SCOPE="/subscriptions/$SUBSCRIPTION_ID/resourceGroups/$EAST_NODE_RG"
WEST_NODE_RG_SCOPE="/subscriptions/$SUBSCRIPTION_ID/resourceGroups/$WEST_NODE_RG"

log "Granting scoped ARM access to the controller"
ensure_role_assignment "$CONTROLLER_PRINCIPAL_ID" Reader "$MAIN_RG_SCOPE"
ensure_role_assignment "$CONTROLLER_PRINCIPAL_ID" "Network Contributor" "$MAIN_RG_SCOPE"
ensure_role_assignment "$CONTROLLER_PRINCIPAL_ID" "Network Contributor" "$EAST_NODE_RG_SCOPE"
ensure_role_assignment "$CONTROLLER_PRINCIPAL_ID" "Network Contributor" "$WEST_NODE_RG_SCOPE"

# Install the APIs and controller permissions on the Fleet hub.
log "Installing controller APIs and RBAC on the Fleet hub"
KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -f \
  https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.1/standard-install.yaml
KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -f \
  "https://raw.githubusercontent.com/istio/istio/${ISTIO_CRD_VERSION}/manifests/charts/base/files/crd-all.gen.yaml"
KUBECONFIG="$HUB_KUBECONFIG" kubectl create namespace "$CONTROLLER_NAMESPACE" \
  --dry-run=client -o yaml | KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -f -
KUBECONFIG="$HUB_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" create serviceaccount "$CONTROLLER_SERVICE_ACCOUNT" \
  --dry-run=client -o yaml | KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -f -
KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -k config/crd
KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -k config/rbac
KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -k config/gatewayclass

# Remove script-owned workload objects from deployments made by older versions.
KUBECONFIG="$HUB_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" delete \
  deployment/glb-gateway-controller secret/acr-pull secret/azure-credentials \
  --ignore-not-found

# Give the remote controller a short-lived hub credential.
HUB_CONTROLLER_TOKEN="$(KUBECONFIG="$HUB_KUBECONFIG" kubectl \
  -n "$CONTROLLER_NAMESPACE" create token "$CONTROLLER_SERVICE_ACCOUNT" --duration=24h)"
HUB_SERVER="$(kubectl config view --raw --minify --kubeconfig "$HUB_KUBECONFIG" \
  -o jsonpath='{.clusters[0].cluster.server}')"
HUB_CA_DATA="$(kubectl config view --raw --minify --kubeconfig "$HUB_KUBECONFIG" \
  -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
[[ -n "$HUB_CONTROLLER_TOKEN" && -n "$HUB_SERVER" && -n "$HUB_CA_DATA" ]] || \
  die "could not build the controller Fleet hub kubeconfig"
HUB_CONTROLLER_KUBECONFIG="$(jq -cn \
  --arg server "$HUB_SERVER" \
  --arg ca "$HUB_CA_DATA" \
  --arg token "$HUB_CONTROLLER_TOKEN" \
  '{apiVersion:"v1",kind:"Config",clusters:[{name:"fleet-hub",cluster:{server:$server,"certificate-authority-data":$ca}}],contexts:[{name:"fleet-hub",context:{cluster:"fleet-hub",user:"controller"}}],"current-context":"fleet-hub",users:[{name:"controller",user:{token:$token}}]}')"

# The managed Fleet hub does not run user pods, so use the east member as runtime.
log "Deploying the controller runtime into $EAST_CLUSTER"
KUBECONFIG="$EAST_KUBECONFIG" kubectl apply -k config/manager
KUBECONFIG="$EAST_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" create secret docker-registry acr-pull \
  --docker-server="$ACR_SERVER" \
  --docker-username="$ACR_USERNAME" \
  --docker-password="$ACR_PASSWORD" \
  --dry-run=client -o yaml | KUBECONFIG="$EAST_KUBECONFIG" kubectl apply -f -

KUBECONFIG="$EAST_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" create secret generic azure-credentials \
  --from-literal=AZURE_CLIENT_ID="$CONTROLLER_CLIENT_ID" \
  --from-literal=AZURE_TENANT_ID="$CONTROLLER_TENANT_ID" \
  --from-literal=AZURE_FEDERATED_TOKEN_FILE=/var/run/secrets/azure/tokens/azure-identity-token \
  --dry-run=client -o yaml | KUBECONFIG="$EAST_KUBECONFIG" kubectl apply -f -
KUBECONFIG="$EAST_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" create secret generic fleet-hub-kubeconfig \
  --from-literal=kubeconfig="$HUB_CONTROLLER_KUBECONFIG" \
  --dry-run=client -o yaml | KUBECONFIG="$EAST_KUBECONFIG" kubectl apply -f -

KUBECONFIG="$EAST_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" patch serviceaccount "$CONTROLLER_SERVICE_ACCOUNT" \
  --type merge \
  -p '{"imagePullSecrets":[{"name":"acr-pull"}]}'
KUBECONFIG="$EAST_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" set image \
  deployment/glb-gateway-controller controller="$CONTROLLER_IMAGE"
KUBECONFIG="$EAST_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" patch deployment glb-gateway-controller \
  --type strategic \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"controller","volumeMounts":[{"name":"azure-identity-token","mountPath":"/var/run/secrets/azure/tokens","readOnly":true},{"name":"fleet-hub-kubeconfig","mountPath":"/var/run/fleet-hub","readOnly":true}]}],"volumes":[{"name":"azure-identity-token","projected":{"sources":[{"serviceAccountToken":{"audience":"api://AzureADTokenExchange","expirationSeconds":3600,"path":"azure-identity-token"}}]}},{"name":"fleet-hub-kubeconfig","secret":{"secretName":"fleet-hub-kubeconfig"}}]}}}}'
KUBECONFIG="$EAST_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" set env \
  deployment/glb-gateway-controller --from=secret/azure-credentials
KUBECONFIG="$EAST_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" set env \
  deployment/glb-gateway-controller KUBECONFIG=/var/run/fleet-hub/kubeconfig
KUBECONFIG="$EAST_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" rollout restart deployment/glb-gateway-controller
KUBECONFIG="$EAST_KUBECONFIG" kubectl -n "$CONTROLLER_NAMESPACE" rollout status \
  deployment/glb-gateway-controller --timeout=10m

# Place the global Service, waypoint, and region-labelled echo workload first.
log "Deploying backing Services, waypoint, echo workloads, and Fleet placement"
sed \
  -e "s/MEMBER_CLUSTER_EAST/$EAST_MEMBER/g" \
  -e "s/MEMBER_CLUSTER_WEST/$WEST_MEMBER/g" \
  -e "s/REGION_EAST/$EAST_REGION/g" \
  -e "s/REGION_WEST/$WEST_REGION/g" \
  config/samples/demo_workload.yaml | \
  KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -f -

wait_for_regional_workloads

# The controller turns this global intent into two regional gateways and one GLB.
log "Applying the global Gateway, HTTPRoute, and policy"
sed \
  -e "s/SUBSCRIPTION_ID/$SUBSCRIPTION_ID/g" \
  -e "s/RESOURCE_GROUP/$RESOURCE_GROUP/g" \
  -e "s/REGION_GLOBAL/$EAST_REGION/g" \
  config/samples/demo_gateway.yaml | \
  KUBECONFIG="$HUB_KUBECONFIG" kubectl apply -f -

GLOBAL_IP="$(wait_for_global_address)"

# Fleet can still be rolling out the generated locality policy after Azure is ready.
log "Verifying safe outlier ejection limits"
wait_for_safe_outlier_policy

log "Verifying local-first routing at both regional frontends"
wait_for_local_first "$EAST_MEMBER" "$EAST_REGION"
wait_for_local_first "$WEST_MEMBER" "$WEST_REGION"

# Azure data-plane propagation can lag ARM success, so curl with bounded retries.
log "Waiting for the Azure Global Load Balancer data plane at $GLOBAL_IP"
RESPONSE=""
for _ in $(seq 1 120); do
  RESPONSE="$(curl --silent --show-error --connect-timeout 5 --max-time 10 "http://$GLOBAL_IP" 2>/dev/null || true)"
  if [[ "$RESPONSE" == "hello from $EAST_REGION" || "$RESPONSE" == "hello from $WEST_REGION" ]]; then
    break
  fi
  printf 'global curl not ready; response=%q\n' "$RESPONSE"
  sleep 10
done

if [[ "$RESPONSE" != "hello from $EAST_REGION" && "$RESPONSE" != "hello from $WEST_REGION" ]]; then
  # Leave useful hub state in the failed run's output before exiting.
  KUBECONFIG="$HUB_KUBECONFIG" kubectl -n global-demo describe globalgatewaypolicy global-demo || true
  KUBECONFIG="$EAST_KUBECONFIG" kubectl -n glb-gateway-system logs deployment/glb-gateway-controller --tail=200 || true
  die "global load balancer did not return the regional echo response"
fi

log "End-to-end demo succeeded"
printf 'Global IP: %s\n' "$GLOBAL_IP"
printf 'Response:  %s\n' "$RESPONSE"
printf 'Controller service principal: %s\n' "$CONTROLLER_CLIENT_ID"
printf 'Delete that tenant-level application after the demo with:\n'
printf '  az ad app delete --id %s\n' "$CONTROLLER_CLIENT_ID"
