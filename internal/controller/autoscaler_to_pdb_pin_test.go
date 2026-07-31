package controllers

import (
	"context"

	"github.com/azure/eviction-autoscaler/internal/namespacefilter"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// These specs lock in that the sibling AutoscalerToPDBReconciler does not overwrite a
// PDB whose floor the eviction controller has pinned during an active surge. Without
// the guard the sibling would rewrite minAvailable to the autoscaler's floor, clobbering
// the pin and risking the re-mutation guard snapshotting a surge-derived value as the
// partner's "original". The guard under test is the evictionSurgeReplicas annotation on
// the autoscaler (isSurgeActiveOnAutoscaler).
var _ = Describe("AutoscalerToPDB pin protection during surge", func() {
	const appLabel = "sibling-pin-app"
	var (
		namespace string
		r         *AutoscalerToPDBReconciler
		ctx       = context.Background()
	)

	// setup creates a surged Deployment (7 replicas), an HPA (minReplicas=5) optionally
	// carrying the active-surge annotation, and a controller-owned PDB pinned at floor 4.
	setup := func(name string, surgeActive bool) {
		maxUnavailable := intstr.FromInt(0)
		dep := createDeployment(name, namespace, appLabel, 7, &maxUnavailable)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())

		minReplicas := int32(5)
		annotations := map[string]string{}
		if surgeActive {
			annotations[EvictionSurgeReplicasAnnotationKey] = "2"
		}
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: annotations},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: name},
				MinReplicas:    &minReplicas,
				MaxReplicas:    10,
			},
		}
		Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

		pinned := intstr.FromInt32(4)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Annotations: map[string]string{
					PDBOwnedByAnnotationKey: ControllerName, // owned -> the sibling would manage it
					AnnotationPinnedFloor:   "4",            // eviction controller's active pin
				},
			},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: &pinned,
				Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			},
		}
		Expect(k8sClient.Create(ctx, pdb)).To(Succeed())
	}

	reconcileAndGetMinAvailable := func(name string) int32 {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: namespace, Name: name}})
		Expect(err).ToNot(HaveOccurred())
		pdb := &policyv1.PodDisruptionBudget{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pdb)).To(Succeed())
		Expect(pdb.Spec.MinAvailable).NotTo(BeNil())
		return pdb.Spec.MinAvailable.IntVal
	}

	BeforeEach(func() {
		nsObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "sibling-pin-",
				Annotations:  map[string]string{namespacefilter.EnableEvictionAutoscalerAnnotationKey: "true"},
			},
		}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		namespace = nsObj.Name

		s := scheme.Scheme
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		Expect(policyv1.AddToScheme(s)).To(Succeed())
		Expect(autoscalingv2.AddToScheme(s)).To(Succeed())

		r = &AutoscalerToPDBReconciler{Client: k8sClient, Scheme: s, Filter: &deploymentTestFilter{}}
	})

	It("leaves the pinned PDB untouched when a surge is active on the autoscaler", func() {
		setup("pinned-surge", true)
		// Floor still pinned at 4 — the sibling skipped because the surge is active.
		Expect(reconcileAndGetMinAvailable("pinned-surge")).To(Equal(int32(4)))
	})

	It("would rewrite the PDB to the autoscaler floor without the surge guard (control)", func() {
		setup("pinned-nosurge", false)
		// With no active surge the sibling overwrites the pin with the HPA min (5) —
		// which is exactly the clobber the surge guard prevents during a pin.
		Expect(reconcileAndGetMinAvailable("pinned-nosurge")).To(Equal(int32(5)))
	})
})
