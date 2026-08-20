package controllers

// Finalizers the controller places on an EvictionAutoScaler. Both live here so the full set
// of teardown guarantees is discoverable in one place, even though each is actuated by a
// different reconciler: the EvictionAutoScaler reconciler writes the Deployment/HPA/KEDA
// surge and owns EASSurgeFinalizer; the PDB actuator writes the partner PDB and owns
// PDBFloorFinalizer. Each controller owns the finalizer for the object it writes.
const (
	// EASSurgeFinalizer is placed on the EvictionAutoScaler while it holds an active surge on
	// its target, so a mid-drain CR delete is held until the EvictionAutoScaler reconciler
	// reverts the surge. Removing it releases the CR for garbage collection.
	EASSurgeFinalizer = "eviction-autoscaler.azure.com/surge-revert"

	// PDBFloorFinalizer is placed on the EvictionAutoScaler while its partner PDB carries a
	// floor mutation, so a mid-drain CR delete is held until the PDB actuator restores the
	// partner PDB (or determines restore is moot). Removing it releases the CR for garbage
	// collection.
	PDBFloorFinalizer = "eviction-autoscaler.azure.com/pdb-floor"
)
