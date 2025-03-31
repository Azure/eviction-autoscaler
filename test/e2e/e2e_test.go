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
	"flag"
	"fmt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"
	"os/exec"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policy "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	types "github.com/azure/eviction-autoscaler/api/v1"
	"github.com/azure/eviction-autoscaler/test/utils"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(policy.AddToScheme(scheme))
	utilruntime.Must(types.AddToScheme(scheme))
}

// SYNC with kustomize file
const namespace = "eviction-autoscaler"
const kindClusterName = "e2e"

var cleanEnv = true

var _ = Describe("controller", Ordered, func() {
	BeforeAll(func() {
		opts := zap.Options{
			Development: true,
		}
		opts.BindFlags(flag.CommandLine)
		flag.Parse()

		ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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

			By("deploy nginx onto the cluster")
			cmd = exec.Command("kubectl", "apply", "-f",
				"test/e2e/deploy-ingress-nginx.yaml")
			_, err = utils.Run(cmd)
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
			clientset, err := client.New(config, client.Options{
				Scheme: scheme,
			})
			// To-Do: try to figure out why usig clientset for evictions results in following err
			// \"no matches for kind "Eviction" in version \"policy/v1\""
			evictionClient, err := kubernetes.NewForConfig(config)
			Expect(err).NotTo(HaveOccurred())
			Expect(clientset).NotTo(Equal(nil))

			By("validating that the controller-manager pod is running as expected")
			var nodeName string
			verifyOneRunningPod := func() error {
				var pods = &corev1.PodList{}
				err := clientset.List(ctx, pods, client.InNamespace(namespace),
					client.MatchingLabels{"control-plane": "controller-manager"}, client.Limit(1))
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				if len(pods.Items) != 1 {
					return fmt.Errorf("got %d controller pods", len(pods.Items))
				}
				if pods.Items[0].Status.Phase != "Running" {
					return fmt.Errorf("controller pod in %s status", pods.Items[0].Status.Phase)
				}
				fmt.Printf("controller pod %s running on %s\n", pods.Items[0].Name, pods.Items[0].Spec.NodeName)

				return nil
			}
			EventuallyWithOffset(1, verifyOneRunningPod, time.Minute, time.Second).Should(Succeed())

			By("validating that the nginx pod is running as expected")
			//var nodeName string
			verifyOneRunningNginxPod := func() error {
				var pods = &corev1.PodList{}
				err := clientset.List(ctx, pods, client.InNamespace("ingress-nginx"), client.Limit(1))
				ExpectWithOffset(1, err).NotTo(HaveOccurred())

				if len(pods.Items) != 1 {
					return fmt.Errorf("got %d nginx pods", len(pods.Items))
				}
				if pods.Items[0].Status.Phase != "Running" {
					return fmt.Errorf("nginx pod in %s status", pods.Items[0].Status.Phase)
				}
				fmt.Printf("nginx pod %s running on %s\n", pods.Items[0].Name, pods.Items[0].Spec.NodeName)
				nodeName = pods.Items[0].Spec.NodeName
				return nil
			}
			EventuallyWithOffset(1, verifyOneRunningNginxPod, time.Minute, time.Second).Should(Succeed())
			By("Verify PDB and PDBWatcher exist")
			verifyPdbExists := func() error {
				var pdbList = &policy.PodDisruptionBudgetList{}
				err := clientset.List(ctx, pdbList, client.InNamespace("ingress-nginx"), client.Limit(1))
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d pdbs in namespace ingress-nginx\n", len(pdbList.Items))
				for _, pdb := range pdbList.Items {
					fmt.Printf("found pdb name: %s \n", pdb.Name)
					if pdb.Name != "ingress-nginx" {
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
			verifyEvictionAutoScalerExists := func() error {
				var evictionAutoScalerList = &types.EvictionAutoScalerList{}
				err = clientset.List(ctx, evictionAutoScalerList, client.InNamespace("ingress-nginx"), &client.ListOptions{Limit: 1})
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d evictionautoscalers in namespace ingress-nginx \n", len(evictionAutoScalerList.Items))
				for _, resource := range evictionAutoScalerList.Items {
					fmt.Printf("found evictionautoscaler name: %s \n", resource.GetName())
					if resource.GetName() != "ingress-nginx" {
						return fmt.Errorf("nginx evictionautoscalers is not present on cluster")
					}
					fmt.Printf("custom resource found: %s\n", resource.GetName())
				}
				return nil
			}
			EventuallyWithOffset(1, verifyPdbExists, time.Minute, time.Second).Should(Succeed())
			EventuallyWithOffset(1, verifyEvictionAutoScalerExists, time.Minute, time.Second).Should(Succeed())

			By("By Cordoning " + nodeName)
			// Cordon and drain the node that the controller-manager pod is running on
			var node = &corev1.Node{}
			err = clientset.Get(ctx, client.ObjectKey{Name: nodeName}, node, &client.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			node.Spec.Unschedulable = true
			err = clientset.Update(ctx, node, &client.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying annotations are added")
			verifyAnnotationExists := func() error {
				var deployment = &appsv1.Deployment{}
				err = clientset.Get(ctx, client.ObjectKey{Name: "ingress-nginx", Namespace: "ingress-nginx"}, deployment)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				if val, ok := deployment.Annotations["evictionSurgeReplicas"]; !ok {
					return fmt.Errorf("Annotation: \"evictionSurgeReplicas\" is not  added")
				} else {
					fmt.Printf("Annotation evictionSurgeReplicas has value %s set\n", val)
				}
				return nil
			}
			EventuallyWithOffset(1, verifyAnnotationExists, time.Minute, time.Second).Should(Succeed())

			By("By Draining " + nodeName)
			drain := func() error {
				var podsmeta = []v1.ObjectMeta{}
				var namespaces = &corev1.NamespaceList{}
				err := clientset.List(ctx, namespaces)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				for _, ns := range namespaces.Items {
					fmt.Printf("List pods in namespace: %s\n", ns.Name)
					var pods = &corev1.PodList{}
					err := clientset.List(ctx, pods, client.InNamespace(ns.Name), client.MatchingFields{"spec.nodeName": nodeName})
					ExpectWithOffset(1, err).NotTo(HaveOccurred())
					for _, p := range pods.Items {
						podsmeta = append(podsmeta, p.ObjectMeta)
					}
				}
				//todo parallize so
				for _, meta := range podsmeta {
					err = evictionClient.PolicyV1().Evictions(meta.Namespace).Evict(ctx, &policy.Eviction{
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
			var deployment = &appsv1.Deployment{}
			By("Verifying we scale back down")
			verifyDeploymentReplicas := func() error {
				err = clientset.Get(ctx, client.ObjectKey{Name: "ingress-nginx", Namespace: "ingress-nginx"}, deployment)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				if *deployment.Spec.Replicas != 1 {
					return fmt.Errorf("got %d controller replicas\n", *deployment.Spec.Replicas)
				}

				if _, ok := deployment.Annotations["evictionSurgeReplicas"]; ok {
					return fmt.Errorf("Annotation \"evictionSurgeReplicas\" is not removed\n")
				}
				return nil
			}
			//have to wait longer than pdbwatchers cooldown
			EventuallyWithOffset(1, verifyDeploymentReplicas, 2*time.Minute, time.Second).Should(Succeed())
			By("Verifying we only have one pod left")
			EventuallyWithOffset(1, verifyOneRunningNginxPod, time.Minute, time.Second).Should(Succeed())

			By("Verify PDB MinAvailable is Unchanged")
			verifyMinAvailableUnchanged := func() error {
				var pdbList = &policy.PodDisruptionBudgetList{}
				err := clientset.List(ctx, pdbList, client.InNamespace("ingress-nginx"), client.Limit(1))
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d pdbs in namespace ingress-nginx\n", len(pdbList.Items))
				for _, pdb := range pdbList.Items {
					fmt.Printf("found pdb name: %s \n", pdb.Name)
					if pdb.Spec.MinAvailable != nil {
						if val := pdb.Spec.MinAvailable.IntValue(); val != 1 {
							return fmt.Errorf("PDB '%s' has MinAvailable set to %d (not 1)\n", pdb.Name, val)
						}
						fmt.Printf("PDB '%s' has MinAvailable set to 1\n", pdb.Name)
					}
				}
				return nil
			}
			EventuallyWithOffset(1, verifyMinAvailableUnchanged, time.Minute, time.Second).Should(Succeed())

			By("Manually scaling Deployment replicas up")
			scaleNginxReplicas := func() error {
				err = clientset.Get(ctx, client.ObjectKey{Name: "ingress-nginx", Namespace: "ingress-nginx"}, deployment)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				deployment.Spec.Replicas = pointer.Int32(2)
				err = clientset.Update(ctx, deployment, &client.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			EventuallyWithOffset(1, scaleNginxReplicas, time.Minute, time.Second).Should(Succeed())

			By("Verify PDB MinAvailable is 2")
			verifyMinAvailableUpdated := func() error {
				var pdbList = &policy.PodDisruptionBudgetList{}
				err := clientset.List(ctx, pdbList, client.InNamespace("ingress-nginx"), client.Limit(1))
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
				err = clientset.Delete(ctx, deployment)
				if err != nil {
					return fmt.Errorf("Error deleting Deployment %s in namespace %s: %v",
						"ingress-nginx", "ingress-nginx", err)
				}

				fmt.Printf("Deployment '%s' deleted successfully in namespace '%s'.\n", "ingress-nginx", "ingress-nginx")
				return nil
			}
			EventuallyWithOffset(1, deleteNginxDeployment, time.Minute, time.Second).Should(Succeed())
			verifyPdbNotExists := func() error {
				var pdbList = &policy.PodDisruptionBudgetList{}
				err := clientset.List(ctx, pdbList, client.InNamespace("ingress-nginx"), client.Limit(1))
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d pdbs in namespace ingress-nginx\n", len(pdbList.Items))
				if len(pdbList.Items) != 0 {
					return fmt.Errorf("nginx pdb is still present on cluster")
				}
				return nil
			}
			verifyEvictionAutoScalerNotExists := func() error {
				var evictionAutoScalerList = &types.EvictionAutoScalerList{}
				err = clientset.List(ctx, evictionAutoScalerList, client.InNamespace("ingress-nginx"), &client.ListOptions{Limit: 1})
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d evictionautoscalers in namespace ingress-nginx \n", len(evictionAutoScalerList.Items))
				if len(evictionAutoScalerList.Items) != 0 {
					return fmt.Errorf("nginx evictionautoscaler is still present on cluster")
				}
				return nil
			}
			EventuallyWithOffset(1, verifyPdbNotExists, time.Minute, time.Second).Should(Succeed())
			EventuallyWithOffset(1, verifyEvictionAutoScalerNotExists, time.Minute, time.Second).Should(Succeed())
		})
	})
})
