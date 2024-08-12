#!/bin/bash

# Define the namespace
NAMESPACE=kube-system

# Get the list of deployments to create PDBs and PDBWatchers for
deployments=$(kubectl get deployments -n $NAMESPACE --no-headers | awk '$1 !~ /^(example-pdbwatcher|eviction-webhook)$/ {print $1}')

# Function to create and apply PDB and PDBWatcher YAMLs
create_and_apply_resources() {
  local deploy=$1

  # Get the labels of the deployment
  labels=$(kubectl get deployment $deploy -n $NAMESPACE -o jsonpath='{.spec.template.metadata.labels}')

  # Create a PDB YAML configuration
  cat <<EOF > ${deploy}-pdb.yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: ${deploy}-pdb
  namespace: $NAMESPACE
spec:
  minAvailable: 1
  selector:
    matchLabels: $labels
EOF

  echo "Created PDB YAML for deployment: $deploy"

  # Create a PDBWatcher YAML configuration
  cat <<EOF > ${deploy}-pdbwatcher.yaml
apiVersion: apps.mydomain.com/v1
kind: PDBWatcher
metadata:
  name: ${deploy}-pdb-watcher
  namespace: $NAMESPACE
spec:
  pdbName: ${deploy}-pdb
  deploymentName: $deploy
EOF

  echo "Created PDBWatcher YAML for deployment: $deploy"

  # Apply the PDB YAML file
  kubectl apply -f ${deploy}-pdb.yaml
  if [ $? -eq 0 ]; then
    echo "Applied PDB for deployment: $deploy"
  else
    echo "Failed to apply PDB for deployment: $deploy"
    return 1
  fi

  # Apply the PDBWatcher YAML file
  kubectl apply -f ${deploy}-pdbwatcher.yaml
  if [ $? -eq 0 ]; then
    echo "Applied PDBWatcher for deployment: $deploy"
  else
    echo "Failed to apply PDBWatcher for deployment: $deploy"
    return 1
  fi
}

# Loop through each deployment and create resources
for deploy in $deployments; do
  echo "actioning on $deploy"
  create_and_apply_resources $deploy
done

echo "All PDBs and PDBWatchers have been created and applied."
