package controllers

import (
	"context"
	"fmt"

	myappsv1 "github.com/azure/eviction-autoscaler/api/v1"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// NamespaceReconciler reconciles Namespace objects and cleans up controller-managed resources
// when the eviction autoscaler annotation is disabled or removed
type NamespaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;create;delete
// +kubebuilder:rbac:groups=eviction-autoscaler.azure.com,resources=evictionautoscalers,verbs=get;list;delete

// Reconcile handles namespace annotation changes and cleans up resources when disabled
func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	
	// Fetch the Namespace
	var namespace corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &namespace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Skip kube-system namespace (always enabled by default)
	if namespace.Name == KubeSystemNamespace {
		return ctrl.Result{}, nil
	}

	// Check if eviction autoscaler is enabled for this namespace
	isEnabled, err := IsEvictionAutoscalerEnabled(ctx, r.Client, namespace.Name)
	if err != nil {
		logger.Error(err, "Failed to check if eviction autoscaler is enabled", "namespace", namespace.Name)
		return ctrl.Result{}, err
	}

	if !isEnabled {
		logger.Info("Eviction autoscaler disabled for namespace, cleaning up resources", 
			"namespace", namespace.Name)
		
		// Delete all controller-managed PDBs in this namespace
		if err := r.cleanupPDBs(ctx, namespace.Name); err != nil {
			logger.Error(err, "Failed to cleanup PDBs", "namespace", namespace.Name)
			return ctrl.Result{}, err
		}

		// Delete all EvictionAutoScalers in this namespace
		if err := r.cleanupEvictionAutoScalers(ctx, namespace.Name); err != nil {
			logger.Error(err, "Failed to cleanup EvictionAutoScalers", "namespace", namespace.Name)
			return ctrl.Result{}, err
		}
		
		logger.Info("Successfully cleaned up resources for disabled namespace", "namespace", namespace.Name)
	} else {
		logger.Info("Eviction autoscaler enabled for namespace, creating PDbs for eligible deployments", 
			"namespace", namespace.Name)
		
		// Create PDbs for all eligible deployments
		if err := r.createPDBsForDeployments(ctx, namespace.Name); err != nil {
			logger.Error(err, "Failed to create PDbs for deployments", "namespace", namespace.Name)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// cleanupPDBs deletes all PDBs owned by the controller in the given namespace
func (r *NamespaceReconciler) cleanupPDBs(ctx context.Context, namespace string) error {
	logger := log.FromContext(ctx)
	
	var pdbList policyv1.PodDisruptionBudgetList
	if err := r.List(ctx, &pdbList, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("failed to list PDBs in namespace %s: %w", namespace, err)
	}

	for _, pdb := range pdbList.Items {
		// Only delete PDBs owned by this controller
		if pdb.Annotations != nil && pdb.Annotations[PDBOwnedByAnnotationKey] == ControllerName {
			logger.Info("Deleting controller-managed PDB", 
				"namespace", pdb.Namespace, "name", pdb.Name)
			if err := r.Delete(ctx, &pdb); err != nil {
				return fmt.Errorf("failed to delete PDB %s/%s: %w", pdb.Namespace, pdb.Name, err)
			}
		}
	}
	
	return nil
}

// cleanupEvictionAutoScalers deletes all EvictionAutoScalers in the given namespace
func (r *NamespaceReconciler) cleanupEvictionAutoScalers(ctx context.Context, namespace string) error {
	logger := log.FromContext(ctx)
	
	var easList myappsv1.EvictionAutoScalerList
	if err := r.List(ctx, &easList, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("failed to list EvictionAutoScalers in namespace %s: %w", namespace, err)
	}

	for _, eas := range easList.Items {
		logger.Info("Deleting EvictionAutoScaler", 
			"namespace", eas.Namespace, "name", eas.Name)
		if err := r.Delete(ctx, &eas); err != nil {
			return fmt.Errorf("failed to delete EvictionAutoScaler %s/%s: %w", eas.Namespace, eas.Name, err)
		}
	}
	
	return nil
}

// createPDBsForDeployments creates PDbs for all eligible deployments in the namespace
func (r *NamespaceReconciler) createPDBsForDeployments(ctx context.Context, namespace string) error {
	logger := log.FromContext(ctx)
	
	var deploymentList v1.DeploymentList
	if err := r.List(ctx, &deploymentList, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("failed to list deployments in namespace %s: %w", namespace, err)
	}

	for _, deployment := range deploymentList.Items {
		// Check if PDB creation should be skipped for this deployment
		if shouldSkip, reason := ShouldSkipPDBCreation(&deployment); shouldSkip {
			logger.V(1).Info("Skipping deployment", 
				"namespace", deployment.Namespace, "deployment", deployment.Name, "reason", reason)
			continue
		}

		// Check if PDB already exists
		_, pdbExists, err := FindPDBForDeployment(ctx, r.Client, &deployment)
		if err != nil {
			return fmt.Errorf("failed to check PDB existence: %w", err)
		}

		if pdbExists {
			logger.V(1).Info("PDB already exists for deployment", 
				"namespace", deployment.Namespace, "deployment", deployment.Name)
			continue
		}

		// Create PDB for this deployment using helper function
		if err := CreatePDBForDeployment(ctx, r.Client, &deployment); err != nil {
			logger.Error(err, "Failed to create PDB for deployment", 
				"namespace", deployment.Namespace, "deployment", deployment.Name)
			// Continue to next deployment instead of failing completely
			continue
		}

		logger.Info("Created PDB for deployment after namespace re-enabled", 
			"namespace", deployment.Namespace, "deployment", deployment.Name)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		WithEventFilter(predicate.Funcs{
			// Only trigger on annotation changes
			CreateFunc: func(e event.CreateEvent) bool {
				return false // Don't trigger on namespace creation
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldNs, okOld := e.ObjectOld.(*corev1.Namespace)
				newNs, okNew := e.ObjectNew.(*corev1.Namespace)
				if !okOld || !okNew {
					return false
				}
				
				// Skip kube-system namespace
				if newNs.Name == KubeSystemNamespace {
					return false
				}
				
				// Check if enable annotation changed
				oldVal := ""
				newVal := ""
				if oldNs.Annotations != nil {
					oldVal = oldNs.Annotations[EnableEvictionAutoscalerAnnotationKey]
				}
				if newNs.Annotations != nil {
					newVal = newNs.Annotations[EnableEvictionAutoscalerAnnotationKey]
				}
				
				// Trigger if annotation changed between enabled and disabled states
				wasEnabled := oldVal == EnableEvictionAutoscalerTrue
				isEnabled := newVal == EnableEvictionAutoscalerTrue
				
				// Trigger on: enabled→disabled (cleanup) or disabled→enabled (create PDbs)
				return wasEnabled != isEnabled
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false // Namespace deletion will cascade delete resources
			},
		}).
		Complete(r)
}
