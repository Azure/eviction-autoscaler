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
	var kubeconfig, pod, namespace *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	pod = flag.String("pod", "piggie", "pod to evict")
	namespace = flag.String("ns", "test", "namespace of pod to evict")
	flag.Parse()
	log.Printf("evicting %s/%s", *namespace, *pod)
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

	ctx := context.Background()
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
