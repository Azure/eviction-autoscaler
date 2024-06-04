package controllers

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	myappsv1 "github.com/Javier090/k8s-pdb-autoscaler/api/v1"
)

// PDBWatcherReconciler reconciles a PDBWatcher object
type PDBWatcherReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apps.mydomain.com,resources=pdbwatchers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.mydomain.com,resources=pdbwatchers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.mydomain.com,resources=pdbwatchers/finalizers,verbs=update

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
	err = r.Get(ctx, types.NamespacedName{Name: pdbWatcher.Name, Namespace: pdbWatcher.Namespace}, pdb)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check the DisruptionsAllowed field
	if pdb.Status.DisruptionsAllowed == 0 {
		// Fetch the Deployment
		deployment := &appsv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: pdbWatcher.Name, Namespace: pdbWatcher.Namespace}, deployment)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Scale up the Deployment
		newReplicas := *deployment.Spec.Replicas + 1
		deployment.Spec.Replicas = &newReplicas
		err = r.Update(ctx, deployment)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Log the scaling action
		logger.Info(fmt.Sprintf("Scaled up Deployment %s/%s to %d replicas", deployment.Namespace, deployment.Name, newReplicas))
	}

	return ctrl.Result{}, nil
}

func (r *PDBWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&myappsv1.PDBWatcher{}).
		Complete(r)
}
