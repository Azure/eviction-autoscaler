package webhook

import (
	"context"
	"net/http"
	"time"

	pdbautoscaler "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
	corev1 "k8s.io/api/core/v1"
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

// this webhook updates the pdbwatcher's spec if there is a newish (configurable) eviction to cause a reconcile and see if we need to scale up
func (e *EvictionHandler) Handle(ctx context.Context, req admission.Request) admission.Response {

	logger := log.FromContext(ctx)

	logger.Info("Received eviction request", "namespace", req.Namespace, "podname", req.Name)

	// Log eviction request
	now := time.Now()
	currentEviction := pdbautoscaler.Eviction{
		PodName:      req.Name,
		EvictionTime: now.Format(time.RFC3339), //is there a guid or time on request we could use?
	}

	// Fetch the pod to get its labels
	pod := &corev1.Pod{}
	err := e.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, pod)
	if err != nil {
		logger.Error(err, "Error: Unable to fetch Pod")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// List all PDBWatchers in the namespace. Is this expensive for every eviction are we cacching this list and pdbs?
	pdbWatcherList := &pdbautoscaler.PDBWatcherList{}
	err = e.Client.List(ctx, pdbWatcherList, &client.ListOptions{Namespace: req.Namespace})
	if err != nil {
		logger.Error(err, "Error: Unable to list PDBWatchers")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Find the applicable PDBWatcher
	var applicablePDBWatcher *pdbautoscaler.PDBWatcher
	for _, pdbWatcher := range pdbWatcherList.Items {
		pdbWatcher := pdbWatcher
		// Fetch the associated PDB
		pdb := &policyv1.PodDisruptionBudget{}
		err := e.Client.Get(ctx, types.NamespacedName{Name: pdbWatcher.Name, Namespace: pdbWatcher.Namespace}, pdb)
		if err != nil {
			logger.Error(err, "Error: Unable to fetch PDB:", "pdbname", pdbWatcher.Name)
			continue
		}

		// Check if the PDB selector matches the evicted pod's labels
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			logger.Error(err, "Error: Invalid PDB selector", "pdbname", pdbWatcher.Name)
			continue
		}

		if selector.Matches(labels.Set(pod.Labels)) {
			applicablePDBWatcher = &pdbWatcher
			break
		}
	}

	if applicablePDBWatcher == nil {
		logger.Info("No applicable PDBWatcher found")
		return admission.Allowed("no applicable PDBWatcher")
	}

	logger.Info("Found pdbwatcher", "name", applicablePDBWatcher.Name)

	/* Can't do this because we might miss our last eviction
	//only update if we're 1 minute since last eviction to avoid swarms.
	//impolite to mutex here as it would block api server. Could have a single channel and channel read updates

	if applicablePDBWatcher.Spec.LastEviction.EvictionTime != "" {
		evictionTime, err := time.Parse(time.RFC3339, applicablePDBWatcher.Spec.LastEviction.EvictionTime)
		if err != nil {
			logger.Error(err, "Failed to parse eviction time "+applicablePDBWatcher.Spec.LastEviction.EvictionTime)
		} else {
			if now.Sub(evictionTime) < time.Minute { //mak configurable in CRD
				logger.Info("Eviction logged successfully", "podName", req.Name, "evictionTime", currentEviction.EvictionTime)
				return admission.Allowed("eviction ignored")
			}
		}
	}
	*/

	applicablePDBWatcher.Spec.LastEviction = currentEviction

	err = e.Client.Update(ctx, applicablePDBWatcher)
	if err != nil {
		//handle conflicts when many evictions happen in parallel? or doesn't matter if we lose the conficts
		logger.Error(err, "Unable to update PDBWatcher status")
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
