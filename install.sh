#!/bin/bash

# Define variables
DOCKER_IMAGE="javgarcia0907/k8s-pdb-autoscaler:latest"
NAMESPACE="default"
DEPLOYMENT_FILE="config/manager/manager.yaml"
SERVICE_ACCOUNT_FILE="service_account.yaml"
ROLE_FILE="role.yaml"
ROLE_BINDING_FILE="role_binding.yaml"
CLUSTER_ROLE_FILE="clusterrole.yaml"
CLUSTER_ROLE_BINDING_FILE="clusterrolebinding.yaml"
PDB_FILE="pdb.yaml"
WEBHOOK_CLUSTER_ROLE_FILE="webhookclusterrole.yaml"
WEBHOOK_ROLE_BINDING_FILE="webhookrolebind.yaml"
WEBHOOK_SERVICE_FILE="config/webhook/manifests/web_service.yml"
WEBHOOK_CONFIGURATION_FILE="config/webhook/manifests/webhook_configuration.yaml"
WEBHOOK_DEPLOYMENT_FILE="config/webhook/manifests/webhook_deployment.yaml"
WEBHOOK_CERTS_SECRET="webhook-certs"
WEBHOOK_CERT_FILE="webhook.crt"
WEBHOOK_KEY_FILE="webhook.key"
PDBWATCHER_CRD_FILE="pdbwatcher_crd.yaml"

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

# Function to create a secret
create_secret() {
  if [ -f $2 ] && [ -f $3 ]; then
    echo "Creating secret $1"
    kubectl create secret generic $1 --from-file=tls.crt=$2 --from-file=tls.key=$3 -n $NAMESPACE
  else
    echo "Secret files $2 or $3 do not exist"
    exit 1
  fi
}

# Generate certificates
generate_certificates() {
  echo "Generating certificates..."
  openssl req -new -newkey rsa:2048 -days 365 -nodes -x509 \
    -subj "/CN=${WEBHOOK_SERVICE_NAME}.${NAMESPACE}.svc" \
    -keyout $WEBHOOK_KEY_FILE -out $WEBHOOK_CERT_FILE
}

# Auto inject CA Bundle to Webhook Config 
inject_ca_bundle() {
  echo "Injecting CA Bundle into webhook configuration..."
  CA_BUNDLE=$(cat $WEBHOOK_CERT_FILE | base64 | tr -d '\n')
  sed -i "s/\${CA_BUNDLE}/${CA_BUNDLE}/g" $WEBHOOK_CONFIGURATION_FILE
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

# Generate certificates
generate_certificates

# Create Webhook Certificates Secret
create_secret $WEBHOOK_CERTS_SECRET $WEBHOOK_CERT_FILE $WEBHOOK_KEY_FILE #generate secrets on fly with install script 

# Inject CA Bundle
inject_ca_bundle

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

# Apply Webhook Cluster Role
apply_yaml $WEBHOOK_CLUSTER_ROLE_FILE

# Apply Webhook Role Binding
apply_yaml $WEBHOOK_ROLE_BINDING_FILE

# Apply Webhook Service
apply_yaml $WEBHOOK_SERVICE_FILE

# Apply Webhook Configuration
apply_yaml $WEBHOOK_CONFIGURATION_FILE

# Apply Webhook Deployment
apply_yaml $WEBHOOK_DEPLOYMENT_FILE

# Apply PDBWatcher CRD
apply_yaml $PDBWATCHER_CRD_FILE

echo "Installation completed."
