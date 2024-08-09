# k8s-pdb-autoscaler

# Kubernetes Controller and Webhook for PDB Monitoring and Scaling to Manage Pod Evictions

## Name: Javier Garcia

## Project Overview
This project aims to create a Kubernetes controller that watches Pod Disruption Budgets (PDBs) and Deployments, alongside a webhook that monitors pod eviction requests initiated by a client, logs them, and communicates them back to the controller. The controller will dynamically adjust resources in response to the constraints by the PDB during disruptions.

## Project Objectives

### 1. Monitor PDBs and Pod Evictions
- **Controller**: Continuously watch PDBs and their associated pods to detect when evictions are blocked. Ensure the controller accurately identifies the disruption state without causing unnecessary scaling actions.
- **Webhook**: Create an admission webhook to intercept pod eviction requests to determine if evictions are being blocked due to DisruptionsAllowed being zero. As DisruptionsAllowed could be zero as a desired state, annotate accordingly.

### 2. Automate Scaling
- **Controller**: Automatically scale Deployments or StatefulSets when necessary, specifically when evictions are blocked due to DisruptionsAllowed being zero and an actual eviction attempt is detected from the webhook.
- **Webhook**: Provide real-time information about eviction attempts, allowing the controller to make immediate scaling decisions, and to prevent scaling decisions that are against what the author of deployment and PDB is trying to tell you.

### 3. Generate Events and Update Status
- **Controller**: Produce events or update the status of PDBs to notify cluster administrators about scaling actions taken. Generate events when scaling occurs and update annotations to reflect current states.
- **Webhook**: Log events or update the status of PDBs or pods when eviction attempts are blocked.

### 4. Efficient Resource Management
- **Controller**: Avoid unnecessary scaling actions that could lead to resource wastage or application downtime.
- **Webhook**: Provide detailed insights into eviction attempts and PDB statuses, ensuring that the controller only scales resources when truly necessary.

## Installation and Setup

Follow the steps below to install and set up the `k8s-pdb-autoscaler` in your Kubernetes cluster:

### Prerequisites

- A Kubernetes cluster
- `kubectl` configured to interact with your cluster
- Docker installed on your local machine

### Installation Steps

#### 1. Clone the Repository

First, clone the repository and navigate to the project directory:

```bash
git clone https://github.com/your-repo/k8s-pdb-autoscaler.git
cd k8s-pdb-autoscaler
```
### 2. Run the Installation Script

Make sure the script is executable:

```bash
chmod +x install.sh
```
Execute the installation script:

``` bash
./install.sh
```
Run 
``` bash
kubectl get pods
```
To verify controller and webhook have been deployed without any issues. 

### 3. Deploy the `autodeploy.sh` Script

Now run the autodeploy.sh script so the controller and webhook can communicated with the deployments within the cluster within the default namespace, this script will create PodDisruptionBudgets (PDBs) and PDBWatchers for all deployments in the default namespace. It is customizable to fit your needs.

Make sure the script is executable 
```
bash chmod +x autodeploy.sh
```
Run the Script 
Execute the script to create and apply the PDBs and PDBWatchers:
```bash 
./autodeploy.sh
```
