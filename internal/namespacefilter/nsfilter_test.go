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

// Opt-In Mode Tests (optin=true, ENABLED_BY_DEFAULT=false)
// Default: disabled, hardcoded list enables specific namespaces

func TestFilter_OptIn_NoAnnotation_NotHardcoded(t *testing.T) {
	// opt-in mode (optin=true): regular namespaces disabled by default
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

	if result != false {
		t.Errorf("expected false (opt-in mode: disabled by default), got %v", result)
	}
}

func TestFilter_OptIn_AnnotationTrue(t *testing.T) {
	// opt-in mode: annotation=true should enable
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

func TestFilter_OptIn_AnnotationFalse(t *testing.T) {
	// opt-in mode: annotation=false should disable
	filter := New([]string{}, true)

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

func TestFilter_OptIn_HardcodedNamespace(t *testing.T) {
	// opt-in mode with hardcoded list: hardcoded namespaces are enabled
	// optin=true, hardcoded=["kube-system"] -> kube-system returns true
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

	if result != true {
		t.Errorf("expected true (opt-in mode: hardcoded namespace enabled), got %v", result)
	}
}

func TestFilter_OptIn_HardcodedWithAnnotationFalse(t *testing.T) {
	// annotation takes precedence over hardcoded list
	filter := New([]string{"kube-system"}, true)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system",
			Annotations: map[string]string{
				EnableEvictionAutoscalerAnnotationKey: "false",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	ctx := context.Background()

	result, err := filter.Filter(ctx, fakeClient, "kube-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != false {
		t.Errorf("expected false (annotation takes precedence over hardcoded), got %v", result)
	}
}

// Opt-Out Mode Tests (optin=false, ENABLED_BY_DEFAULT=true)
// Default: enabled, hardcoded list is ignored, only annotations can disable

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

	if result != true {
		t.Errorf("expected true (opt-out mode: enabled by default), got %v", result)
	}
}

func TestFilter_OptOut_AnnotationTrue(t *testing.T) {
	// opt-out mode: annotation=true should enable (redundant but explicit)
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
		t.Errorf("expected true (annotation=true), got %v", result)
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

func TestFilter_OptOut_HardcodedNamespace_Ignored(t *testing.T) {
	// opt-out mode: hardcoded list is ignored, namespace still enabled by default
	// optin=false, hardcoded=["special"] -> special returns true (hardcoded ignored in opt-out)
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
		t.Errorf("expected true (opt-out mode: hardcoded list ignored, enabled by default), got %v", result)
	}
}

func TestFilter_OptOut_HardcodedWithAnnotation(t *testing.T) {
	// opt-out mode: annotation takes precedence (hardcoded list ignored anyway)
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

// Edge Cases

func TestFilter_InvalidAnnotationValue(t *testing.T) {
	filter := New([]string{}, true)

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
	filter := New([]string{}, true)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	_, err := filter.Filter(ctx, fakeClient, "nonexistent-namespace")
	if err == nil {
		t.Error("expected error for nonexistent namespace, got nil")
	}
}

// Real-world Scenarios

func TestFilter_RealWorldScenario_OptInWithKubeSystem(t *testing.T) {
	// Real scenario: opt-in mode with kube-system in hardcoded list
	// ENABLED_BY_DEFAULT=false, ACTIONED_NAMESPACES=kube-system
	filter := New([]string{"kube-system"}, true)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	// kube-system should be enabled (in hardcoded list)
	kubeSystemNs := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system",
		},
	}

	// regular namespace should be disabled (not in hardcoded list, no annotation)
	regularNs := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
		},
	}

	// namespace with annotation should follow annotation
	annotatedNs := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "production",
			Annotations: map[string]string{
				EnableEvictionAutoscalerAnnotationKey: "true",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(kubeSystemNs, regularNs, annotatedNs).Build()
	ctx := context.Background()

	// Test kube-system (should be enabled)
	result, err := filter.Filter(ctx, fakeClient, "kube-system")
	if err != nil {
		t.Fatalf("unexpected error for kube-system: %v", err)
	}
	if result != true {
		t.Errorf("expected kube-system to be enabled in opt-in mode with hardcoded list, got %v", result)
	}

	// Test default (should be disabled)
	result, err = filter.Filter(ctx, fakeClient, "default")
	if err != nil {
		t.Fatalf("unexpected error for default: %v", err)
	}
	if result != false {
		t.Errorf("expected default to be disabled in opt-in mode, got %v", result)
	}

	// Test production (should be enabled via annotation)
	result, err = filter.Filter(ctx, fakeClient, "production")
	if err != nil {
		t.Fatalf("unexpected error for production: %v", err)
	}
	if result != true {
		t.Errorf("expected production to be enabled via annotation, got %v", result)
	}
}

func TestFilter_RealWorldScenario_OptOut(t *testing.T) {
	// Real scenario: opt-out mode, all namespaces enabled by default
	// ENABLED_BY_DEFAULT=true, ACTIONED_NAMESPACES ignored
	filter := New([]string{"ignored"}, false)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	// All namespaces should be enabled by default
	ns1 := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
		},
	}

	ns2 := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system",
		},
	}

	// Namespace with annotation=false should be disabled
	disabledNs := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "disabled",
			Annotations: map[string]string{
				EnableEvictionAutoscalerAnnotationKey: "false",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns1, ns2, disabledNs).Build()
	ctx := context.Background()

	// Test default (should be enabled)
	result, err := filter.Filter(ctx, fakeClient, "default")
	if err != nil {
		t.Fatalf("unexpected error for default: %v", err)
	}
	if result != true {
		t.Errorf("expected default to be enabled in opt-out mode, got %v", result)
	}

	// Test kube-system (should be enabled)
	result, err = filter.Filter(ctx, fakeClient, "kube-system")
	if err != nil {
		t.Fatalf("unexpected error for kube-system: %v", err)
	}
	if result != true {
		t.Errorf("expected kube-system to be enabled in opt-out mode, got %v", result)
	}

	// Test disabled (should be disabled via annotation)
	result, err = filter.Filter(ctx, fakeClient, "disabled")
	if err != nil {
		t.Fatalf("unexpected error for disabled: %v", err)
	}
	if result != false {
		t.Errorf("expected disabled to be disabled via annotation, got %v", result)
	}
}
