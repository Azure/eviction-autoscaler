#!/bin/bash
set -e
#So ideally we move towards kustomize and cert manager
#that will make image and namespace simpler first then cert manger should make the webhook ca bundle easier.

# Define variables
IMAGE="paulgmiller/k8s-pdb-autoscaler:latest" 
#WEBHOOK_IMAGE="paulgmiller/k8s-pdb-autoscaler:webhookv1"
NAMESPACE="default"
DEPLOYMENT_FILE="config/manager/manager.yaml"
#lot of extra files in rbac. Condense?
SERVICE_ACCOUNT_FILE="config/rbac/service_account.yaml"
ROLE_BINDING_FILE="config/rbac/role_binding.yaml"
CLUSTER_ROLE_FILE="config/rbac/role.yaml"

CRD_FILE="config/crd/bases/eviction-autoscaler.azure.com_evictionsautoscalers.yaml "
WEBHOOK_CONFIGURATION_FILE="config/webhook/manifests/webhook_configuration.yaml"
WEBHOOK_SVC_FILE="config/webhook/manifests/webhook_svc.yaml"

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
  mkdir -p .creds
  echo "Generating certificates..."
  #mkdir .certs #don't want to check this in.
  # Create a private key
  openssl genrsa -out .certs/webhook-server.key 2048

  # Create a certificate signing request (CSR)
  openssl req -new -key .certs/webhook-server.key -out .certs/webhook-server.csr --config config/webhook/manifests/cert.conf  

  # Create a self-signed certificate
  openssl x509 -req -in .certs/webhook-server.csr -signkey .certs/webhook-server.key -out .certs/webhook-server.crt -days 365 -extensions v3_req -extfile config/webhook/manifests/cert.conf
  
  #not idenpotennt
  kubectl create secret tls webhook-server-tls \
    --cert=.certs/webhook-server.crt \
    --key=.certs/webhook-server.key 

  echo "Injecting CA Bundle into webhook configuration..."
  CA_BUNDLE=$(cat .certs/webhook-server.crt  | base64 | tr -d '\n')
  sed -i "s/\${CA_BUNDLE}/${CA_BUNDLE}/g" $WEBHOOK_CONFIGURATION_FILE
  echo $CA_BUNDLE

    #openssl req -new -newkey rsa:2048 -nodes -keyout $WEBHOOK_KEY_FILE -out $WEBHOOK_CSR_FILE -config $CSR_CONF_FILE
  #openssl x509 -req -in $WEBHOOK_CSR_FILE -CA $CA_CERT_FILE -CAkey $CA_KEY_FILE -CAcreateserial -out $WEBHOOK_CERT_FILE -days 365 -extensions v3_req -extfile $CSR_CONF_FILE
}


# Start installation
echo "Starting installation..."

# Build the Docker image for the controller
echo "Building Docker image for the controller..."
docker build -t $IMAGE .

# Push the Docker image for the controller
echo "Pushing Docker image for the controller..."
docker push $IMAGE


# Create namespace
create_namespace $NAMESPACE


# Apply CRD
apply_yaml $CRD_FILE

# Apply Service Account
apply_yaml $SERVICE_ACCOUNT_FILE

# Apply Cluster role
apply_yaml $CLUSTER_ROLE_FILE

# Apply ClusterRole Binding
apply_yaml $ROLE_BINDING_FILE

#leases aren't at cluster level but are name space specific
apply_yaml config/rbac/leader_election_role_binding.yaml 
apply_yaml config/rbac/leader_election_role.yaml 

# Apply Deployment for Controller/webhook
apply_yaml $DEPLOYMENT_FILE

#need an if statement for webhook or not

# Generate certificates
#generate_certificates

# Apply Webhook Configuration
#apply_yaml $WEBHOOK_CONFIGURATION_FILE

# Apply Webhook svc
#apply_yaml $WEBHOOK_SVC_FILE


echo "Installation completed."
