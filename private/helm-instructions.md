# Eviction-Autoscaler Helm Installation

## Install via Helm

1. Add the eviction-autoscaler Helm repository:

    ```bash
    helm repo add eviction-autoscaler https://azure.github.io/eviction-autoscaler/charts
    helm repo update
    ```

2. Install the chart into your cluster:

    ```bash
    helm install eviction-autoscaler eviction-autoscaler/eviction-autoscaler \
          --namespace eviction-autoscaler --create-namespace \
          --set controllerConfig.pdb.create=true
    ```

**Configuration Examples:**

Enable PDB creation and specify target namespaces:
```bash
helm install eviction-autoscaler eviction-autoscaler/eviction-autoscaler \
      --namespace eviction-autoscaler --create-namespace \
      --set controllerConfig.pdb.create=true \
      --set controllerConfig.namespaces.actionedNamespaces="{kube-system,default}"
```

Enable eviction-autoscaler for all namespaces by default:
```bash
helm install eviction-autoscaler eviction-autoscaler/eviction-autoscaler \
      --namespace eviction-autoscaler --create-namespace \
      --set controllerConfig.pdb.create=false \
      --set controllerConfig.namespaces.enabledByDefault=true
```

> **Note:** Setting `pdb.create=true` will automatically create a PodDisruptionBudget (PDB) for deployments that do not already have one, ensuring your workloads are protected and enabling eviction-autoscaler to manage disruptions effectively.
>
> If a deployment already has a PDB whose label selector matches the deployment's pod template labels, eviction-autoscaler will **not** create a new PDB—even if `pdb.create=true`. This avoids duplicate PDBs and ensures existing disruption budgets are respected.
>
> For example, if you deploy an app without a PDB:
>
> ```yaml
> apiVersion: apps/v1
> kind: Deployment
> metadata:
>   name: my-app
>   namespace: default
> spec:
>   replicas: 2
>   selector:
>     matchLabels:
>       app: my-app
>   template:
>     metadata:
>       labels:
>         app: my-app
>     spec:
>       containers:
>       - name: my-app
>         image: nginx
> ```
>
> With `pdb.create=true`, eviction-autoscaler will automatically create a matching PDB:
>
> ```yaml
> apiVersion: policy/v1
> kind: PodDisruptionBudget
> metadata:
>   name: my-app
>   namespace: default
> spec:
>   minAvailable: 2
>   selector:
>     matchLabels:
>       app: my-app
> ```
>
> If a matching PDB already exists, eviction-autoscaler will not create another. If you later disable `pdb.create`, eviction-autoscaler will not delete any existing PDBs—it will simply stop creating new ones.

3. (Optional) Customize values by passing `--values my-values.yaml` or using `--set key=value`.

Refer to the [Helm Values](https://github.com/Azure/eviction-autoscaler/blob/main/helm/eviction-autoscaler/values.yaml) for configuration options.

## Namespace Configuration via Helm

### Mode 1: `ENABLED_BY_DEFAULT=false` (Default)

```bash
helm install eviction-autoscaler eviction-autoscaler/eviction-autoscaler \
  --namespace eviction-autoscaler --create-namespace \
  --set controllerConfig.pdb.create=true \
  --set controllerConfig.namespaces.enabledByDefault=false \
  --set-json 'controllerConfig.namespaces.actionedNamespaces=["kube-system","production","staging"]'
```

Or via values.yaml:

```yaml
controllerConfig:
  pdb:
    create: true
  namespaces:
    enabledByDefault: false  # Namespaces disabled by default (default)
    actionedNamespaces:
      - kube-system
      - production
      - staging
```

### Mode 2: `ENABLED_BY_DEFAULT=true`

```bash
helm install eviction-autoscaler eviction-autoscaler/eviction-autoscaler \
  --namespace eviction-autoscaler --create-namespace \
  --set controllerConfig.pdb.create=true \
  --set controllerConfig.namespaces.enabledByDefault=true
```

Or via values.yaml:

```yaml
controllerConfig:
  pdb:
    create: true
  namespaces:
    enabledByDefault: true  # Namespaces enabled by default
    actionedNamespaces: []   # Ignored when enabled_by_default=true
```

### Example: enabled_by_default=false Configuration

```bash
helm install eviction-autoscaler eviction-autoscaler/eviction-autoscaler \
  --namespace eviction-autoscaler --create-namespace \
  --set controllerConfig.pdb.create=true \
  --set controllerConfig.namespaces.enabledByDefault=true \
  --set-json 'controllerConfig.namespaces.actionedNamespaces=["kube-system","production"]'
```
