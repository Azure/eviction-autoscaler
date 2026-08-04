package controllers

import (
	"context"
	"time"

	pdbautoscaler "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	"github.com/azure/eviction-autoscaler/internal/podutil"
	corev1 "k8s.io/api/core/v1"
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

// EvictionAutoScalerReconciler reconciles a EvictionAutoScaler object
type NodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

const NodeNameIndex = "spec.nodeName"

// drainingTaintKeys are taints applied by node-lifecycle controllers that drain
// nodes without cordoning them (spec.unschedulable stays false):
//   - karpenter.sh/disrupted: Karpenter v1 disruption and termination
//   - ToBeDeletedByClusterAutoscaler: cluster-autoscaler scale-down
var drainingTaintKeys = []string{
	"karpenter.sh/disrupted",
	"ToBeDeletedByClusterAutoscaler",
}

// hasDrainingTaint reports whether the node carries a NoSchedule taint from a
// known draining controller. Matching specific keys (not any NoSchedule taint)
// matters: dedicated-node taints are permanent and do not signal a drain.
func hasDrainingTaint(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule {
			continue
		}
		for _, key := range drainingTaintKeys {
			if taint.Key == key {
				return true
			}
		}
	}
	return false
}

// nodeIsDraining reports whether evictions on this node are imminent, either
// via a classic cordon or via a draining controller's taint.
func nodeIsDraining(node *corev1.Node) bool {
	return node.Spec.Unschedulable || hasDrainingTaint(node)
}

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=watch;get;list

// Reconcile is the main loop of the controller. It will look for unschedulded nodes and for every pod on the node
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	namespace := req.Namespace
	if namespace == "" {
		namespace = "_cluster"
	}
	defer recordPanic("node", namespace, &req.Name)
	logger := log.FromContext(ctx)

	// Fetch the EvictionAutoScaler instance
	node := &corev1.Node{}
	err := r.Get(ctx, req.NamespacedName, node)
	if err != nil {
		//should we use a finalizer to scale back down on deletion?
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil // EvictionAutoScaler not found, could be deleted, nothing to do
		}
		return ctrl.Result{}, err // Error fetching EvictionAutoScaler
	}

	// Track node cordoning/draining events
	if nodeIsDraining(node) {
		metrics.NodeCordoningCounter.Inc()
	} else {
		return ctrl.Result{}, nil
	}

	logger.Info("Node is draining", "node", node.Name, "unschedulable", node.Spec.Unschedulable, "drainingTaint", hasDrainingTaint(node))

	var podlist corev1.PodList
	if err := r.List(ctx, &podlist, client.MatchingFields{NodeNameIndex: node.Name}); err != nil {
		return ctrl.Result{}, err
	}

	podchanged := false
	for _, pod := range podlist.Items {
		// TODO group pods by namespace to share list/get of EvictionAutoScalers/pdbs
		// Also  could do this to avoid list/llooku up but need to measure if either helps
		//if !possibleTarget(pod.GetOwnerReferences()) {
		//	continue
		//}

		EvictionAutoScalerList := &pdbautoscaler.EvictionAutoScalerList{}
		err = r.Client.List(ctx, EvictionAutoScalerList, &client.ListOptions{Namespace: pod.Namespace})
		if err != nil {
			logger.Error(err, "Error: Unable to list EvictionAutoScalers")
			return ctrl.Result{}, err
		}
		var applicableEvictionAutoScaler *pdbautoscaler.EvictionAutoScaler
		for _, EvictionAutoScaler := range EvictionAutoScalerList.Items {
			// Fetch the PDB using a 1:1 name mapping
			pdb := &policyv1.PodDisruptionBudget{}
			err = r.Get(ctx, types.NamespacedName{Name: EvictionAutoScaler.Name, Namespace: EvictionAutoScaler.Namespace}, pdb)
			if err != nil {
				if errors.IsNotFound(err) {
					logger.Error(err, "no matching pdb", "namespace", EvictionAutoScaler.Namespace, "name", EvictionAutoScaler.Name)
					continue
				}
				return ctrl.Result{}, err
			}

			// Check if the PDB selector matches the evicted pod's labels
			selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
			if err != nil {
				logger.Error(err, "Error: Invalid PDB selector", "pdbname", EvictionAutoScaler.Name)
				continue
			}

			if selector.Matches(labels.Set(pod.Labels)) {
				applicableEvictionAutoScaler = EvictionAutoScaler.DeepCopy()
				break //should we keep going to ensure multiple EvictionAutoScalers don't match?
			}
		}
		if applicableEvictionAutoScaler == nil {
			continue
		}

		// Track eviction and node drain events
		metrics.EvictionCounter.WithLabelValues(pod.Namespace).Inc()

		logger.Info("Found EvictionAutoScaler for pod", "name", applicableEvictionAutoScaler.Name, "namespace", pod.Namespace, "podname", pod.Name, "node", node.Name)
		pod := pod.DeepCopy()
		updatedpod := podutil.UpdatePodCondition(&pod.Status, &corev1.PodCondition{
			Type:    corev1.DisruptionTarget,
			Status:  corev1.ConditionTrue,
			Reason:  "EvictionAttempt",
			Message: "eviction attempt anticipated by node cordon",
		})
		if updatedpod {
			if err := r.Client.Status().Update(ctx, pod); err != nil {
				logger.Error(err, "Error: Unable to update Pod status")
				return ctrl.Result{}, err
			}
		}

		applicableEvictionAutoScaler.Spec.LastEviction = pdbautoscaler.Eviction{
			PodName:      pod.Name,
			EvictionTime: metav1.Now(),
		}
		if err := r.Update(ctx, applicableEvictionAutoScaler); err != nil {
			logger.Error(err, "unable to update EvictionAutoScaler", "name", applicableEvictionAutoScaler.Name)
			return ctrl.Result{}, err
		}
		podchanged = true
	}

	///if we updated requeue again so we keep updating (could ignore if there were no pods mathing pdbs)
	// pods till they get off or node is uncordoned.
	//TODO pull smallest cooldown from all EvictionAutoScalers if they allow defining it.
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
			// Trigger only on drain-signal transitions: cordon flips or a
			// draining taint appearing/disappearing. Create/Delete keep the
			// default (true) so already-draining nodes reconcile on resync.
			UpdateFunc: nodeDrainSignalChanged,
		}).
		Complete(r)
}

// nodeDrainSignalChanged reports whether an update transitions the node's
// drain signal (cordon flip or draining-taint appearance/removal), ignoring
// status-only updates such as heartbeats.
func nodeDrainSignalChanged(ue event.UpdateEvent) bool {
	oldNode, okOld := ue.ObjectOld.(*corev1.Node)
	newNode, okNew := ue.ObjectNew.(*corev1.Node)
	if !okOld || !okNew {
		return false
	}
	return oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable ||
		hasDrainingTaint(oldNode) != hasDrainingTaint(newNode)
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
