package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPodClientOptions(t *testing.T) {
	for _, mode := range []struct {
		name    string
		enabled bool
	}{
		{name: "direct", enabled: false},
		{name: "cached", enabled: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			cachedPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "from-cache", Namespace: "workloads",
					Labels: map[string]string{"app": "demo"},
				},
				Spec: corev1.PodSpec{NodeName: "node-a"},
			}
			cached := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(cachedPod, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cached-node"}}).
				WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
					return []string{obj.(*corev1.Pod).Spec.NodeName}
				}).Build()
			apiPod := cachedPod.DeepCopy()
			apiPod.Name = "from-api"
			requests := make(chan *http.Request, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(&corev1.PodList{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
					Items:    []corev1.Pod{*apiPod},
				}); err != nil {
					t.Errorf("encode Pod response: %v", err)
				}
			}))
			defer server.Close()

			mapper := meta.NewDefaultRESTMapper(nil)
			mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
			mapper.Add(corev1.SchemeGroupVersion.WithKind("Node"), meta.RESTScopeRoot)
			options := podClientOptions(mode.enabled)
			options.Scheme = scheme
			options.Mapper = mapper
			if options.Cache == nil {
				options.Cache = &client.CacheOptions{}
			}
			options.Cache.Reader = cached
			c, err := client.New(&rest.Config{Host: server.URL}, options)
			if err != nil {
				t.Fatal(err)
			}

			selector := labels.SelectorFromSet(labels.Set{"app": "demo"})
			for _, query := range []struct {
				name          string
				options       []client.ListOption
				path          string
				fieldSelector string
				labelSelector string
			}{
				{
					name:          "node",
					options:       []client.ListOption{client.MatchingFields{"spec.nodeName": "node-a"}},
					path:          "/api/v1/pods",
					fieldSelector: "spec.nodeName=node-a",
				},
				{
					name: "deployment-discovery",
					options: []client.ListOption{
						&client.ListOptions{Namespace: "workloads", LabelSelector: selector},
					},
					path:          "/api/v1/namespaces/workloads/pods",
					labelSelector: "app=demo",
				},
				{
					name: "displaced-pod-count",
					options: []client.ListOption{
						client.InNamespace("workloads"),
						client.MatchingLabelsSelector{Selector: selector},
					},
					path:          "/api/v1/namespaces/workloads/pods",
					labelSelector: "app=demo",
				},
			} {
				t.Run(query.name, func(t *testing.T) {
					var pods corev1.PodList
					if err := c.List(ctx, &pods, query.options...); err != nil {
						t.Fatal(err)
					}
					expectedName := apiPod.Name
					if mode.enabled {
						expectedName = cachedPod.Name
					}
					if len(pods.Items) != 1 || pods.Items[0].Name != expectedName {
						t.Fatalf("expected Pod %q, got %v", expectedName, pods.Items)
					}
					select {
					case request := <-requests:
						if mode.enabled {
							t.Fatal("cached Pod list unexpectedly called the API server")
						}
						if request.Method != http.MethodGet || request.URL.Path != query.path {
							t.Errorf("unexpected request: %s %s", request.Method, request.URL)
						}
						if got := request.URL.Query().Get("fieldSelector"); got != query.fieldSelector {
							t.Errorf("field selector = %q, want %q", got, query.fieldSelector)
						}
						if got := request.URL.Query().Get("labelSelector"); got != query.labelSelector {
							t.Errorf("label selector = %q, want %q", got, query.labelSelector)
						}
					default:
						if !mode.enabled {
							t.Fatal("direct Pod list did not call the API server")
						}
					}
				})
			}

			var nodes corev1.NodeList
			if err := c.List(ctx, &nodes); err != nil {
				t.Fatal(err)
			}
			if len(nodes.Items) != 1 || nodes.Items[0].Name != "cached-node" {
				t.Fatalf("expected Nodes to remain cache-backed, got %v", nodes.Items)
			}
			select {
			case request := <-requests:
				t.Fatalf("Node list unexpectedly called the API server: %s", request.URL)
			default:
			}
		})
	}
}
