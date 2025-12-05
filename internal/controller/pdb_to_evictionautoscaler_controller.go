package controllers

import (
	"context"
	"fmt"

	types "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/internal/metrics"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8s_types "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var errOwnerNotFound error = fmt.Errorf("owner not found")

// PDBToEvictionAutoScalerReconciler reconciles a PodDisruptionBudget object.
type PDBToEvictionAutoScalerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Filter   filter
}

// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;create;watch;update
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;update;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;update;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

// Reconcile reads the state of the cluster for a PDB and creates/deletes EvictionAutoScalers accordingly.
func (r *PDBToEvictionAutoScalerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.WithValues("pdb", req.Name, "namespace", req.Namespace)
	ctx = log.IntoContext(ctx, logger)
	// Fetch the PodDisruptionBudget object based on the reconcile request
	var pdb policyv1.PodDisruptionBudget
	err := r.Get(ctx, req.NamespacedName, &pdb)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Handle ownership transfer based on ownedBy annotation
	err = r.handleOwnershipTransfer(ctx, &pdb)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Update PDB metrics to check if this PDB was created by our deployment controller
	createdByUsStr := metrics.GetPDBCreatedByUsLabel(pdb.Annotations)
	// Track PDB existence
	metrics.PDBCounter.WithLabelValues(pdb.Namespace, createdByUsStr).Inc()

	// Check if eviction autoscaler should be enabled for this PDB
	isEnabled, err := r.Filter.Filter(ctx, r.Client, pdb.Namespace)
	if err != nil {
		logger.Error(err, "Failed to check if eviction autoscaler is enabled", "namespace", pdb.Namespace)
		return reconcile.Result{}, err
	}
	if !isEnabled {
		logger.V(1).Info("Eviction autoscaler not enabled for namespace", "namespace", pdb.Namespace)
		// Only delete EvictionAutoScaler for user-owned PDbs
		// Controller-owned PDbs will be deleted by DeploymentToPDBReconciler, which cascade-deletes the EvictionAutoScaler
		isControllerOwned := pdb.Annotations != nil && pdb.Annotations[PDBOwnedByAnnotationKey] == ControllerName
		if !isControllerOwned {
			var eas types.EvictionAutoScaler
			err = r.Get(ctx, req.NamespacedName, &eas)
			if err == nil {
				logger.Info("Deleting EvictionAutoScaler for user-owned PDB in disabled namespace", "eas", eas.Name)
				if err := r.Delete(ctx, &eas); err != nil {
					return reconcile.Result{}, client.IgnoreNotFound(err)
				}
			}
		}
		return reconcile.Result{}, nil
	}

	// If the PDB exists, create a corresponding EvictionAutoScaler if it does not exist
	var EvictionAutoScaler types.EvictionAutoScaler
	err = r.Get(ctx, req.NamespacedName, &EvictionAutoScaler)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		deploymentName, _, e := r.discoverDeployment(ctx, &pdb)
		if e != nil {
			return reconcile.Result{}, e
		}

		// EvictionAutoScaler not found, create it
		//variables
		controller := true
		blockOwnerDeletion := true

		// Create a new EvictionAutoScaler
		EvictionAutoScaler = types.EvictionAutoScaler{
			TypeMeta: metav1.TypeMeta{
				Kind:       "EvictionAutoScaler",
				APIVersion: "eviction-autoscaler.azure.com/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      pdb.Name,
				Namespace: pdb.Namespace,
				Annotations: map[string]string{
					"ownedBy": "EvictionAutoScaler",
					"target":  deploymentName,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "policy/v1",
						Kind:               "PodDisruptionBudget",
						Name:               pdb.Name,
						UID:                pdb.UID,
						Controller:         &controller,         // Mark as managed by this controller
						BlockOwnerDeletion: &blockOwnerDeletion, // Prevent deletion of the EvictionAutoScaler until the controller is deleted
					},
				},
			},
			Spec: types.EvictionAutoScalerSpec{
				TargetName: deploymentName,
				TargetKind: deploymentKind,
			},
		}

		err := r.Create(ctx, &EvictionAutoScaler)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("unable to create EvictionAutoScaler: %v", err)
		}

		// Track EvictionAutoScaler creation
		metrics.EvictionAutoScalerCreationCounter.WithLabelValues(pdb.Namespace, pdb.Name, deploymentName).Inc()

		logger.Info("Created EvictionAutoScaler")
	}
	// Return no error and no requeue
	return reconcile.Result{}, nil
}

