package controllers

import (
	"context"
	"time"

	pdbautoscaler "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
	"github.com/paulgmiller/k8s-pdb-autoscaler/internal/podutil"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// PDBWatcherReconciler reconciles a PDBWatcher object
type NodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

const NodeNameIndex = "spec.nodeName"

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=watch;get;list

// Reconcile is the main loop of the controller. It will look for unschedulded nodes and for every pod on the node
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PDBWatcher instance
	node := &corev1.Node{}
	err := r.Get(ctx, req.NamespacedName, node)
	if err != nil {
		//should we use a finalizer to scale back down on deletion?
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil // PDBWatcher not found, could be deleted, nothing to do
		}
		return ctrl.Result{}, err // Error fetching PDBWatcher
	}
	node = node.DeepCopy() //don't mutate the cache
	logger.Info("Reconciling node", "name", node.Name, "unschedulable", node.Spec.Unschedulable)

	if !node.Spec.Unschedulable {
		return ctrl.Result{}, err

	}
	var podlist corev1.PodList
	if err := r.List(ctx, &podlist, client.MatchingFields{NodeNameIndex: node.Name}); err != nil {
		return ctrl.Result{}, err
	}

	podchanged := false
	for _, pod := range podlist.Items {
		// TODO group pods by namespace to share list/get of pdbwatchers/pdbs
		// Also  could do this to avoid list/llooku up but need to measure if either helps
		//if !possibleTarget(pod.GetOwnerReferences()) {
		//	continue
		//}

		pdbWatcherList := &pdbautoscaler.PDBWatcherList{}
		err = r.Client.List(ctx, pdbWatcherList, &client.ListOptions{Namespace: req.Namespace})
		if err != nil {
			logger.Error(err, "Error: Unable to list PDBWatchers")
			return ctrl.Result{}, err
		}
		var applicablePDBWatcher *pdbautoscaler.PDBWatcher
		for _, pdbWatcher := range pdbWatcherList.Items {
			// Fetch the PDB using a 1:1 name mapping
			pdb := &policyv1.PodDisruptionBudget{}
			err = r.Get(ctx, types.NamespacedName{Name: pdbWatcher.Name, Namespace: pdbWatcher.Namespace}, pdb)
			if err != nil {
				if errors.IsNotFound(err) {
					logger.Error(err, "no matching pdb", "namespace", pdbWatcher.Namespace, "name", pdbWatcher.Name)
					continue
				}
				return ctrl.Result{}, err
			}

			// Check if the PDB selector matches the evicted pod's labels
			selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
			if err != nil {
				logger.Error(err, "Error: Invalid PDB selector", "pdbname", pdbWatcher.Name)
				continue
			}

			if selector.Matches(labels.Set(pod.Labels)) {
				applicablePDBWatcher = pdbWatcher.DeepCopy()
				break
			}
		}
		if applicablePDBWatcher == nil {
			continue
		}

		logger.Info("Found pdbwatcher for pod", "name", applicablePDBWatcher.Name, "namespace", pod.Namespace, "podname", pod.Name)
		pod := pod.DeepCopy()
		updatedpod := podutil.UpdatePodCondition(&pod.Status, &v1.PodCondition{
			Type:    v1.DisruptionTarget,
			Status:  v1.ConditionTrue,
			Reason:  "EvictionAttempt",
			Message: "eviction attempt anticipated by node cordon",
		})
		if updatedpod {
			if err := r.Client.Status().Update(ctx, pod); err != nil {
				logger.Error(err, "Error: Unable to update Pod status")
				return ctrl.Result{}, err
			}
		}

		applicablePDBWatcher.Spec.LastEviction = pdbautoscaler.Eviction{
			PodName:      pod.Name,
			EvictionTime: metav1.Now(),
		}
		if err := r.Update(ctx, applicablePDBWatcher); err != nil {
			logger.Error(err, "unable to update pdbwatcher", "name", applicablePDBWatcher.Name)
			return ctrl.Result{}, err
		}
		podchanged = true
	}

	///if we updated requeue again so we keep updating (could ignore if there were no pods mathing pdbs)
	// pods till they get off or node is uncordoned.
	//TODO pull smallest cooldown from all pdbwatchers if they allow defining it.
	var cooldownNeeded time.Duration
	if podchanged {
		cooldownNeeded = cooldown
	}
	return ctrl.Result{RequeueAfter: cooldownNeeded}, nil
}

func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.TODO(), &corev1.Pod{}, NodeNameIndex, func(rawObj client.Object) []string {
		// Extract the spec.nodeName field
		pod := rawObj.(*corev1.Pod)
		if pod.Spec.NodeName == "" {
			return nil // Don't index Pods without a NodeName
		}
		return []string{pod.Spec.NodeName}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		WithEventFilter(predicate.Funcs{
			// ignore status updates as we only care about cordon.
			UpdateFunc: func(ue event.UpdateEvent) bool {
				oldNode := ue.ObjectOld.(*v1.Node)
				newNode := ue.ObjectNew.(*v1.Node)
				return oldNode.Spec.Unschedulable == newNode.Spec.Unschedulable
			},
		}).
		Complete(r)
}

/*
func possibleTarget(owners []metav1.OwnerReference) bool {
	//this kind of funny since a deployment pod will be owned by a replicaset
	for _, owner := range owners {
		if owner.Kind == "ReplicaSet" || owner.Kind == "StatefulSet" {
			return true
		}
	}
	return false
}*/
