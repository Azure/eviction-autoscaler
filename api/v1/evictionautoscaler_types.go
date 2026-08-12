package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EvictionLog defines a log entry for pod evictions
type Eviction struct {
	PodName      string      `json:"podName,omitempty"`
	EvictionTime metav1.Time `json:"evictionTime,omitempty"`
}

// EvictionAutoScalerSpec defines the desired state of EvictionAutoScaler
type EvictionAutoScalerSpec struct {
	//todo make this mirror horizontalpodautoscaler's target reference
	TargetName   string   `json:"targetName"`
	TargetKind   string   `json:"targetKind"` //deployment or statefulset (anything with an update statedgy)
	LastEviction Eviction `json:"lastEviction,omitempty"`
}

// EvictionAutoScalerStatus defines the observed state of EvictionAutoScaler
type EvictionAutoScalerStatus struct {
	LastEviction     Eviction           `json:"lastEviction,omitempty"` //this is the last one the controller has processed.
	MinReplicas      int32              `json:"minReplicas"`            // Minimum number of replicas to maintain
	TargetGeneration int64              `json:"deploymentGeneration"`   // generation (spec hash) of deployment or statefulse
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// EvictionAutoScaler is the Schema for the EvictionAutoScalers API
type EvictionAutoScaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EvictionAutoScalerSpec   `json:"spec,omitempty"`
	Status EvictionAutoScalerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EvictionAutoScalerList contains a list of EvictionAutoScaler
type EvictionAutoScalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EvictionAutoScaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EvictionAutoScaler{}, &EvictionAutoScalerList{})
}
