#!/bin/bash

# Define variables
CONTROLLER_IMAGE="javgarcia0907/k8s-pdb-autoscaler:webhookv1"
WEBHOOK_IMAGE="javgarcia0907/k8s-pdb-autoscaler:webhookv1"
NAMESPACE="default"
DEPLOYMENT_FILE="config/manager/manager.yaml"
SERVICE_ACCOUNT_FILE="service_account.yaml"
ROLE_FILE="role.yaml"
ROLE_BINDING_FILE="role_binding.yaml"
CLUSTER_ROLE_FILE="clusterrole.yaml"
CLUSTER_ROLE_BINDING_FILE="clusterrolebinding.yaml"
WEBHOOK_CLUSTER_ROLE_FILE="config/webhook/manifests/Roles/webhookclusterrole.yaml"
WEBHOOK_ROLE_BINDING_FILE="config/webhook/manifests/Roles/webhookrolebind.yaml"
WEBHOOK_SERVICE_FILE="config/webhook/manifests/web_service.yml"
WEBHOOK_CONFIGURATION_FILE="config/webhook/manifests/webhook_configuration.yaml"
WEBHOOK_DEPLOYMENT_FILE="config/webhook/manifests/webhook_deployment.yaml"
WEBHOOK_CERTS_SECRET="webhook-certs"
WEBHOOK_CERT_FILE="config/webhook/manifests/tls.crt"
WEBHOOK_KEY_FILE="config/webhook/manifests/tls.key"
WEBHOOK_CSR_FILE="config/webhook/manifests/webhook.csr"
CSR_CONF_FILE="config/webhook/manifests/csr.conf"
CA_CERT_FILE="config/webhook/manifests/ca.crt"
CA_KEY_FILE="config/webhook/manifests/ca.key"
WEBHOOK_ACCOUNT_TOKEN="config/webhook/manifests/service-account-token-secret.yaml"
PDBWATCHER_CRD_FILE="internal/controller/PDBRoles/pdbwatcher_crd.yaml"
PDBWATCHER_ROLE="internal/controller/PDBRoles/clusterrole.yaml"
PDBWATCHER_BIND="internal/controller/PDBRoles/clusterrolebinding.yaml"

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
  openssl req -new -newkey rsa:2048 -nodes -keyout $WEBHOOK_KEY_FILE -out $WEBHOOK_CSR_FILE -config $CSR_CONF_FILE
  openssl x509 -req -in $WEBHOOK_CSR_FILE -CA $CA_CERT_FILE -CAkey $CA_KEY_FILE -CAcreateserial -out $WEBHOOK_CERT_FILE -days 365 -extensions v3_req -extfile $CSR_CONF_FILE
}

# Auto inject CA Bundle to Webhook Config 
inject_ca_bundle() {
  echo "Injecting CA Bundle into webhook configuration..."
  CA_BUNDLE=$(cat $CA_CERT_FILE | base64 | tr -d '\n')
  sed -i "s/\${CA_BUNDLE}/${CA_BUNDLE}/g" $WEBHOOK_CONFIGURATION_FILE
}

# Start installation
echo "Starting installation..."

# Build the Docker image for the controller
echo "Building Docker image for the controller..."
docker build -t $CONTROLLER_IMAGE -f Dockerfile.controller .

# Build the Docker image for the webhook
echo "Building Docker image for the webhook..."
docker build -t $WEBHOOK_IMAGE -f Dockerfile.webhook .

# Push the Docker image for the controller
echo "Pushing Docker image for the controller..."
docker push $CONTROLLER_IMAGE

# Push the Docker image for the webhook
echo "Pushing Docker image for the webhook..."
docker push $WEBHOOK_IMAGE

# Create namespace
create_namespace $NAMESPACE

# Generate certificates
generate_certificates

# Create Webhook Certificates Secret
create_secret $WEBHOOK_CERTS_SECRET $WEBHOOK_CERT_FILE $WEBHOOK_KEY_FILE

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

# Apply Deployment for Controller
apply_yaml $DEPLOYMENT_FILE

# Apply Webhook Cluster Role
apply_yaml $WEBHOOK_CLUSTER_ROLE_FILE

# Apply Webhook Role Binding
apply_yaml $WEBHOOK_ROLE_BINDING_FILE

# Apply Webhook Service
apply_yaml $WEBHOOK_SERVICE_FILE

# Apply Webhook Secret token
apply_yaml $WEBHOOK_ACCOUNT_TOKEN

# Apply Webhook Configuration
apply_yaml $WEBHOOK_CONFIGURATION_FILE

# Apply Webhook Deployment
apply_yaml $WEBHOOK_DEPLOYMENT_FILE

# Apply PDBWatcher CRD
apply_yaml $PDBWATCHER_CRD_FILE

# Apply PDBWatcher Role
apply_yaml $PDBWATCHER_ROLE

# Apply PDBWatcher Rolebind
apply_yaml $PDBWATCHER_BIND

echo "Installation completed."
