# Eviction-Autoscaler

[![Go Report Card](https://goreportcard.com/badge/github.com/azure/eviction-autoscaler)](https://goreportcard.com/report/github.com/azure/eviction-autoscaler)
[![GoDoc](https://pkg.go.dev/badge/github.com/azure/eviction-autoscaler)](https://pkg.go.dev/github.com/azure/eviction-autoscaler)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![CI Pipeline](https://github.com/azure/eviction-autoscaler/actions/workflows/ci.yml/badge.svg)](https://github.com/azure/eviction-autoscaler/actions/workflows/ci.yml)

## Table of Contents

- [Introduction](#introduction)
- [Features](#features)
- [Installation](#installation)
- [Usage](#usage)

## Introduction

Kubernetes (k8s) deployments already have a max surge concept, and there's no reason this surge should only apply to new rollouts and not to node maintenance or other situations where PodDisruptionBudget (PDB)-protected pods need to be evicted.
This project uses node cordons to signal eviction-autoscaler Custom Resources that correspond to a PodDisruptionBudget and target a deployment. An eviction autoscaler controller then attempts to scale up a the targeted deployment (or scaleset if you're feeling brave) when the pdb's allowed disruptions is zero and scales down once evictions have stopped.

### Why Not Overprovision?

Overprovisioning isn't free. Sometimes it makes sense to run as cost-effectively as possible, but you still don't want to experience downtime due to a cluster upgrade or even a VM maintenance event.  

Your app might also experience issues for unrelated reasons, and a maintenance event shouldn't result in downtime if adding extra replicas can save you.

## Features

- **Node Controller**: Signals eviction-autoscaler for all pods on cordoned nodes selected by corresponding pdb whose name/namespace it shares.
- **Eviction-autoscaler Controller**: Watches eviction-autoscale resources. If there a recent eviction singals and the PDB's AllowedDisruotions is zero, it triggers a surge in the corresponding deployment. Once evitions have stopped for some cooldown period and allowed diruptions has rised above zero it scales down.
- **PDB Controller** (Optional): Automatically creates eviction-autoscalers Custom Resources for existing PDBs.
- **Deployment Controller** (Optional): Creates PDBs for deployments that don't already have them and keeps min available matching the deployments replicas (not counting any surged in by eviction autoscaler)

```mermaid
graph TD;
    Cordon[Cordon]
    NodeController[Cordoned Node Controller]
    CRD[Eviction Autoscaler Custom Resource]
    Controller[Eviction-Autoscaler Controller]
    Deployment[Deployment or StatefulSet]
    PDB[Pod Disruption Budget]
    PDBController[Optional PDB creator]


    Cordon -->|Triggers| NodeController
    NodeController -->|writes spec| CRD
    CRD -->|spec watched by| Controller
    Controller -->|surges and shrinks| Deployment
    Controller -->|Writes status| CRD
    Controller -->|reads allowed disruptions | PDB
    PDBController -->|watches | Deployment
    PDBController -->|creates if not exist| PDB
```

## Installation

### Prerequisites

- Docker
- kind for e2e tests.
- A sense of adventure

### Install

You can install Eviction-Autoscaler using the Azure Kubernetes Extension Resource Provider (RP) or via Helm.

### Install via Helm

1. Add the eviction-autoscaler Helm repository:

    ```bash
    helm repo add eviction-autoscaler https://azure.github.io/eviction-autoscaler/charts
    helm repo update
    ```

2. Install the chart into your cluster:

    ```bash
    helm install eviction-autoscaler eviction-autoscaler/eviction-autoscaler \
          --namespace eviction-autoscaler --create-namespace \
          --set pdb.create=true
    ```

> **Note:** Setting `pdb.create=true` will automatically create a PodDisruptionBudget (PDB) for deployments that do not already have one, ensuring your workloads are protected and enabling eviction-autoscaler to manage disruptions effectively.  

3. (Optional) Customize values by passing `--values my-values.yaml` or using `--set key=value`.

Refer to the [Helm Values](https://github.com/Azure/eviction-autoscaler/blob/main/helm/eviction-autoscaler/values.yaml) for configuration options.

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
    --release-train dev \
    --config AgentTimeoutInMinutes=30 \
    --subscription <your-subscription-id> \
    --version 0.1.2 \
    --auto-upgrade-minor-version false
```

> **Note:** The `--configuration-settings pdb.create=true` option enables automatic creation of PodDisruptionBudgets (PDBs) for deployments that do not already have one. See the Helm installation section above for details and instructions on excluding specific deployments.
> **Note:** The `--auto-upgrade-minor-version false` option is only required if you want to disable automatic minor version upgrades.
> **Note:** The `--release-train dev` option specifies that the extension will use the "dev" release train, which typically includes the latest development builds and experimental features.  
> Other available release train options include `stable` (recommended for production workloads) and `preview` (for pre-release features).  
> Use `dev` for testing or development environments, `preview` for evaluating upcoming features, and `stable` for production deployments.

Refer to the [extension documentation](https://github.com/azure/eviction-autoscaler/tree/main/charts/eviction-autoscaler) for configuration options.

Configuration options will be documented here in future updates. If you have suggestions, please open an issue or PR.

### Excluding Deployments from Automatic PDB Creation

If you want to exclude a specific deployment from automatic PodDisruptionBudget (PDB) creation, add the following annotation to its manifest:

```yaml
metadata:
    annotations:
        eviction-autoscaler.azure.com/pdb-create: "false"
```

This annotation instructs eviction-autoscaler not to create a PDB for that deployment, regardless of whether you installed via Helm or the Azure Kubernetes Extension Resource Provider.

## Usage

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
