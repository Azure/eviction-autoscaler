# Eviction-Autoscaler

[![Go Report Card](https://goreportcard.com/badge/github.com/azure/eviction-autoscaler)](https://goreportcard.com/report/github.com/azure/eviction-autoscaler)
[![GoDoc](https://pkg.go.dev/badge/github.com/azure/eviction-autoscaler)](https://pkg.go.dev/github.com/azure/eviction-autoscaler)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![CI Pipeline](https://github.com/azure/eviction-autoscaler/actions/workflows/ci.yml/badge.svg)](https://github.com/azure/eviction-autoscaler/actions/workflows/ci.yml)

## Table of Contents

- [Introduction](#introduction)
- [Features](#features)
- [Installation](#installation)
- [Networking](#networking)
- [Usage](#usage)
  - [Surge Annotations](#surge-annotations)
  - [How Surge Sizing Works](#how-surge-sizing-works)

## Introduction

Kubernetes (k8s) deployments already have a max surge concept, and there's no reason this surge should only apply to new rollouts and not to node maintenance or other situations where PodDisruptionBudget (PDB)-protected pods need to be evicted.
This project uses node cordons to signal eviction-autoscaler Custom Resources that correspond to a PodDisruptionBudget and target a deployment. An eviction autoscaler controller then attempts to scale up a the targeted deployment (or scaleset if you're feeling brave) when the pdb's allowed disruptions is zero and scales down once evictions have stopped.

### Why Not Overprovision?

Overprovisioning isn't free. Sometimes it makes sense to run as cost-effectively as possible, but you still don't want to experience downtime due to a cluster upgrade or even a VM maintenance event.  

Your app might also experience issues for unrelated reasons, and a maintenance event shouldn't result in downtime if adding extra replicas can save you.

## Features

- **Node Controller**: Signals eviction-autoscaler for all pods on cordoned nodes selected by corresponding pdb whose name/namespace it shares.
- **Eviction-autoscaler Controller**: Watches eviction-autoscale resources. If there a recent eviction singals and the PDB's AllowedDisruptions is zero, it triggers a surge in the corresponding deployment. Once evitions have stopped for some cooldown period and allowed diruptions has rised above zero it scales down.
- **HPA-aware surge**: When an HPA targets the deployment, the controller surges by temporarily raising the HPA's `minReplicas` instead of mutating deployment replicas directly. This prevents the HPA from immediately scaling the deployment back down during a surge. On revert, the original `minReplicas` floor is restored.
- **KEDA-aware surge**: When a KEDA ScaledObject targets the deployment, the controller surges by temporarily raising the ScaledObject's `minReplicaCount`. The same pattern applies — annotations on the ScaledObject track the surge state and original value for safe revert.
- **PDB Controller** (Optional): Automatically creates eviction-autoscalers Custom Resources for existing PDBs. When an HPA or KEDA ScaledObject targets the deployment, PDB `minAvailable` is set from the autoscaler's min replicas floor rather than `deployment.spec.replicas`.
- **Autoscaler-to-PDB Controller** (Optional): Watches HPA and KEDA ScaledObject changes and updates PDB `minAvailable` to track the autoscaler's min replicas floor, even when deployment replicas don't change.
- **Deployment Controller** (Optional): Creates PDBs for deployments that don't already have them and keeps min available matching the deployments replicas (not counting any surged in by eviction autoscaler). Defers to the Autoscaler-to-PDB controller when an HPA or KEDA ScaledObject is present.

```mermaid
graph TD;
    Cordon[Cordon]
    NodeController[Cordoned Node Controller]
    CRD[Eviction Autoscaler Custom Resource]
    Controller[Eviction-Autoscaler Controller]
    Deployment[Deployment or StatefulSet]
    HPA[HPA / KEDA ScaledObject]
    PDB[Pod Disruption Budget]
    PDBController[Optional PDB creator]
    AutoscalerToPDB[Autoscaler-to-PDB Controller]


    Cordon -->|Triggers| NodeController
    NodeController -->|writes spec| CRD
    CRD -->|spec watched by| Controller
    Controller -->|surges via| HPA
    Controller -->|surges directly if no HPA| Deployment
    Controller -->|Writes status| CRD
    Controller -->|reads allowed disruptions | PDB
    HPA -->|controls| Deployment
    AutoscalerToPDB -->|watches| HPA
    AutoscalerToPDB -->|updates minAvailable| PDB
    PDBController -->|watches | Deployment
    PDBController -->|creates if not exist| PDB
```

## Installation

### Prerequisites

- Docker
- kind for e2e tests.
- A sense of adventure

### Install

You can install Eviction-Autoscaler using the Azure Kubernetes Extension Resource Provider (RP).

### Install via Azure Kubernetes Extension RP

Follow the steps below to register the required features and deploy the extension to your AKS cluster.

#### 1. Register the Extensions Feature

```bash
az feature register --namespace Microsoft.KubernetesConfiguration --name Extensions
```

Wait until the feature state is `Registered`:

```bash
az feature show --namespace Microsoft.KubernetesConfiguration --name Extensions
```

#### 2. Register the Kubernetes Configuration Provider

```bash
az provider register -n Microsoft.KubernetesConfiguration
```

#### 3. Create an AKS Cluster (example)

```bash
az aks create \
        --resource-group <your-resource-group> \
        --name <your-aks-cluster-name> \
        --node-count 2 \
        --generate-ssh-keys
```

#### 4. Deploy the Eviction-Autoscaler Extension

```bash
az k8s-extension create \
    --cluster-name <your-cluster-name> \
    --cluster-type managedClusters \
    --extension-type microsoft.evictionautoscaler \
    --name <your-extension-name> \
    --resource-group <your-resource-group-name> \
    --release-train stable \
    --auto-upgrade-minor-version true
```

**With namespace configuration:**
```bash
az k8s-extension create \
    --cluster-name <your-cluster-name> \
    --cluster-type managedClusters \
    --extension-type microsoft.evictionautoscaler \
    --name eviction-autoscaler \
    --resource-group <your-resource-group-name> \
    --release-train stable \
    --configuration-settings controllerConfig.namespaces.actionedNamespaces="{kube-system,default}" \
    --auto-upgrade-minor-version true
```

**Cluster-wide auto-protection (enable all namespaces and auto-create PDBs):**
```bash
az k8s-extension create \
    --cluster-name <your-cluster-name> \
    --cluster-type managedClusters \
    --extension-type microsoft.evictionautoscaler \
    --name eviction-autoscaler \
    --resource-group <your-resource-group-name> \
    --release-train dev \
    --configuration-settings controllerConfig.namespaces.enabledByDefault=true controllerConfig.pdb.create=true\
    --config AgentTimeoutInMinutes=30 \
    --subscription <your-subscription-id> \
    --version 0.1.16 \
    --auto-upgrade-minor-version false
```

**Configuration Options:**

- `controllerConfig.pdb.create=true` - Automatically creates PDBs for deployments (default: false)
- `controllerConfig.namespaces.enabledByDefault=true` - Enables all namespaces (default: false, opt-in mode)
- `controllerConfig.namespaces.actionedNamespaces` - List of namespaces to enable when using opt-in mode (default: [kube-system])

**Common Configuration Combinations:**

1. **Conservative (Manual Control)** - `pdb.create=false`, `enabledByDefault=false`, `actionedNamespaces=[kube-system]`
   - Only watches specific namespaces, requires manual PDB creation

2. **Targeted Auto-Protection** - `pdb.create=true`, `enabledByDefault=false`, `actionedNamespaces=[production,staging]`
   - Auto-creates PDBs only in specified namespaces
   - Most common production setup - balances automation with control

3. **Cluster-Wide Auto-Protection** - `pdb.create=true`, `enabledByDefault=true`
   - Auto-creates PDBs for all deployments across all namespaces
   - Maximum automation and protection, namespaces can opt-out with annotation `eviction-autoscaler.azure.com/enable: "false"`

4. **Monitoring Only** - `pdb.create=false`, `enabledByDefault=true`
   - Monitors all namespaces but doesn't create PDBs
   - The `eviction-autoscaler.azure.com/pdb-create: "true"` annotation is ignored when controller-level `pdb.create=false`
   - Good for migrating from manual PDB management

> **Note:** The `--configuration-settings controllerConfig.pdb.create=true` option enables automatic creation of PodDisruptionBudgets (PDBs) for deployments that do not already have one. ensuring your workloads are protected and enabling eviction-autoscaler to manage disruptions effectively. Eviction-autoscaler determines whether a deployment already has a corresponding PDB by comparing the PDB's label selector with the deployment's pod template labels. This ensures that each deployment is protected from disruptions and avoids duplicate PDBs. If you later disable `pdb.create`, eviction-autoscaler will not delete any existing PDBs—it will simply stop creating new ones.
> **Note:** The `--auto-upgrade-minor-version false` option is only required if you want to disable automatic minor version upgrades.
> **Note:** The `--release-train dev` option specifies that the extension will use the "dev" release train, which typically includes the latest development builds and experimental features.  
> Other available release train options include `stable` (recommended for production workloads) and `preview` (for pre-release features).  
> Use `dev` for testing or development environments, `preview` for evaluating upcoming features, and `stable` for production deployments.

Refer to the [extension documentation](https://github.com/azure/eviction-autoscaler/tree/main/charts/eviction-autoscaler) for configuration options.

Configuration options will be documented here in future updates. If you have suggestions, please open an issue or PR.

#### Updating Controller Configuration

> **Important:** When modifying controller configuration values (such as `controllerConfig.pdb.create` or `controllerConfig.namespaces.*`), you must delete and re-install the extension for the changes to take effect.
>
> To update configuration:
>
> 1. Delete the existing extension:
>    ```bash
>    az k8s-extension delete --resource-group <your-resource-group> \
>      --cluster-name <your-cluster-name> \
>      --cluster-type managedClusters \
>      --name eviction-autoscaler --yes
>    ```
>
> 2. Re-install the extension with your updated configuration settings using the `az k8s-extension create` command shown above.

### Excluding Deployments from Automatic PDB Creation

If you want to exclude a specific deployment from automatic PodDisruptionBudget (PDB) creation, add the following annotation to its manifest:

```yaml
metadata:
    annotations:
        eviction-autoscaler.azure.com/pdb-create: "false"
```

This annotation instructs eviction-autoscaler not to create a PDB for that deployment, regardless of whether you installed via the Azure Kubernetes Extension Resource Provider.

### Deployments with MaxUnavailable

Eviction-autoscaler automatically skips PDB creation for deployments that have a `maxUnavailable` value other than 0 in their rolling update strategy. This is because such deployments already tolerate some level of downtime during updates or maintenance.

For example, the following deployment will **not** get an automatic PDB:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  replicas: 3
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 25%           # This doesn't affect PDB creation
      maxUnavailable: 1       # Allows 1 pod to be unavailable - skips PDB creation
  # ... rest of spec
```

In this case, since `maxUnavailable: 1`, the deployment is explicitly designed to tolerate one pod being down. Creating a PDB would conflict with this configuration. Note that `maxSurge` does not affect PDB creation - only `maxUnavailable` matters.

If you want a PDB for such a deployment, you can either:
- Set `maxUnavailable: 0` in the deployment strategy, or
- Manually create and manage the PDB yourself

This behavior applies to both integer values (`maxUnavailable: 1`) and percentage values (`maxUnavailable: 25%`). Only deployments with `maxUnavailable: 0` or `maxUnavailable: 0%` will automatically get PDBs created.

### Namespace Control: enabled_by_default Configuration

Eviction autoscaler provides flexible namespace-level control with two operational modes controlled by environment variables:

#### Environment Variables

- **`ENABLED_BY_DEFAULT`**: Controls the operational mode (default: `false`)
  - `false`: Namespaces disabled by default - only specified namespaces enabled
  - `true`: Namespaces enabled by default - all namespaces enabled unless disabled
- **`ACTIONED_NAMESPACES`**: Comma-separated list of namespaces with special behavior
- **`PDB_CREATE`**: Enable automatic PDB creation for deployments (default: `false`)

#### Mode 1: `ENABLED_BY_DEFAULT=false` (Default)

When `ENABLED_BY_DEFAULT=false` (the default), eviction autoscaler operates as follows:

- **All namespaces are disabled by default**
- Namespaces listed in **`ACTIONED_NAMESPACES`** are **automatically enabled**
- Other namespaces can be **enabled** by adding the annotation `eviction-autoscaler.azure.com/enable: "true"`
- Namespaces in `ACTIONED_NAMESPACES` can be **overridden** with annotation `eviction-autoscaler.azure.com/enable: "false"`

**Configuration via environment variables:**

```bash
export ENABLED_BY_DEFAULT=false
export ACTIONED_NAMESPACES="kube-system,production,staging"
```

**Enabling a namespace when enabled_by_default=false:**

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: development
  annotations:
    eviction-autoscaler.azure.com/enable: "true"  # Explicitly enable
```

Or using kubectl:

```bash
kubectl annotate namespace development eviction-autoscaler.azure.com/enable=true
```

#### Mode 2: `ENABLED_BY_DEFAULT=true`

When `ENABLED_BY_DEFAULT=true` is set, eviction autoscaler operates as follows:

- **All namespaces are enabled by default**
- **`ACTIONED_NAMESPACES` is ignored** - only annotations control which namespaces are disabled
- Namespaces can be **disabled** by adding the annotation `eviction-autoscaler.azure.com/enable: "false"`
- Namespaces can be **explicitly enabled** with annotation `eviction-autoscaler.azure.com/enable: "true"` (though they're already enabled by default)

**Configuration via environment variables:**

Set the environment variable `ENABLED_BY_DEFAULT=true`.

**Disabling a namespace when enabled_by_default=true:**

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: development
  annotations:
    eviction-autoscaler.azure.com/enable: "false"  # Explicitly disable
```

Or using kubectl:

```bash
kubectl annotate namespace development eviction-autoscaler.azure.com/enable=false
```

**Enabling a namespace not in the list:**

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: development
  annotations:
    eviction-autoscaler.azure.com/enable: "true"  # Explicitly enable
```

#### Configuration Comparison

| Mode | `ENABLED_BY_DEFAULT` | Default Behavior | `ACTIONED_NAMESPACES` | Annotation Behavior |
|------|---------------------|------------------|----------------------|---------------------|
| **enabled_by_default=false** (default) | `false` or unset | All disabled | These namespaces are enabled | Can enable others with `enable: "true"` or override with `enable: "false"` |
| **enabled_by_default=true** | `true` | All enabled | Ignored | Can disable with `enable: "false"` |

**Important:** Annotations always take precedence over the default behavior and the `ACTIONED_NAMESPACES` list.

### Resource Cleanup and Deletion Behavior

When eviction-autoscaler is disabled for a namespace (either by annotation or configuration change), resources are automatically cleaned up based on their ownership:

#### Controller-Owned Resources (created by eviction-autoscaler)

Resources created by eviction-autoscaler with the `ownedBy: EvictionAutoScaler` annotation are fully managed by the controller:

1. **When a namespace is disabled:**
   - The `DeploymentToPDBReconciler` detects the namespace is disabled
   - It deletes all controller-owned PDBs in that namespace
   - The `EvictionAutoScaler` CRs are automatically deleted by Kubernetes garbage collection (via OwnerReference)

2. **When a deployment is deleted:**
   - The PDB is automatically deleted (via OwnerReference: PDB → Deployment)
   - The `EvictionAutoScaler` CR is automatically deleted (via OwnerReference: EvictionAutoScaler → PDB)

**Example of controller-owned resources:**

```bash
# PDB created by eviction-autoscaler
kubectl get pdb my-app -o yaml
# metadata:
#   annotations:
#     ownedBy: EvictionAutoScaler
#   ownerReferences:
#   - apiVersion: apps/v1
#     kind: Deployment
#     name: my-app
```

#### User-Owned Resources (manually created)

Resources created manually without the `ownedBy: EvictionAutoScaler` annotation are preserved:

1. **When a namespace is disabled:**
   - The `PDBToEvictionAutoScalerReconciler` deletes only the `EvictionAutoScaler` CR
   - **Your manually created PDB is left intact** - eviction-autoscaler never deletes resources it doesn't own

2. **When a deployment is deleted:**
   - If the PDB has no OwnerReference (user-owned), it remains untouched
   - Only the `EvictionAutoScaler` CR is deleted

**Example of user-owned PDB:**

```bash
# User creates their own PDB
kubectl apply -f - <<EOF
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: my-app
  namespace: default
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app: my-app
EOF

# Eviction-autoscaler creates an EvictionAutoScaler CR but does NOT take ownership of the PDB
# If namespace is disabled, only the EvictionAutoScaler CR is deleted - the PDB remains
```

#### Performance Note

Namespace watches trigger reconciliation by listing all deployments/PDBs in that namespace. This is efficient because:
- The controller-runtime client uses an **in-memory cache**
- List operations read from cache, not the Kubernetes API server
- No API server round-trip overhead
- Fast local memory operations

#### Example: enabled_by_default=false Configuration

**Via environment variables:**

Deploy with environment variables:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: eviction-autoscaler
  namespace: eviction-autoscaler
spec:
  template:
    spec:
      containers:
      - name: manager
        env:
        - name: ENABLED_BY_DEFAULT
          value: "true"
        - name: ACTIONED_NAMESPACES
          value: "kube-system,production,staging"
        - name: PDB_CREATE
          value: "true"
```

Deployments in `production` and `staging` namespaces will be managed by eviction autoscaler. Deployments in other namespaces (e.g., `development`, `testing`) will be ignored.

### PDB Ownership and Lifecycle Management

When eviction-autoscaler creates a PodDisruptionBudget (PDB) for a deployment, it manages the PDB's lifecycle using both Kubernetes owner references and annotations:

- **Owner Reference**: Links the PDB to its deployment, ensuring the PDB is deleted when the deployment is deleted
- **Annotation**: `ownedBy: EvictionAutoScaler` marks the PDB as managed by eviction-autoscaler

#### Taking Manual Control of a PDB

If you want to take manual control of a PDB that was created by eviction-autoscaler, remove the `ownedBy` annotation:

```bash
kubectl annotate pdb <pdb-name> -n <namespace> ownedBy-
```

When the annotation is removed, eviction-autoscaler will:
1. Detect the annotation removal (which triggers reconciliation)
2. Remove the owner reference from the PDB
3. Stop managing the PDB

After this, the PDB becomes user-managed and will **not** be deleted when the deployment is deleted. You take full responsibility for managing and cleaning up the PDB.

**Example workflow:**

```bash
# Check the current PDB annotations
kubectl get pdb my-app -n default -o jsonpath='{.metadata.annotations}'

# Remove the ownedBy annotation to take control
kubectl annotate pdb my-app -n default ownedBy-

# The PDB is now yours to manage
# Deleting the deployment will no longer delete the PDB
kubectl delete deployment my-app -n default

# You must manually delete the PDB when you're done with it
kubectl delete pdb my-app -n default
```

**Re-establishing controller ownership:**

If you want eviction-autoscaler to take control back of a PDB, simply add the annotation back:

```bash
# Add the annotation back to return control to eviction-autoscaler
kubectl annotate pdb my-app -n default ownedBy=EvictionAutoScaler

# The controller will re-establish the owner reference on the next reconciliation
# The PDB will now be deleted when the deployment is deleted
```

## Networking

### ARM Endpoint Usage

Eviction-Autoscaler is a **pure Kubernetes operator**. It makes no calls to Azure Resource Manager (ARM) or any other Azure control-plane API. All communication is with the in-cluster Kubernetes API server via `controller-runtime`. Consequently, **no ARM Private Link private endpoint is required** for this extension.

### Required Outbound Network Rules (Private / Restricted Clusters)

The only external network dependency is pulling the extension container image from Microsoft Container Registry (MCR) at install/upgrade time. For clusters with restricted egress (e.g., private AKS clusters with `--outbound-type userDefinedRouting`), allow the following outbound FQDNs:

| FQDN | Port | Purpose |
|---|---|---|
| `mcr.microsoft.com` | 443 | MCR redirect endpoint |
| `*.data.mcr.microsoft.com` | 443 | MCR blob storage (layer download) |

These are already included in the AKS-required FQDN list. Verify your cluster's rules against the official reference:

> **[AKS outbound network and FQDN rules for AKS clusters](https://learn.microsoft.com/azure/aks/outbound-rules-control-egress)**

No additional FQDNs are needed beyond what a standard AKS cluster already requires.

### Private AKS Clusters

Eviction-Autoscaler runs as an in-cluster pod and communicates with the Kubernetes API server through the in-cluster endpoint (`kubernetes.default.svc`). On a [private AKS cluster](https://learn.microsoft.com/azure/aks/private-cluster), this path is already fully private — no extra Private Endpoint configuration is needed for the extension itself.

If you use the `az k8s-extension create` CLI to install or upgrade the extension, that command calls ARM from your **local machine** (not from the cluster). Ensure your local machine or CI agent can reach `management.azure.com`, or use [Azure Private Link for ARM](https://learn.microsoft.com/azure/azure-resource-manager/management/create-private-link-access-portal) if your management plane is also locked down.

## Usage

### Surge Annotations

During a surge, the eviction autoscaler places annotations on the **autoscaler object** (HPA or KEDA ScaledObject) — not on the deployment. This avoids modifying the deployment's metadata and the generation-tracking complexity that comes with it.

| Annotation | Placed On | Value | Purpose |
|---|---|---|---|
| `evictionSurgeReplicas` | HPA or ScaledObject | Surged replica count (e.g., `"3"`) | Marks that a surge is active |
| `eviction-autoscaler.azure.com/original-min-replicas` | HPA or ScaledObject | Pre-surge min replicas (e.g., `"1"`) | Stores the original value for safe revert |
| `evictionSurgeReplicas` | Deployment | Surged replica count | Only used when **no** HPA/KEDA is present |

These annotations are managed automatically by the controller. They are set atomically with the `minReplicas`/`minReplicaCount` change during surge and removed during revert. You should not modify them manually.

**Inspecting surge state:**

```bash
# Check if a surge is active on an HPA
kubectl get hpa <name> -n <namespace> -o jsonpath='{.metadata.annotations}'

# Check if a surge is active on a KEDA ScaledObject
kubectl get scaledobject <name> -n <namespace> -o jsonpath='{.metadata.annotations}'

# Check the original minReplicas that will be restored on revert
kubectl get hpa <name> -n <namespace> -o jsonpath='{.metadata.annotations.eviction-autoscaler\.azure\.com/original-min-replicas}'
```

### How Surge Sizing Works

Eviction-autoscaler scales **to** a specific target rather than scaling **by** a fixed amount. The target is computed per reconcile:

```
surgeTarget = minReplicas + displaced
```

where `displaced` is the number of PDB-selected pods currently running on cordoned nodes. `surgeTarget` is capped at `minReplicas + maxSurge` (the deployment's configured max surge). This means the surge is right-sized to exactly what is needed — no over-provisioning.

#### Incremental Scale-Up

As additional nodes are cordoned during a rolling drain, `displaced` grows and the controller tops the deployment up automatically on each reconcile.

**Example — two-node drain:**

| Event | Cordoned nodes | Displaced pods | Replicas |
|---|---|---|---|
| Initial state | 0 | 0 | `minReplicas` = 3 |
| Node A cordoned (2 pods) | 1 | 2 | 3 + 2 = **5** |
| Node B cordoned (1 pod) | 2 | 3 | 3 + 3 = **6** |
| Pods drain off node A | 1 | 1 | still **6** (see below) |
| All drains complete, cooldown expires | 0 | 0 | back to **3** |

#### Scale-Down Timing

Scale-down back to `minReplicas` happens only when **both** conditions are met:

1. The last eviction happened more than the cooldown period ago (default 60s).
2. The PDB's `DisruptionsAllowed` is greater than zero (i.e. the drain is no longer blocking evictions).

**Importantly, the replica count does not decrease while a drain is still in progress.** If node A drains but node B is still cordoned and blocking evictions, replicas stay at their current level until the full drain completes and the cooldown expires. This avoids a churn cycle where scale-down triggers new evictions, which trigger scale-up again.

```
# What you will see during a partial drain
$ kubectl get deployment my-app -o jsonpath='{.spec.replicas}'
6   # stays here even after some pods have moved, until all drains complete + cooldown
```

If you need to force a faster scale-down you can manually uncordon nodes; once `DisruptionsAllowed` rises and the cooldown passes, the controller will revert.

### Build and Push Multi-Arch Image

Use `docker buildx` through the Make target to build and push a manifest image for multiple architectures.

```bash
make docker-buildx \
  IMG=<registry>/<repo>/eviction-autoscaler:<tag> \
  PLATFORMS=linux/amd64,linux/arm64
```

Notes:
- `docker-buildx` pushes directly to the registry (`--push`), so `IMG` must be a pushable image reference.
- For the `docker-buildx` Makefile target, if `PLATFORMS` is omitted, it defaults to `linux/arm64,linux/amd64,linux/s390x,linux/ppc64le`.
- In `hack/release.sh`, if `RELEASE_PLATFORMS` is omitted, it defaults to `linux/amd64,linux/arm64`.

```bash
kubectl create ns laboratory
kubectl create deployment -n laboratory piggie --image nginx
# unless disabled there will now be a pdb and a pdbwatcher that map to the deployment
# show a starting state
kubectl get pods -n laboratory
kubectl get poddisruptionbudget piggie -n laboratory -o yaml # should be allowed disruptions 0
kubectl get evictionautoscalers piggie -n laboratory -o yaml
# cordon
NODE=$(kubectl get pods -n laboratory -l app=piggie -o=jsonpath='{.items[*].spec.nodeName}')
kubectl cordon $NODE
# show we've scaled up
kubectl get pods -n laboratory
kubectl get poddisruptionbudget piggie -n laboratory -o yaml # should be allowed disruptions 1
kubectl get evictionautoscalers piggie -n laboratory -o yaml
# actually kick the node off now that pdb isn't at zero.
kubectl drain $NODE --delete-emptydir-data --ignore-daemonsets

```

Here's a drain of  Node on a to node cluster that is running the [aks store demo](https://github.com/Azure-Samples/aks-store-demo) (4 deployments and two stateful sets). You can see the drains being rejected then going through on the left and new pods being surged in on the right.

![Screenshot 2024-09-07 173336](https://github.com/user-attachments/assets/c7407ae5-6fcd-48d4-900d-32a7c6ca8b08)

## Shout out

This project originated as an intern project and is still available at [github.com/Javier090/k8s-pdb-autoscaler](https://github.com/Javier090/k8s-pdb-autoscaler).

## Contributing

This project welcomes contributions and suggestions.  Most contributions require you to agree to a
Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us
the rights to use your contribution. For details, visit <https://cla.opensource.microsoft.com>.

When you submit a pull request, a CLA bot will automatically determine whether you need to provide
a CLA and decorate the PR appropriately (e.g., status check, comment). Simply follow the instructions
provided by the bot. You will only need to do this once across all repos using our CLA.

This project has adopted the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/).
For more information see the [Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or
contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with any additional questions or comments.

## Trademarks

This project may contain trademarks or logos for projects, products, or services. Authorized use of Microsoft
trademarks or logos is subject to and must follow
[Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general).
Use of Microsoft trademarks or logos in modified versions of this project must not cause confusion or imply Microsoft sponsorship.
Any use of third-party trademarks or logos are subject to those third-party's policies.