// handleOwnershipTransfer manages the owner reference based on the ownedBy annotation
func (r *PDBToEvictionAutoScalerReconciler) handleOwnershipTransfer(ctx context.Context, pdb *policyv1.PodDisruptionBudget) error {
	logger := log.FromContext(ctx)

	// Check if PDB has the ownedBy annotation
	hasAnnotation := pdb.Annotations != nil && pdb.Annotations[PDBOwnedByAnnotationKey] == ControllerName

	// Check if PDB has an owner reference to a deployment
	hasOwnerRef := false
	var deploymentOwnerIdx int
	for idx, ownerRef := range pdb.OwnerReferences {
		if ownerRef.Kind == ResourceTypeDeployment {
			hasOwnerRef = true
			deploymentOwnerIdx = idx
			break
		}
	}

	// Handle annotation and owner reference synchronization
	if !hasAnnotation && hasOwnerRef {
		// User removed annotation - remove owner reference to transfer ownership
		logger.Info("Removing owner reference from PDB - user has taken ownership",
			"namespace", pdb.Namespace, "name", pdb.Name)

		// Remove the deployment owner reference
		newOwnerRefs := []metav1.OwnerReference{}
		for idx, ownerRef := range pdb.OwnerReferences {
			if idx != deploymentOwnerIdx {
				newOwnerRefs = append(newOwnerRefs, ownerRef)
			}
		}
		pdb.OwnerReferences = newOwnerRefs

		if err := r.Update(ctx, pdb); err != nil {
			logger.Error(err, "Failed to remove owner reference from PDB",
				"namespace", pdb.Namespace, "name", pdb.Name)
			return err
		}
		logger.Info("Successfully removed owner reference from PDB",
			"namespace", pdb.Namespace, "name", pdb.Name)
	} else if hasAnnotation && !hasOwnerRef {
		// Annotation is present but owner reference is missing - add it back
		logger.Info("Adding owner reference to PDB - controller taking control back",
			"namespace", pdb.Namespace, "name", pdb.Name)

		deploymentName, deploymentUID, err := r.discoverDeployment(ctx, pdb)
		if err != nil {
			logger.Error(err, "Failed to get deployment",
				"namespace", pdb.Namespace, "name", deploymentName)
			return err
		}

		controller := true
		blockOwnerDeletion := true

		pdb.OwnerReferences = append(pdb.OwnerReferences, metav1.OwnerReference{
			APIVersion:         "apps/v1",
			Kind:               ResourceTypeDeployment,
			Name:               deploymentName,
			UID:                deploymentUID,
			Controller:         &controller,
			BlockOwnerDeletion: &blockOwnerDeletion,
		})

		if err := r.Update(ctx, pdb); err != nil {
			logger.Error(err, "Failed to add owner reference to PDB",
				"namespace", pdb.Namespace, "name", pdb.Name)
			return err
		}
		logger.Info("Successfully added owner reference to PDB",
			"namespace", pdb.Namespace, "name", pdb.Name)
	}

	return nil
}

func (r *PDBToEvictionAutoScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	logger := mgr.GetLogger()
	// Set up the controller to watch Deployments and trigger the reconcile function
	return ctrl.NewControllerManagedBy(mgr).
		For(&policyv1.PodDisruptionBudget{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				return nil
			}

			// Check if namespace is enabled for eviction autoscaler
			isEnabled, err := r.Filter.Filter(ctx, r.Client, ns.Name)
			if err != nil {
				logger.Error(err, "Failed to check if eviction autoscaler is enabled", "namespace", ns.Name)
				return nil
			}
			if !isEnabled {
				return nil
			}

			// List all PDBs in the namespace
			var pdbList policyv1.PodDisruptionBudgetList
			if err := r.Client.List(ctx, &pdbList, client.InNamespace(ns.Name)); err != nil {
				logger.Error(err, "Failed to list PDBs in namespace", "namespace", ns.Name)
				return nil
			}

			var requests []reconcile.Request
			for _, pdb := range pdbList.Items {
				requests = append(requests, reconcile.Request{
					NamespacedName: k8s_types.NamespacedName{
						Namespace: pdb.Namespace,
						Name:      pdb.Name,
					},
				})
			}
			return requests
		})).
		WithEventFilter(predicate.Funcs{
			// Trigger for Create and Update events
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Trigger on ownedBy annotation changes
				return triggerOnPDBAnnotationChange(e, logger)
			},
			DeleteFunc: func(e event.DeleteEvent) bool { return false },
		}).
		Owns(&types.EvictionAutoScaler{}). // Watch EvictionAutoScalers for ownership
		Complete(r)
}

