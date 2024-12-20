/*
MIT LISCENCES
*/

// Note: the example only works with the code within the same release/branch.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	policy "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
	var kubeconfig, pod, label, namespace, node *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"),
			"(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	pod = flag.String("pod", "", "pod to evict")
	label = flag.String("label", "", "pod to evict")
	node = flag.String("node", "", "evict from node")
	namespace = flag.String("ns", "test", "namespace of pod to evict")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}
	config.QPS = 100
	config.Burst = 500

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	var podsmeta []v1.ObjectMeta
	if *label != "" {
		pods, err := clientset.CoreV1().Pods(*namespace).List(ctx, v1.ListOptions{LabelSelector: *label, Limit: 1})
		if err != nil {
			panic(err.Error())
		}
		for _, p := range pods.Items {
			podsmeta = append(podsmeta, p.ObjectMeta)
		}

	} else if *node != "" {
		var podsmeta []v1.ObjectMeta
		namespaces, err := clientset.CoreV1().Namespaces().List(ctx, v1.ListOptions{})
		if err != nil {
			panic(err.Error())
		}
		for _, ns := range namespaces.Items {
			pods, err := clientset.CoreV1().Pods(ns.Name).List(ctx, v1.ListOptions{FieldSelector: "spec.nodeName=" + *node})
			if err != nil {
				panic(err.Error())
			}
			log.Printf("found %d pods on node %s", len(pods.Items), *node)
			for _, p := range pods.Items {
				podsmeta = append(podsmeta, p.ObjectMeta)
			}
		}

	} else if *pod == "" {
		//get all the pods
		pods, err := clientset.CoreV1().Pods(*namespace).List(ctx, v1.ListOptions{})
		if err != nil {
			panic(err.Error())
		}
		for _, p := range pods.Items {
			podsmeta = append(podsmeta, p.ObjectMeta)
		}
	} else {
		podsmeta = append(podsmeta, v1.ObjectMeta{
			Name:      *pod,
			Namespace: *namespace,
		})
	}

	var wg sync.WaitGroup
	for _, meta := range podsmeta {
		if strings.HasPrefix(meta.Name, "konnectivity") {
			log.Println("skipping konnectivity pod")
			continue
		}

		log.Printf("evicting %s/%s", meta.Namespace, meta.Name)
		wg.Add(1)
		go func() {
			defer wg.Done()

			for ctx.Err() == nil {
				err = clientset.PolicyV1().Evictions(meta.Namespace).Evict(ctx, &policy.Eviction{
					ObjectMeta: meta,
				})
				if err == nil {
					log.Printf("evicted %s/%s", meta.Namespace, meta.Name)
					break
				}
				if !errors.IsTooManyRequests(err) {
					log.Fatalf("failed to evict %s/%s: %v", meta.Namespace, meta.Name, err)
					break
				}
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
				}
			}
		}()
	}
	wg.Wait()

}
