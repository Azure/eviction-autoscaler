+++++++++++++-+#!/bin/bash

# Define the namespace
NAMESPACE=$1














et the list of deployments to create PDBs and PDBWatchers for
deployments=$(kubectl get deployments -n $NAMESPACE --no-headers 
statefulsets=$(kubectl get statefulsets -n $NAMESPACE --no-headers

# Function to create and apply PDB and PDBWatcher YAMLs
create_and_apply_resources() {
  local deploy=$1
  local kind=$2

  # Get the labels of the deployment
  labels=$(kubectl get $kind $deploy -n $NAMESPACE -o jsonpath='{.spec.template.metadata.labels}')

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

  echo "Created PDB YAML for $kind: $deploy"

  # Create a PDBWatcher YAML configuration
  cat <<EOF > ${deploy}-pdbwatcher.yaml
apiVersion: apps.mydomain.com/v1
kind: PDBWatcher
metadata:
  name: ${deploy}-pdb-watcher
  namespace: $NAMESPACE
spec:
  pdbName: ${deploy}-pdb
  targetName: $deploy
  targetKind: $kind
EOF

  echo "Created PDBWatcher YAML for deployment: $deploy"

  # Apply the PDB YAML file
  kubectl apply -f ${deploy}-pdb.yaml
  if [ $? -eq 0 ]; then
    echo "Applied PDB for $kind: $deploy"
      rm ${deploy}-pdb.yaml
  else
    echo "Failed to apply PDB for $kind: $deploy"
    return 1
  fi


  # Apply the PDBWatcher YAML file
  kubectl apply -f ${deploy}-pdbwatcher.yaml
  if [ $? -eq 0 ]; then
    echo "Applied PDBWatcher for deployment: $deploy"
    rm ${deploy}-pdbwatcher.yaml
  else
    echo "Failed to apply PDBWatcher for deployment: $deploy"
    return 1
  fi
}

# Loop through each deployment and create resources
for deploy in $deployments; do
  echo "actioning on $deploy"
  create_and_apply_resources $deploy "deployment"
done

for ss in $statefulsets; do
  echo "actioning on $ss"
  create_and_apply_resources $ss "statefulset"
done

+e+cho "All PDBs and PDBWatchers have been created and applied."
