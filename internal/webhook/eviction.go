package webhook

import (
	"context"
	"net/http"
	"time"

	myappsv1 "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
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
	evictionLog := myappsv1.Eviction{
		PodName:      req.Name,
		EvictionTime: time.Now().Format(time.RFC3339),
	}

	// Fetch the pod to get its labels
	pod := &corev1.Pod{}
	err := e.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, pod)
	if err != nil {
		logger.Error(err, "Error: Unable to fetch Pod")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// List all PDBWatchers in the namespace
	pdbWatcherList := &myappsv1.PDBWatcherList{}
	err = e.Client.List(ctx, pdbWatcherList, &client.ListOptions{Namespace: req.Namespace})
	if err != nil {
		logger.Error(err, "Error: Unable to list PDBWatchers")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Find the applicable PDBWatcher
	var applicablePDBWatcher *myappsv1.PDBWatcher
	for _, pdbWatcher := range pdbWatcherList.Items {
		// Fetch the associated PDB
		pdb := &policyv1.PodDisruptionBudget{}
		err := e.Client.Get(ctx, types.NamespacedName{Name: pdbWatcher.Spec.PDBName, Namespace: pdbWatcher.Namespace}, pdb)
		if err != nil {
			logger.Error(err, "Error: Unable to fetch PDB:", "pdbname", pdbWatcher.Spec.PDBName)
			continue
		}

		// Check if the PDB selector matches the evicted pod's labels
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			logger.Error(err, "Error: Invalid PDB selector", "pdbname", pdbWatcher.Spec.PDBName)
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

	//TODO only update if we're 1 minute since last eviction to avoid swarms.

	applicablePDBWatcher.Spec.LastEviction = evictionLog

	err = e.Client.Update(ctx, applicablePDBWatcher)
	if err != nil {
		logger.Error(err, "Unable to update PDBWatcher status")
		return admission.Errored(http.StatusInternalServerError, err) //this might happen if there's alot of evictions... Allow? Retry?
	}

	logger.Info("Eviction logged successfully", "podName", req.Name, "evictionTime", evictionLog.EvictionTime)
	return admission.Allowed("eviction allowed")
}

// what the heck does this do
func (e *EvictionHandler) InjectDecoder(d *admission.Decoder) error {
	e.decoder = d
	return nil
}
