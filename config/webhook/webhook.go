package main

import (
	"context"
	"net/http"
	"os"
	"time"

	myappsv1 "github.com/Javier090/k8s-pdb-autoscaler/api/v1"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
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

	// Log eviction request
	evictionLog := myappsv1.EvictionLog{
		PodName:      req.Name,
		EvictionTime: time.Now().Format(time.RFC3339),
	}

	// Fetch the PDBWatcher instance
	pdbWatcher := &myappsv1.PDBWatcher{}
	err := e.Client.Get(ctx, types.NamespacedName{Name: "example-pdbwatcher", Namespace: req.Namespace}, pdbWatcher)
	if err != nil {
		logger.Error(err, "Unable to fetch PDBWatcher")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Update the PDBWatcher status with the new eviction log
	pdbWatcher.Status.EvictionLogs = append(pdbWatcher.Status.EvictionLogs, evictionLog)
	err = e.Client.Status().Update(ctx, pdbWatcher)
	if err != nil {
		logger.Error(err, "Unable to update PDBWatcher status")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.Allowed("eviction allowed")
}

func (e *EvictionHandler) InjectDecoder(d *admission.Decoder) error {
	e.decoder = d
	return nil
}
