package namespacefilter

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func init() {
	// Set up test logger
	logf.SetLogger(zap.New(zap.UseDevMode(true)))
}

func TestFilter_OptOut_NoAnnotation(t *testing.T) {
	// opt-out mode (optin=false): namespaces enabled by default
	filter := New([]string{}, false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-namespace",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	ctx := context.Background()

	result, err := filter.Filter(ctx, fakeClient, "test-namespace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// In opt-out mode with no annotation, optin=false means disabled by default
	if result != false {
		t.Errorf("expected false (opt-out default = optin value = false), got %v", result)
	}
}

func TestFilter_OptOut_AnnotationTrue(t *testing.T) {
	// opt-out mode: annotation=true should enable
	filter := New([]string{}, false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-namespace",
			Annotations: map[string]string{
				EnableEvictionAutoscalerAnnotationKey: "true",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	ctx := context.Background()

	result, err := filter.Filter(ctx, fakeClient, "test-namespace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != true {
		t.Errorf("expected true (annotation=true overrides), got %v", result)
	}
}

func TestFilter_OptOut_AnnotationFalse(t *testing.T) {
	// opt-out mode: annotation=false should disable
	filter := New([]string{}, false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-namespace",
			Annotations: map[string]string{
				EnableEvictionAutoscalerAnnotationKey: "false",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	ctx := context.Background()

	result, err := filter.Filter(ctx, fakeClient, "test-namespace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != false {
		t.Errorf("expected false (annotation=false), got %v", result)
	}
}

func TestFilter_OptOut_HardcodedNamespace(t *testing.T) {
	// opt-out mode with hardcoded list: hardcoded namespaces flip the default
	// optin=false, hardcoded=["special"] -> special returns !false = true
	filter := New([]string{"special"}, false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "special",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	ctx := context.Background()

	result, err := filter.Filter(ctx, fakeClient, "special")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != true {
		t.Errorf("expected true (hardcoded flips optin=false to true), got %v", result)
	}
}

func TestFilter_OptOut_HardcodedWithAnnotation(t *testing.T) {
	// annotation takes precedence over hardcoded list
	filter := New([]string{"special"}, false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "special",
			Annotations: map[string]string{
				EnableEvictionAutoscalerAnnotationKey: "false",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	ctx := context.Background()

	result, err := filter.Filter(ctx, fakeClient, "special")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != false {
		t.Errorf("expected false (annotation takes precedence), got %v", result)
	}
}

func TestFilter_OptIn_NoAnnotation(t *testing.T) {
	// opt-in mode (optin=true): namespaces use default optin value
	filter := New([]string{}, true)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-namespace",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	ctx := context.Background()

	result, err := filter.Filter(ctx, fakeClient, "test-namespace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// In opt-in mode, default returns optin value = true
	if result != true {
		t.Errorf("expected true (opt-in default = optin value = true), got %v", result)
	}
}

func TestFilter_OptIn_AnnotationTrue(t *testing.T) {
	// opt-in mode: annotation=true enables
	filter := New([]string{}, true)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-namespace",
			Annotations: map[string]string{
				EnableEvictionAutoscalerAnnotationKey: "true",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	ctx := context.Background()

	result, err := filter.Filter(ctx, fakeClient, "test-namespace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != true {
		t.Errorf("expected true (annotation=true), got %v", result)
	}
}

func TestFilter_OptIn_HardcodedNamespace(t *testing.T) {
	// opt-in mode with hardcoded list: hardcoded namespaces flip the default
	// optin=true, hardcoded=["kube-system"] -> kube-system returns !true = false
	filter := New([]string{"kube-system"}, true)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	ctx := context.Background()

	result, err := filter.Filter(ctx, fakeClient, "kube-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Hardcoded with optin=true flips to !true = false
	if result != false {
		t.Errorf("expected false (hardcoded flips optin=true to false), got %v", result)
	}
}

func TestFilter_InvalidAnnotationValue(t *testing.T) {
	filter := New([]string{}, false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-namespace",
			Annotations: map[string]string{
				EnableEvictionAutoscalerAnnotationKey: "invalid",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	ctx := context.Background()

	_, err := filter.Filter(ctx, fakeClient, "test-namespace")
	if err == nil {
		t.Error("expected error for invalid annotation value, got nil")
	}
}

func TestFilter_NamespaceNotFound(t *testing.T) {
	filter := New([]string{}, false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	_, err := filter.Filter(ctx, fakeClient, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent namespace, got nil")
	}
}

func TestFilter_RealWorldScenario_OptOutWithKubeSystem(t *testing.T) {
	// Real-world: opt-out mode with kube-system in hardcoded list
	// This enables kube-system by default, disables others
	filter := New([]string{"kube-system"}, false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	kubeSystemNs := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system",
		},
	}

	userNs := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "user-app",
		},
	}

	userOptInNs := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "user-optin",
			Annotations: map[string]string{
				EnableEvictionAutoscalerAnnotationKey: "true",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(kubeSystemNs, userNs, userOptInNs).
		Build()
	ctx := context.Background()

	// kube-system should be enabled (hardcoded flips false to true)
	result, err := filter.Filter(ctx, fakeClient, "kube-system")
	if err != nil {
		t.Fatalf("unexpected error for kube-system: %v", err)
	}
	if result != true {
		t.Errorf("kube-system: expected true, got %v", result)
	}

	// user-app should be disabled (default opt-out = false)
	result, err = filter.Filter(ctx, fakeClient, "user-app")
	if err != nil {
		t.Fatalf("unexpected error for user-app: %v", err)
	}
	if result != false {
		t.Errorf("user-app: expected false, got %v", result)
	}

	// user-optin should be enabled (annotation overrides)
	result, err = filter.Filter(ctx, fakeClient, "user-optin")
	if err != nil {
		t.Fatalf("unexpected error for user-optin: %v", err)
	}
	if result != true {
		t.Errorf("user-optin: expected true, got %v", result)
	}
}
