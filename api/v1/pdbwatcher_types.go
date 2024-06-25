package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PDBWatcherSpec defines the desired state of PDBWatcher
type PDBWatcherSpec struct {
	PDBName        string `json:"pdbName"`
	DeploymentName string `json:"deploymentName"`
	ScaleFactor    int32  `json:"scaleFactor"`
	MinReplicas    int32  `json:"minReplicas"`
	MaxReplicas    int32  `json:"maxReplicas"`
}

// PDBWatcherStatus defines the observed state of PDBWatcher
type PDBWatcherStatus struct {
	CurrentReplicas    int32  `json:"currentReplicas"`
	DisruptionsAllowed int32  `json:"disruptionsAllowed"`
	Error              string `json:"error,omitempty"`
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
