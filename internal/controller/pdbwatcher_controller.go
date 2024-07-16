package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	myappsv1 "github.com/Javier090/k8s-pdb-autoscaler/api/v1"
)

// PDBWatcherReconciler reconciles a PDBWatcher object
type PDBWatcherReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=apps.mydomain.com,resources=pdbwatchers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.mydomain.com,resources=pdbwatchers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.mydomain.com,resources=pdbwatchers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch

func (r *PDBWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PDBWatcher instance
	pdbWatcher := &myappsv1.PDBWatcher{}
	err := r.Get(ctx, req.NamespacedName, pdbWatcher)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Fetch the PDB
	pdb := &policyv1.PodDisruptionBudget{}
	err = r.Get(ctx, types.NamespacedName{Name: pdbWatcher.Spec.PDBName, Namespace: pdbWatcher.Namespace}, pdb)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check if PDB overlaps with multiple deployments
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return ctrl.Result{}, err
	}

	podList := &corev1.PodList{}
	err = r.List(ctx, podList, &client.ListOptions{Namespace: pdbWatcher.Namespace, LabelSelector: selector})
	if err != nil {
		return ctrl.Result{}, err
	}

	deploymentMap := make(map[string]struct{})
	for _, pod := range podList.Items {
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "ReplicaSet" {
				replicaSet := &appsv1.ReplicaSet{}
				err = r.Get(ctx, types.NamespacedName{Name: ownerRef.Name, Namespace: pdbWatcher.Namespace}, replicaSet)
				if err != nil {
					return ctrl.Result{}, err
				}

				// Get the Deployment that owns this ReplicaSet
				for _, rsOwnerRef := range replicaSet.OwnerReferences {
					if rsOwnerRef.Kind == "Deployment" {
						deploymentMap[rsOwnerRef.Name] = struct{}{}
					}
				}
			}
		}
	}

	if len(deploymentMap) > 1 {
		r.Recorder.Event(pdbWatcher, corev1.EventTypeWarning, "MultipleDeployments", "PDB overlaps with multiple deployments")
		return ctrl.Result{}, fmt.Errorf("PDB %s/%s overlaps with multiple deployments", pdbWatcher.Namespace, pdbWatcher.Spec.PDBName)
	}

	// Determine the deployment name
	deploymentName := pdbWatcher.Spec.DeploymentName
	if len(deploymentMap) == 1 {
		for name := range deploymentMap {
			deploymentName = name
		}
	}

	// Fetch the Deployment
	deployment := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: pdbWatcher.Namespace}, deployment)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update status dynamically
	pdbWatcher.Status.CurrentReplicas = *deployment.Spec.Replicas
	pdbWatcher.Status.MinReplicas = *deployment.Spec.Replicas
	pdbWatcher.Status.MaxReplicas = *deployment.Spec.Replicas + deployment.Spec.Strategy.RollingUpdate.MaxSurge.IntVal
	pdbWatcher.Status.ScaleFactor = 1 // Dynamically Calculated
	err = r.Status().Update(ctx, pdbWatcher)
	if err != nil {
		logger.Error(err, "Failed to update PDBWatcher status")
		return ctrl.Result{}, err
	}

	// Check the DisruptionsAllowed field
	if pdb.Status.DisruptionsAllowed == 0 {
		// Check if there are recent evictions
		recentEviction := false
		for _, log := range pdbWatcher.Status.EvictionLogs {
			evictionTime, err := time.Parse(time.RFC3339, log.EvictionTime)
			if err != nil {
				logger.Error(err, "Failed to parse eviction time")
				continue
			}

			if time.Since(evictionTime) < 5*time.Minute {
				recentEviction = true
				break
			}
		}

		if recentEviction {
			// Scale up the Deployment
			newReplicas := pdbWatcher.Status.CurrentReplicas + pdbWatcher.Status.ScaleFactor
			if newReplicas > pdbWatcher.Status.MaxReplicas {
				newReplicas = pdbWatcher.Status.MaxReplicas
			}
			deployment.Spec.Replicas = &newReplicas
			err = r.Update(ctx, deployment)
			if err != nil {
				return ctrl.Result{}, err
			}

			// Log the scaling action
			logger.Info(fmt.Sprintf("Scaled up Deployment %s/%s to %d replicas", deployment.Namespace, deployment.Name, newReplicas))
		}
	}

	// Process Eviction Logs
	if len(pdbWatcher.Status.EvictionLogs) > 0 {
		// Clear eviction logs after processing
		pdbWatcher.Status.EvictionLogs = []myappsv1.EvictionLog{}
		err = r.Status().Update(ctx, pdbWatcher)
		if err != nil {
			logger.Error(err, "Failed to update PDBWatcher status")
			return ctrl.Result{}, err
		}
	}

	// What will return us to the original state? Assuming we need a watch on PDB for it to come off
	return ctrl.Result{}, nil
}

func (r *PDBWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&myappsv1.PDBWatcher{}).
		Complete(r)
}
