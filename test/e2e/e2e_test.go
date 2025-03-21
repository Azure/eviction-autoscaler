/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"

	"log"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policy "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	"github.com/azure/eviction-autoscaler/test/utils"
)

// SYNC with kustomize file
const namespace = "eviction-autoscaler"
const kindClusterName = "e2e"

var cleanEnv = true

var _ = Describe("controller", Ordered, func() {
	BeforeAll(func() {
		//allow to bypass if they have one?

		if cleanEnv {
			By("creating kind cluster")
			cmd := exec.Command("kind", "create", "cluster", "--config", "test/e2e/kind.yaml", "--name", kindClusterName)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			fmt.Print(string(output))

			cmd = exec.Command("kubectl", "config", "use-context", "kind-"+kindClusterName)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			//By("installing prometheus operator")
			//Expect(utils.InstallPrometheusOperator()).To(Succeed())

			//By("installing the cert-manager")
			//Expect(utils.InstallCertManager()).To(Succeed())
			By("creating manager namespace")
			_, err = utils.Run(exec.Command("kubectl", "create", "ns", namespace))
			Expect(err).NotTo(HaveOccurred())
		}

	})

	AfterAll(func() {
		//By("uninstalling the Prometheus manager bundle")
		//utils.UninstallPrometheusOperator()

		//By("uninstalling the cert-manager bundle")
		//utils.UninstallCertManager()
		if cleanEnv {

			By("removing kind cluster")
			cmd := exec.Command("kind", "delete", "cluster", "-n", kindClusterName)
			_, _ = utils.Run(cmd)
		}
	})

	Context("Operator", func() {
		ctx := context.Background()
		It("should run successfully", func() {
			var err error

			// projectimage stores the name of the image used in the example
			var projectimage = "evictionautoscaler:e2etest"

			By("building the manager(Operator) image")
			cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectimage))
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("loading the the manager(Operator) image on Kind")
			err = utils.LoadImageToKindClusterWithName(projectimage, kindClusterName)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("installing CRDs")
			cmd = exec.Command("make", "install")
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("deploying the controller-manager")
			cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectimage))
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(homedir.HomeDir(), ".kube", "config"))
			Expect(err).NotTo(HaveOccurred())
			// create the clientset
			clientset, err := kubernetes.NewForConfig(config)
			Expect(err).NotTo(HaveOccurred())

			dynamicClient, err := dynamic.NewForConfig(config)
			if err != nil {
				log.Fatalf("Error creating dynamic Kubernetes client: %v", err)
			}

			By("validating that the controller-manager pod is running as expected")
			var nodeName string
			verifyOneRunningPod := func() error {
				pods, err := clientset.CoreV1().Pods(namespace).List(ctx, v1.ListOptions{
					LabelSelector: "control-plane=controller-manager", Limit: 1,
				})
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				if len(pods.Items) != 1 {
					return fmt.Errorf("got %d controller pods", len(pods.Items))
				}
				if pods.Items[0].Status.Phase != "Running" {
					return fmt.Errorf("controller pod in %s status", pods.Items[0].Status.Phase)
				}
				fmt.Printf("controller pod %s running on %s\n", pods.Items[0].Name, pods.Items[0].Spec.NodeName)
				nodeName = pods.Items[0].Spec.NodeName
				return nil
			}
			EventuallyWithOffset(1, verifyOneRunningPod, time.Minute, time.Second).Should(Succeed())

			By("validating that the nginx pod is running as expected")
			//var nodeName string
			verifyOneRunningNginxPod := func() error {
				pods, err := clientset.CoreV1().Pods("ingress-nginx").List(ctx, v1.ListOptions{
					LabelSelector: "app.kubernetes.io/component=controller", Limit: 1,
				})
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				if len(pods.Items) != 1 {
					return fmt.Errorf("got %d nginx pods", len(pods.Items))
				}
				if pods.Items[0].Status.Phase != "Running" {
					return fmt.Errorf("nginx pod in %s status", pods.Items[0].Status.Phase)
				}
				fmt.Printf("nginx pod %s running on %s\n", pods.Items[0].Name, pods.Items[0].Spec.NodeName)
				//nodeName = pods.Items[0].Spec.NodeName
				return nil
			}
			EventuallyWithOffset(1, verifyOneRunningNginxPod, time.Minute, time.Second).Should(Succeed())
			By("Verify PDB and PDBWatcher exist")
			verifyPdbExists := func() error {
				pdbList, err := clientset.PolicyV1().PodDisruptionBudgets("ingress-nginx").List(ctx,
					v1.ListOptions{Limit: 1})
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d pdbs in namespace ingress-nginx\n", len(pdbList.Items))
				for _, pdb := range pdbList.Items {
					fmt.Printf("found pdb name: %s \n", pdb.Name)
					if pdb.Name != "ingress-nginx-controller" {
						return fmt.Errorf("nginx pdb is not present on cluster")
					}
					if pdb.Spec.MinAvailable != nil {
						if val := pdb.Spec.MinAvailable.IntValue(); val != 1 {
							return fmt.Errorf("PDB '%s' has MinAvailable set to %d (not 1)\n", pdb.Name, val)
						}
						fmt.Printf("PDB '%s' has MinAvailable set to 1\n", pdb.Name)
					}
				}
				return nil
			}
			verifyPdbWatcherExists := func() error {
				gvr := schema.GroupVersionResource{
					Group:    "apps.mydomain.com", // API group for your custom resource
					Version:  "v1",                // API version for the custom resource
					Resource: "pdbwatchers",       // Resource name (plural form of your custom resource)
				}
				pdbWatcherList, err := dynamicClient.Resource(gvr).Namespace("ingress-nginx").List(ctx,
					v1.ListOptions{Limit: 1})
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d pdbwatchers in namespace ingress-nginx \n", len(pdbWatcherList.Items))
				for _, resource := range pdbWatcherList.Items {
					fmt.Printf("found pdbwatcher name: %s \n", resource.GetName())
					if resource.GetName() != "ingress-nginx-controller" {
						return fmt.Errorf("nginx pdbwatcher is not present on cluster")
					}
					fmt.Printf("custom resource found: %s", resource.GetName())
				}
				return nil
			}
			EventuallyWithOffset(1, verifyPdbExists, time.Minute, time.Second).Should(Succeed())
			EventuallyWithOffset(1, verifyPdbWatcherExists, time.Minute, time.Second).Should(Succeed())

			By("By Cordoning " + nodeName)
			// Cordon and drain the node that the controller-manager pod is running on
			node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, v1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			node.Spec.Unschedulable = true
			_, err = clientset.CoreV1().Nodes().Update(ctx, node, v1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("By Draining " + nodeName)
			drain := func() error {
				var podsmeta []v1.ObjectMeta
				namespaces, err := clientset.CoreV1().Namespaces().List(ctx, v1.ListOptions{})
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				for _, ns := range namespaces.Items {
					pods, err := clientset.CoreV1().Pods(ns.Name).List(ctx, v1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
					ExpectWithOffset(1, err).NotTo(HaveOccurred())
					for _, p := range pods.Items {
						podsmeta = append(podsmeta, p.ObjectMeta)
					}
				}
				//todo parallize so
				for _, meta := range podsmeta {
					err = clientset.PolicyV1().Evictions(meta.Namespace).Evict(ctx, &policy.Eviction{
						ObjectMeta: meta,
					})
					if errors.IsTooManyRequests(err) {
						return fmt.Errorf("failed to evict %s/%s: %v", meta.Namespace, meta.Name, err)
					}
					ExpectWithOffset(1, err).NotTo(HaveOccurred())
					fmt.Printf("evicted %s/%s\n", meta.Namespace, meta.Name)
				}
				return nil
			}
			EventuallyWithOffset(1, drain, time.Minute, time.Second).Should(Succeed())
			//verify there is always one running pod? other might be terminating/creating so need different
			//check that there are two pods temporarily or does that not matter as long as we successfully evicted?
			By("Verifying we scale back down")
			verifyDeploymentReplicas := func() error {
				deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, "controller-manager", v1.GetOptions{})
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				if *deployment.Spec.Replicas != 1 {
					return fmt.Errorf("got %d controller replicas", *deployment.Spec.Replicas)
				}
				return nil
			}
			//have to wait longer than pdbwatchers cooldown
			EventuallyWithOffset(1, verifyDeploymentReplicas, 2*time.Minute, time.Second).Should(Succeed())
			By("Verifying we only have one pod left")
			EventuallyWithOffset(1, verifyOneRunningPod, time.Minute, time.Second).Should(Succeed())

			By("Manually scaling Deployment replicas up")
			scaleNginxReplicas := func() error {
				cmd := exec.Command("kubectl", "scale", "deployment",
					"ingress-nginx-controller", "--replicas", "2", "-n", "ingress-nginx")

				// Run the command and capture the output
				output, err := cmd.CombinedOutput()
				if err != nil {
					log.Printf("Error scaling deployment: %v\n", err)
					return err
				}

				// Log the output from the command (success message)
				log.Printf("Scaled deployment %s to %d replicas: %s\n", "ingress-nginx-controller", 2, output)
				return nil
			}
			EventuallyWithOffset(1, scaleNginxReplicas, time.Minute, time.Second).Should(Succeed())

			By("Verify PDB MinAvailable is 2")
			verifyMinAvailableUpdated := func() error {
				pdbList, err := clientset.PolicyV1().PodDisruptionBudgets("ingress-nginx").List(ctx,
					v1.ListOptions{Limit: 1})
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d pdbs in namespace ingress-nginx\n", len(pdbList.Items))
				for _, pdb := range pdbList.Items {
					fmt.Printf("found pdb name: %s \n", pdb.Name)
					if pdb.Spec.MinAvailable != nil {
						if val := pdb.Spec.MinAvailable.IntValue(); val != 2 {
							return fmt.Errorf("PDB '%s' has MinAvailable set to %d (not 2)\n", pdb.Name, val)
						}
						fmt.Printf("PDB '%s' has MinAvailable set to 2\n", pdb.Name)
					}
				}
				return nil
			}
			EventuallyWithOffset(1, verifyMinAvailableUpdated, time.Minute, time.Second).Should(Succeed())

			By("Delete Deployment and verify PDB gets deleted")
			deleteNginxDeployment := func() error {
				err = clientset.AppsV1().Deployments("ingress-nginx").
					Delete(context.TODO(), "ingress-nginx-controller", v1.DeleteOptions{})
				if err != nil {
					return fmt.Errorf("Error deleting Deployment %s in namespace %s: %v",
						"ingress-nginx-controller", "ingress-nginx", err)
				}

				fmt.Printf("Deployment '%s' deleted successfully in namespace '%s'.\n", "ingress-nginx-controller", "ingress-nginx")
				return nil
			}
			EventuallyWithOffset(1, deleteNginxDeployment, time.Minute, time.Second).Should(Succeed())
			verifyPdbNotExists := func() error {
				pdbList, err := clientset.PolicyV1().PodDisruptionBudgets("ingress-nginx").List(ctx,
					v1.ListOptions{Limit: 1})
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d pdbs in namespace ingress-nginx\n", len(pdbList.Items))
				if len(pdbList.Items) != 0 {
					return fmt.Errorf("nginx pdb is still present on cluster")
				}
				return nil
			}
			verifyPdbWatcherNotExists := func() error {
				gvr := schema.GroupVersionResource{
					Group:    "apps.mydomain.com", // API group for your custom resource
					Version:  "v1",                // API version for the custom resource
					Resource: "pdbwatchers",       // Resource name (plural form of your custom resource)
				}
				pdbWatcherList, err := dynamicClient.Resource(gvr).Namespace("ingress-nginx").List(ctx,
					v1.ListOptions{Limit: 1})
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d pdbwatchers in namespace ingress-nginx \n", len(pdbWatcherList.Items))
				if len(pdbWatcherList.Items) != 0 {
					return fmt.Errorf("nginx pdbwatcher is still present on cluster")
				}
				return nil
			}
			EventuallyWithOffset(1, verifyPdbNotExists, time.Minute, time.Second).Should(Succeed())
			EventuallyWithOffset(1, verifyPdbWatcherNotExists, time.Minute, time.Second).Should(Succeed())
		})
	})
})
