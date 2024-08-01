#!/bin/bash

# Define the namespace
NAMESPACE=default

# Get the list of deployments to create PDBs and PDBWatchers for
deployments=$(kubectl get deployments -n $NAMESPACE --no-headers | awk '$1 !~ /^(example-pdbwatcher|eviction-webhook|controller-manager)$/ {print $1}')

# Create PDB and PDBWatcher for each deployment
for deploy in $deployments; do
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

done

# Apply the PDB YAML files
for pdb in *-pdb.yaml; do
  kubectl apply -f $pdb
done

# Apply the PDBWatcher YAML files
for pdbwatcher in *-pdbwatcher.yaml; do
  kubectl apply -f $pdbwatcher
done

echo "All PDBs and PDBWatchers have been created and applied."

