package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	myappsv1 "github.com/Javier090/k8s-pdb-autoscaler/api/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func main() {

	// Set up controller-runtime logging
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// Initialize the scheme
	scheme := runtime.NewScheme()

	// Add core Kubernetes types to the scheme
	if err := corev1.AddToScheme(scheme); err != nil {
		log.Fatalf("unable to add core Kubernetes resources to scheme: %v", err)
	}

	// Add PodDisruptionBudget to the scheme
	if err := policyv1.AddToScheme(scheme); err != nil {
		log.Fatalf("unable to add PodDisruptionBudget to scheme: %v", err)
	}

	// Add custom resource types to the scheme
	if err := myappsv1.AddToScheme(scheme); err != nil {
		log.Fatalf("unable to add custom resource to scheme: %v", err)
	}

	// Configure the manager without unsupported fields
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           scheme,
		LeaderElection:   false,
		LeaderElectionID: "eviction-webhook",
	})

	if err != nil {
		log.Printf("Unable to create manager: %v", err)
		os.Exit(1)
	}

	// Configure the webhook server
	hookServer := webhook.NewServer(webhook.Options{
		Port:    9443,
		CertDir: "/ect/webhook/certs",
	})

	// Register the webhook handler
	hookServer.Register("/validate-eviction", &admission.Webhook{
		Handler: &EvictionHandler{
			Client: mgr.GetClient(),
		},
	})

	// Add the webhook server to the manager
	if err := mgr.Add(hookServer); err != nil {
		log.Printf("Unable to add webhook server to manager: %v", err)
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Printf("Problem running manager: %v", err)
		os.Exit(1)
	}
}

type EvictionHandler struct {
	Client  client.Client
	decoder *admission.Decoder
}

func (e *EvictionHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log.Printf("Received eviction request, namespace: %s, name: %s", req.Namespace, req.Name)

	// Log eviction request
	evictionLog := myappsv1.EvictionLog{
		PodName:      req.Name,
		EvictionTime: time.Now().Format(time.RFC3339),
	}

	// Fetch the pod to get its labels
	pod := &corev1.Pod{}
	err := e.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, pod)
	if err != nil {
		log.Printf("Error: Unable to fetch Pod: %v", err)
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// List all PDBWatchers in the namespace
	pdbWatcherList := &myappsv1.PDBWatcherList{}
	err = e.Client.List(ctx, pdbWatcherList, &client.ListOptions{Namespace: req.Namespace})
	if err != nil {
		log.Printf("Error: Unable to list PDBWatchers: %v", err)
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Find the applicable PDBWatcher
	var applicablePDBWatcher *myappsv1.PDBWatcher
	for _, pdbWatcher := range pdbWatcherList.Items {
		// Fetch the associated PDB
		pdb := &policyv1.PodDisruptionBudget{}
		err := e.Client.Get(ctx, types.NamespacedName{Name: pdbWatcher.Spec.PDBName, Namespace: pdbWatcher.Namespace}, pdb)
		if err != nil {
			log.Printf("Error: Unable to fetch PDB: %v, pdbName: %s", err, pdbWatcher.Spec.PDBName)
			continue
		}

		// Check if the PDB selector matches the evicted pod's labels
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			log.Printf("Error: Invalid PDB selector: %v, pdbName: %s", err, pdbWatcher.Spec.PDBName)
			continue
		}

		if selector.Matches(labels.Set(pod.Labels)) {
			applicablePDBWatcher = &pdbWatcher
			break
		}
	}

	if applicablePDBWatcher == nil {
		log.Println("No applicable PDBWatcher found")
		return admission.Allowed("no applicable PDBWatcher")
	}

	// Update the PDBWatcher status with the new eviction log
	// Ensure the eviction log does not grow indefinitely
	maxLogs := 100
	if len(applicablePDBWatcher.Status.EvictionLogs) >= maxLogs {
		applicablePDBWatcher.Status.EvictionLogs = applicablePDBWatcher.Status.EvictionLogs[1:]
	}
	applicablePDBWatcher.Status.EvictionLogs = append(applicablePDBWatcher.Status.EvictionLogs, evictionLog)

	err = e.Client.Status().Update(ctx, applicablePDBWatcher)
	if err != nil {
		log.Printf("Error: Unable to update PDBWatcher status: %v", err)
		return admission.Errored(http.StatusInternalServerError, err)
	}

	log.Printf("Eviction logged successfully, podName: %s, evictionTime: %s", req.Name, evictionLog.EvictionTime)
	return admission.Allowed("eviction allowed")
}

func (e *EvictionHandler) InjectDecoder(d *admission.Decoder) error {
	e.decoder = d
	return nil
}
