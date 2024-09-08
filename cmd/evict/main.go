/*
MIT LISCENCES
*/

// Note: the example only works with the code within the same release/branch.
package main

import (
	"context"
	"flag"
	"log"
	"path/filepath"

	policy "k8s.io/api/policy/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	//
	// Uncomment to load all auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth"
	//
	// Or uncomment to load specific auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

func main() {
	var kubeconfig, pod, label, namespace *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"),
			"(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	pod = flag.String("pod", "piggie", "pod to evict")
	label = flag.String("label", "", "pod to evict")
	namespace = flag.String("ns", "test", "namespace of pod to evict")
	flag.Parse()

	ctx := context.Background()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	if *label != "" {
		pods, err := clientset.CoreV1().Pods(*namespace).List(ctx, v1.ListOptions{LabelSelector: *label, Limit: 1})
		if err != nil {
			panic(err.Error())
		}
		pod = &pods.Items[0].Name
	}

	log.Printf("evicting %s/%s", *namespace, *pod)

	err = clientset.PolicyV1().Evictions(*namespace).Evict(ctx, &policy.Eviction{
		ObjectMeta: v1.ObjectMeta{
			Name:      *pod,
			Namespace: *namespace,
		},
	})

	if err != nil {
		panic(err.Error())
	}
}
