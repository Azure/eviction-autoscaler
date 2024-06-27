package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PDBWatcherSpec defines the desired state of PDBWatcher
type PDBWatcherSpec struct {
	// PDBName is the name of the Pod Disruption Budget to watch.
	PDBName string `json:"pdbName"`
	// DeploymentName is the name of the Deployment to scale.
	DeploymentName string `json:"deploymentName"`
	// ScaleFactor is the factor by which the Deployment should be scaled.
	ScaleFactor int32 `json:"scaleFactor"`
	// MinReplicas is the minimum number of replicas to maintain for the Deployment.
	// Note: The scaling will start from this value.
	MinReplicas int32 `json:"minReplicas"`
	// MaxReplicas is the maximum number of replicas to maintain for the Deployment defined by the MaxSurge amout.
	// Note: The scaling will not exceed this value.
	MaxReplicas int32 `json:"maxReplicas"`
}

// PDBWatcherStatus defines the observed state of PDBWatcher
type PDBWatcherStatus struct {
	// CurrentReplicas is the current number of replicas of the Deployment.
	CurrentReplicas int32 `json:"currentReplicas"`
	// DisruptionsAllowed is the current number of disruptions allowed for the PDB.
	DisruptionsAllowed int32 `json:"disruptionsAllowed"`
	// Error contains any error encountered during the reconcile process.
	Error string `json:"error,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// PDBWatcher is the Schema for the pdbwatchers API
type PDBWatcher struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PDBWatcherSpec   `json:"spec,omitempty"`
	Status PDBWatcherStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PDBWatcherList contains a list of PDBWatcher
type PDBWatcherList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PDBWatcher `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PDBWatcher{}, &PDBWatcherList{})
}