func (r *PDBToEvictionAutoScalerReconciler) discoverDeployment(ctx context.Context, pdb *policyv1.PodDisruptionBudget) (string, k8s_types.UID, error) {
	logger := log.FromContext(ctx)

	// Convert PDB label selector to Kubernetes selector
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return "", "", fmt.Errorf("error converting label selector: %v", err)
	}
	logger.Info("PDB Selector", "selector", pdb.Spec.Selector)

	podList := &corev1.PodList{}
	err = r.List(ctx, podList, &client.ListOptions{Namespace: pdb.Namespace, LabelSelector: selector})
	if err != nil {
		return "", "", fmt.Errorf("error listing pods: %v", err)
	}
	logger.Info("Number of pods found", "count", len(podList.Items))

	if len(podList.Items) == 0 {
		// TODO instead of an error which leads to a backoff retry quietly for a while then error?
		return "", "", fmt.Errorf("no pods found matching the PDB selector %s; leaky pdb(?!)", pdb.Name)
	}

	// Iterate through each pod
	for _, pod := range podList.Items {
		// Check the OwnerReferences of each pod
		for _, ownerRef := range pod.OwnerReferences {
			if ownerRef.Kind == "ReplicaSet" {
				replicaSet := &appsv1.ReplicaSet{}
				err = r.Get(ctx, k8s_types.NamespacedName{Name: ownerRef.Name, Namespace: pdb.Namespace}, replicaSet)
				if apierrors.IsNotFound(err) {
					return "", "", fmt.Errorf("error fetching ReplicaSet: %v", err)
				}

				// Log ReplicaSet details
				logger.Info("Found ReplicaSet", "replicaSet", replicaSet.Name)

				// Look for the Deployment owner of the ReplicaSet
				for _, rsOwnerRef := range replicaSet.OwnerReferences {
					if rsOwnerRef.Kind == "Deployment" {
						logger.Info("Found Deployment owner", "deployment", rsOwnerRef.Name)
						return rsOwnerRef.Name, rsOwnerRef.UID, nil
					}
				}
				// no replicaset owner just move on and see if any other pods have have something.
			}
			//// Optional: Handle StatefulSets if necessary
			//if ownerRef.Kind == "StatefulSet" {
			//	statefulSet := &appsv1.StatefulSet{}
			//	err = r.Get(ctx, k8s_types.NamespacedName{Name: ownerRef.Name, Namespace: pdb.Namespace}, statefulSet)
			//	if apierrors.IsNotFound(err) {
			//		return "", "", fmt.Errorf("error fetching StatefulSet: %v", err)
			//	}
			//	logger.Info("Found StatefulSet owner", "statefulSet", statefulSet.Name)
			//	// Handle StatefulSet logic if required
			//}

		}
	}
	logger.Info("No Deployment owner found")
	return "", "", errOwnerNotFound
}

// triggerOnPDBAnnotationChange checks if a PDB update event should trigger reconciliation
// by comparing the ownedBy annotation between old and new PDB
func triggerOnPDBAnnotationChange(e event.UpdateEvent, logger logr.Logger) bool {
	oldPDB, okOld := e.ObjectOld.(*policyv1.PodDisruptionBudget)
	newPDB, okNew := e.ObjectNew.(*policyv1.PodDisruptionBudget)
	if okOld && okNew {
		oldVal := tryGet(oldPDB.Annotations, PDBOwnedByAnnotationKey)
		newVal := tryGet(newPDB.Annotations, PDBOwnedByAnnotationKey)
		if oldVal != newVal {
			logger.Info("PDB update event detected, ownedBy annotation changed",
				"namespace", newPDB.Namespace, "name", newPDB.Name,
				"oldValue", oldVal, "newValue", newVal)
			return true
		}
	}
	return false
}

// tryGet safely retrieves a value from a map, returning empty string if map is nil
func tryGet(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}
