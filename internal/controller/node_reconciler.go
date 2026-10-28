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
	// Must match the manager client configuration: registering an index creates a Pod informer.
	PodCacheEnabled bool
}

const NodeNameIndex = "spec.nodeName"

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

	// Track node cordoning events
	if node.Spec.Unschedulable {
		metrics.NodeCordoningCounter.Inc()
	}

	if !node.Spec.Unschedulable {
		return ctrl.Result{}, err
	}

	logger.Info("Node is cordoned", "node", node.Name)

	// The default Pod cache retains identity, labels, owners, node name and full
	// status. POD_CACHE_ENABLED=false sends this same filtered list to the API.
	var podlist corev1.PodList
	if err := r.List(ctx, &podlist, client.MatchingFields{NodeNameIndex: node.Name}); err != nil {
		return ctrl.Result{}, err
	}

	autoscalersByNamespace, err := r.listEvictionAutoScalersForPods(ctx, podlist.Items)
	if err != nil {
		return ctrl.Result{}, err
	}

	podchanged := false
	for _, pod := range podlist.Items {
		applicableEvictionAutoScaler, matched, err := r.findMatchingEvictionAutoScaler(
			ctx, &pod, autoscalersByNamespace[pod.Namespace])
		if err != nil {
			return ctrl.Result{}, err
		}
		if !matched {
			continue
		}

		// Track eviction and node drain events
		metrics.EvictionCounter.WithLabelValues(pod.Namespace).Inc()

		logger.Info("Found EvictionAutoScaler for pod", "name", applicableEvictionAutoScaler.Name, "namespace", pod.Namespace, "podname", pod.Name, "node", node.Name)
		if err := r.markPodDisruptionTarget(ctx, &pod); err != nil {
			logger.Error(err, "Unable to patch Pod status", "namespace", pod.Namespace, "podname", pod.Name)
			return ctrl.Result{}, err
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

func (r *NodeReconciler) markPodDisruptionTarget(ctx context.Context, pod *corev1.Pod) error {
	updated := pod.DeepCopy()
	if !podutil.UpdatePodCondition(&updated.Status, &corev1.PodCondition{
		Type:    corev1.DisruptionTarget,
		Status:  corev1.ConditionTrue,
		Reason:  "EvictionAttempt",
		Message: "eviction attempt anticipated by node cordon",
	}) {
		return nil
	}
	// A full Update would clear metadata omitted from the compact cache. Patch
	// only the changed status and reject stale snapshots rather than lose updates.
	patch := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
	return r.Status().Patch(ctx, updated, patch)
}

func (r *NodeReconciler) listEvictionAutoScalersForPods(
	ctx context.Context, pods []corev1.Pod,
) (map[string][]pdbautoscaler.EvictionAutoScaler, error) {
	autoscalersByNamespace := make(map[string][]pdbautoscaler.EvictionAutoScaler)
	for _, pod := range pods {
		if _, loaded := autoscalersByNamespace[pod.Namespace]; loaded {
			continue
		}
		var autoscalers pdbautoscaler.EvictionAutoScalerList
		if err := r.List(ctx, &autoscalers, client.InNamespace(pod.Namespace)); err != nil {
			log.FromContext(ctx).Error(err, "Unable to list EvictionAutoScalers", "namespace", pod.Namespace)
			return nil, err
		}
		// Keep an owned snapshot: Update refreshes these objects' resource versions
		// for subsequent Pods matching the same EAS within this reconciliation.
		autoscalersByNamespace[pod.Namespace] = autoscalers.DeepCopy().Items
	}
	return autoscalersByNamespace, nil
}

func (r *NodeReconciler) findMatchingEvictionAutoScaler(
	ctx context.Context, pod *corev1.Pod, autoscalers []pdbautoscaler.EvictionAutoScaler,
) (*pdbautoscaler.EvictionAutoScaler, bool, error) {
	logger := log.FromContext(ctx)
	for i := range autoscalers {
		autoscaler := &autoscalers[i]
		var pdb policyv1.PodDisruptionBudget
		if err := r.Get(ctx, types.NamespacedName{Name: autoscaler.Name, Namespace: autoscaler.Namespace}, &pdb); err != nil {
			if errors.IsNotFound(err) {
				logger.Error(err, "no matching pdb", "namespace", autoscaler.Namespace, "name", autoscaler.Name)
				continue
			}
			return nil, false, err
		}
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			logger.Error(err, "Error: Invalid PDB selector", "pdbname", autoscaler.Name)
			continue
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			return autoscaler, true, nil
		}
	}
	return nil, false, nil
}

func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// IndexField itself creates a Pod informer. Register it only for cached
	// reads; direct reads use the API server's spec.nodeName field selector instead.
	if r.PodCacheEnabled {
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, NodeNameIndex, func(rawObj client.Object) []string {
			pod := rawObj.(*corev1.Pod)
			if pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}); err != nil {
			return err
		}
	}

	// With no concurrency override, controller-runtime uses one node worker.
	// Other controllers may independently issue Pod reads at the same time.
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		WithEventFilter(predicate.Funcs{
			// ignore status updates as we only care about cordon.
			UpdateFunc: func(ue event.UpdateEvent) bool {
				oldNode := ue.ObjectOld.(*corev1.Node)
				newNode := ue.ObjectNew.(*corev1.Node)
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
