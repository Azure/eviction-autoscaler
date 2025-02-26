package webhook

import (
	"context"
	"net/http"

	pdbautoscaler "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/podutil"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type EvictionHandler struct {
	Client  client.Client
	decoder *admission.Decoder
}

// this webhook updates the EvictionAutoScaler's spec if there is a newish (configurable) eviction to cause a reconcile and see if we need to scale up
func (e *EvictionHandler) Handle(ctx context.Context, req admission.Request) admission.Response {

	logger := log.FromContext(ctx)

	logger.Info("Received eviction request", "namespace", req.Namespace, "podname", req.Name)

	currentEviction := pdbautoscaler.Eviction{
		PodName:      req.Name,
		EvictionTime: metav1.Now(),
	}

	// Fetch the pod to get its labels
	pod := &corev1.Pod{}
	err := e.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, pod)
	if err != nil {
		logger.Error(err, "Error: Unable to fetch Pod")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	podObj := pod.DeepCopy()

	// List all EvictionAutoScalers in the namespace. Is this expensive for every eviction are we cacching this list and pdbs?
	EvictionAutoScalerList := &pdbautoscaler.EvictionAutoScalerList{}
	err = e.Client.List(ctx, EvictionAutoScalerList, &client.ListOptions{Namespace: req.Namespace})
	if err != nil {
		logger.Error(err, "Error: Unable to list EvictionAutoScalers")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Find the applicable EvictionAutoScaler
	var applicableEvictionAutoScaler *pdbautoscaler.EvictionAutoScaler
	for _, EvictionAutoScaler := range EvictionAutoScalerList.Items {
		EvictionAutoScaler := EvictionAutoScaler
		// Fetch the associated PDB
		pdb := &policyv1.PodDisruptionBudget{}
		err := e.Client.Get(ctx, types.NamespacedName{Name: EvictionAutoScaler.Name, Namespace: EvictionAutoScaler.Namespace}, pdb)
		if err != nil {
			logger.Error(err, "Error: Unable to fetch PDB:", "pdbname", EvictionAutoScaler.Name)
			return admission.Errored(http.StatusInternalServerError, err)
		}

		// Check if the PDB selector matches the evicted pod's labels
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			logger.Error(err, "Error: Invalid PDB selector", "pdbname", EvictionAutoScaler.Name)
			continue
		}

		if selector.Matches(labels.Set(pod.Labels)) {
			applicableEvictionAutoScaler = &EvictionAutoScaler
			break
		}
	}

	if applicableEvictionAutoScaler == nil {
		logger.Info("No applicable EvictionAutoScaler found")
		return admission.Allowed("no applicable EvictionAutoScaler")
	}

	logger.Info("Found EvictionAutoScaler", "name", applicableEvictionAutoScaler.Name)

	updatedpod := podutil.UpdatePodCondition(&podObj.Status, &v1.PodCondition{
		Type:    v1.DisruptionTarget,
		Status:  v1.ConditionTrue,
		Reason:  "EvictionAttempt",
		Message: "eviction attempt recorded by eviction webhook",
	})
	if updatedpod {
		if err := e.Client.Status().Update(ctx, podObj); err != nil {
			logger.Error(err, "Error: Unable to update Pod status")
			//don't fail yet still want to try and update the EvictionAutoScaler
		}
	}

	// want to rate limit on mass evictions but also if we slow down too much we may miss last eviction and not scale down.
	//if applicableEvictionAutoScaler.Spec.LastEviction.EvictionTime.Time.Sub(currentEviction.EvictionTime.Time) < time.Second {
	//	return admission.Allowed("eviction allowed")
	//}

	applicableEvictionAutoScaler.Spec.LastEviction = currentEviction

	err = e.Client.Update(ctx, applicableEvictionAutoScaler)
	if err != nil {
		//handle conflicts when many evictions happen in parallel? or doesn't matter if we lose the conficts
		logger.Error(err, "Unable to update EvictionAutoScaler status")
		return admission.Errored(http.StatusInternalServerError, err) //Is this a problem if webhook doesn't ignore failures?
	}

	logger.Info("Eviction logged successfully", "podName", req.Name, "evictionTime", currentEviction.EvictionTime)
	return admission.Allowed("eviction allowed")
}

// what the heck does this do
func (e *EvictionHandler) InjectDecoder(d *admission.Decoder) error {
	e.decoder = d
	return nil
}
