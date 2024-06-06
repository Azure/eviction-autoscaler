#!/bin/bash

# Define variables
DOCKER_IMAGE="javgarcia0907/k8s-pdb-autoscaler:latest"
NAMESPACE="system"
CRD_FILE="config/crd/bases"
DEPLOYMENT_FILE="config/manager/manager.yaml"
SERVICE_ACCOUNT_FILE="service_account.yaml"
ROLE_FILE="role.yaml"
ROLE_BINDING_FILE="role_binding.yaml"
CLUSTER_ROLE_FILE="clusterrole.yaml"
CLUSTER_ROLE_BINDING_FILE="clusterrolebinding.yaml"
PDB_FILE="testPDB.yaml"
DEPLOYMENT_TEST_FILE="testDeployment.yaml"

# Function to create namespace if not exists
create_namespace() {
  kubectl get namespace $1 > /dev/null 2>&1
  if [ $? -ne 0 ]; then
    echo "Creating namespace $1"
    kubectl create namespace $1
  else
    echo "Namespace $1 already exists"
  fi
}

# Function to apply a yaml file
apply_yaml() {
  if [ -f $1 ]; then
    echo "Applying $1"
    kubectl apply -f $1
  else
    echo "File $1 does not exist"
    exit 1
  fi
}

# Start installation
echo "Starting installation..."

# Build the Docker image
echo "Building Docker image..."
docker build -t $DOCKER_IMAGE .

# Push the Docker image
echo "Pushing Docker image..."
docker push $DOCKER_IMAGE

# Create namespace
create_namespace $NAMESPACE

# Apply CRD
apply_yaml $CRD_FILE

# Apply Service Account
apply_yaml $SERVICE_ACCOUNT_FILE

# Apply Role
apply_yaml $ROLE_FILE

# Apply Role Binding
apply_yaml $ROLE_BINDING_FILE

# Apply Cluster Role
apply_yaml $CLUSTER_ROLE_FILE

# Apply Cluster Role Binding
apply_yaml $CLUSTER_ROLE_BINDING_FILE

# Apply Deployment
apply_yaml $DEPLOYMENT_FILE

# Apply PDB
apply_yaml $PDB_FILE

# Apply Deployment Test
apply_yaml $DEPLOYMENT_TEST_FILE

echo "Installation completed."

