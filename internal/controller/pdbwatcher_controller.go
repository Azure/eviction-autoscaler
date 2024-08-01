package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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
			return ctrl.Result{}, nil // PDBWatcher not found, could be deleted, nothing to do
		}
		return ctrl.Result{}, err // Error fetching PDBWatcher
	}

	// Check for conflicts with other PDBWatchers
	conflictWatcherList := &myappsv1.PDBWatcherList{}
	err = r.List(ctx, conflictWatcherList, &client.ListOptions{Namespace: pdbWatcher.Namespace})
	if err != nil {
		return ctrl.Result{}, err // Error listing PDBWatchers
	}

	for _, watcher := range conflictWatcherList.Items {
		if watcher.Name != pdbWatcher.Name && watcher.Spec.PDBName == pdbWatcher.Spec.PDBName {
			// Conflict detected
			errMsg := fmt.Sprintf("PDB %s is already being watched by another PDBWatcher %s", pdbWatcher.Spec.PDBName, watcher.Name)
			r.Recorder.Event(pdbWatcher, corev1.EventTypeWarning, "Conflict", errMsg)
			return ctrl.Result{}, fmt.Errorf(errMsg)
		}
	}

	// Fetch the PDB
	pdb := &policyv1.PodDisruptionBudget{}
	err = r.Get(ctx, types.NamespacedName{Name: pdbWatcher.Spec.PDBName, Namespace: pdbWatcher.Namespace}, pdb)
	if err != nil {
		return ctrl.Result{}, err // Error fetching PDB
	}

	// Check if PDB overlaps with multiple deployments
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return ctrl.Result{}, err // Error converting label selector
	}

	podList := &corev1.PodList{}
	err = r.List(ctx, podList, &client.ListOptions{Namespace: pdbWatcher.Namespace, LabelSelector: selector})
	if err != nil {
		return ctrl.Result{}, err // Error listing pods
	}

	deploymentMap := make(map[string]struct{})
	for _, pod := range podList.Items {
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "ReplicaSet" {
				replicaSet := &appsv1.ReplicaSet{}
				err = r.Get(ctx, types.NamespacedName{Name: ownerRef.Name, Namespace: pdbWatcher.Namespace}, replicaSet)
				if err != nil {
					return ctrl.Result{}, err // Error fetching ReplicaSet
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

	// If multiple deployments are found, log a warning and return an error
	if len(deploymentMap) > 1 {
		r.Recorder.Event(pdbWatcher, corev1.EventTypeWarning, "MultipleDeployments", "PDB overlaps with multiple deployments")
		return ctrl.Result{}, fmt.Errorf("PDB %s/%s overlaps with multiple deployments", pdbWatcher.Namespace, pdbWatcher.Spec.PDBName)
	}

	// Determine the deployment name
	deploymentName := pdbWatcher.Spec.DeploymentName
	if deploymentName == "" && len(deploymentMap) == 1 {
		for name := range deploymentMap {
			deploymentName = name
		}
	}

	// Log the deployment map and deployment name for debugging
	logger.Info(fmt.Sprintf("Deployment map: %v", deploymentMap))
	logger.Info(fmt.Sprintf("Determined Deployment name: %s", deploymentName))

	// Validate the deployment name
	if deploymentName == "" {
		errMsg := "Deployment name is empty"
		logger.Error(fmt.Errorf(errMsg), errMsg)
		return ctrl.Result{}, fmt.Errorf(errMsg)
	}

	// Fetch the Deployment
	deployment := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: pdbWatcher.Namespace}, deployment)
	if err != nil {
		return ctrl.Result{}, err // Error fetching Deployment
	}

	// Check if the resource version has changed or if it's empty (initial state)
	if pdbWatcher.Status.ResourceVersion == "" || pdbWatcher.Status.ResourceVersion != deployment.ResourceVersion {
		// The resource version has changed, which means someone else has modified the Deployment.
		// To avoid conflicts, we update our status to reflect the new state and avoid making further changes.
		pdbWatcher.Status.ResourceVersion = deployment.ResourceVersion
		pdbWatcher.Status.MinReplicas = *deployment.Spec.Replicas
		err = r.Status().Update(ctx, pdbWatcher)
		if err != nil {
			logger.Error(err, "Failed to update PDBWatcher status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Track initial state to detect conflicts
	initialResourceVersion := deployment.ResourceVersion
	initialReplicas := *deployment.Spec.Replicas

	// Update status dynamically
	if pdbWatcher.Status.CurrentReplicas == 0 {
		pdbWatcher.Status.CurrentReplicas = initialReplicas
	}
	pdbWatcher.Status.MinReplicas = initialReplicas

	// Handle nil Deployment Strategy and MaxSurge
	maxSurge := int32(1) // Default max surge value
	if deployment.Spec.Strategy.RollingUpdate != nil && deployment.Spec.Strategy.RollingUpdate.MaxSurge != nil {
		if deployment.Spec.Strategy.RollingUpdate.MaxSurge.Type == intstr.Int {
			maxSurge = deployment.Spec.Strategy.RollingUpdate.MaxSurge.IntVal
		} else if deployment.Spec.Strategy.RollingUpdate.MaxSurge.Type == intstr.String {
			percentageStr := strings.TrimSuffix(deployment.Spec.Strategy.RollingUpdate.MaxSurge.StrVal, "%")
			percentage, err := strconv.Atoi(percentageStr)
			if err == nil {
				maxSurge = (pdbWatcher.Status.MinReplicas * int32(percentage)) / 100
			}
		}
	}
	pdbWatcher.Status.MaxReplicas = pdbWatcher.Status.MinReplicas + maxSurge

	pdbWatcher.Status.ScaleFactor = 1 // Modify based on requirements

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

			// Save ResourceVersion to PDBWatcher status
			pdbWatcher.Status.ResourceVersion = initialResourceVersion

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

	// Watch for changes in PDB to revert to original state
	if pdb.Status.DisruptionsAllowed > 0 && *deployment.Spec.Replicas != pdbWatcher.Status.MinReplicas {
		// Check if the resource version has changed
		if pdbWatcher.Status.ResourceVersion != deployment.ResourceVersion {
			// Deployment has been modified externally, update the resource version and min replicas
			pdbWatcher.Status.ResourceVersion = deployment.ResourceVersion
			pdbWatcher.Status.MinReplicas = *deployment.Spec.Replicas
			err = r.Status().Update(ctx, pdbWatcher)
			if err != nil {
				logger.Error(err, "Failed to update PDBWatcher status")
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		// Revert Deployment to the original state
		deployment.Spec.Replicas = &pdbWatcher.Status.MinReplicas
		err = r.Update(ctx, deployment)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Log the scaling action
		logger.Info(fmt.Sprintf("Reverted Deployment %s/%s to %d replicas", deployment.Namespace, deployment.Name, *deployment.Spec.Replicas))

		// Update ResourceVersion in PDBWatcher status
		pdbWatcher.Status.ResourceVersion = deployment.ResourceVersion
		err = r.Status().Update(ctx, pdbWatcher)
		if err != nil {
			logger.Error(err, "Failed to update PDBWatcher status")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *PDBWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&myappsv1.PDBWatcher{}).
		Complete(r)
}
