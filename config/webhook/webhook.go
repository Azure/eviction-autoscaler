package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	myappsv1 "github.com/Javier090/k8s-pdb-autoscaler/api/v1"
)

var (
	logger logr.Logger
)

func main() {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})
	if err != nil {
		logger.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	server := mgr.GetWebhookServer()
	server.Register("/validate-eviction", &admission.Webhook{Handler: &EvictionHandler{
		Client: mgr.GetClient(),
	}})

	logger = mgr.GetLogger().WithName("webhook")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "Problem running manager")
		os.Exit(1)
	}
}

type EvictionHandler struct {
	Client  client.Client
	decoder *admission.Decoder
}

func (e *EvictionHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger.Info("Received eviction request", "namespace", req.Namespace, "name", req.Name)

	// Fetch the Pod
	pod := &corev1.Pod{}
	err := e.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Find the corresponding PDBWatcher
	pdbWatcher := &myappsv1.PDBWatcher{}
	err = e.Client.Get(ctx, types.NamespacedName{Name: pod.Labels["pdb-watcher"], Namespace: req.Namespace}, pdbWatcher)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Add eviction log to PDBWatcher
	evictionLog := myappsv1.EvictionLog{
		PodName:      pod.Name,
		EvictionTime: time.Now().Format(time.RFC3339),
		Status:       "Evicted",
	}
	pdbWatcher.Status.EvictionLogs = append(pdbWatcher.Status.EvictionLogs, evictionLog)

	// Update the PDBWatcher status
	err = e.Client.Status().Update(ctx, pdbWatcher)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Logs eviction request
	fmt.Printf("Eviction logged for Pod %s/%s\n", req.Namespace, req.Name)

	return admission.Allowed("eviction allowed")
}

func (e *EvictionHandler) InjectDecoder(d *admission.Decoder) error {
	e.decoder = d
	return nil
}
