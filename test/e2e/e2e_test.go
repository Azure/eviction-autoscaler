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
	"os/exec"
	"path/filepath"
	"strings"
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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(policy.AddToScheme(scheme))
	utilruntime.Must(types.AddToScheme(scheme))
}

// Test namespace for eviction-autoscaler
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
			// Namespace will be created automatically by Helm with --create-namespace
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

		// Test 1: Core functionality - PDB creation, eviction handling, ownership
		It("should manage PDB lifecycle and handle evictions correctly", func() {
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

			By("creating ingress-nginx namespace with enable annotation")
			cmd = exec.Command("kubectl", "create", "namespace", "ingress-nginx")
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("annotating ingress-nginx namespace to enable eviction autoscaler")
			cmd = exec.Command("kubectl", "annotate", "namespace", "ingress-nginx",
				"eviction-autoscaler.azure.com/enable=true")
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("deploying nginx onto the cluster using template")
			err = createDeployment(deploymentConfig{
				Name:           "ingress-nginx",
				Namespace:      "ingress-nginx",
				Replicas:       1,
				MaxUnavailable: 0,
			})
			ExpectWithOffset(1, err).NotTo(HaveOccurred()) // Deploy the controller using the Helm chart
			By("deploying the controller-manager with Helm")

			// Split the image into repository and tag so we can
			// override the chart values accordingly.
			imgParts := strings.Split(projectimage, ":")
			Expect(imgParts).To(HaveLen(2), "expected image to be of the form <repository>:<tag>")

			repo := imgParts[0]
			tag := imgParts[1]

			// Use `helm upgrade --install` so that the test can be re-run without manual cleanup.
			// Not: we use pullPolicy=IfNotPresent is required for Kind e2e testing because
			// We build and load the image locally into Kind cluster
			// the image tag doesn't exist in any remote registry
			// if pullPolicy=Always would fail trying to pull from remote registry
			helmArgs := []string{
				"upgrade", "--install", "eviction-autoscaler", "helm/eviction-autoscaler",
				"--namespace", namespace, "--create-namespace",
				"--set", fmt.Sprintf("image.repository=%s", repo),
				"--set", fmt.Sprintf("image.tag=%s", tag),
				"--set", "image.pullPolicy=IfNotPresent",
				"--set", "pdb.create=true",
			}

			cmd = exec.Command("helm", helmArgs...)
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("annotating the eviction-autoscaler namespace to enable eviction autoscaler")
			cmd = exec.Command("kubectl", "annotate", "namespace", namespace,
				"eviction-autoscaler.azure.com/enable=true", "--overwrite")
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			cmd = exec.Command("kubectl", "wait", "--for=condition=available",
				"deployment/eviction-autoscaler",
				"--namespace", namespace, "--timeout=300s")
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(homedir.HomeDir(), ".kube", "config"))
			Expect(err).NotTo(HaveOccurred())

			// create the clientset
			clientset, err := client.New(config, client.Options{
				Scheme: scheme,
			})
			Expect(err).NotTo(HaveOccurred())
			// To-Do: try to figure out why usig clientset for evictions results in following err
			// \"no matches for kind "Eviction" in version \"policy/v1\""
			evictionClient, err := kubernetes.NewForConfig(config)
			Expect(err).NotTo(HaveOccurred())
			Expect(clientset).NotTo(BeNil())

			By("validating that the controller-manager pod is running as expected")

			verifyRunningPods := func(namespace string, labels client.MatchingLabels, numberOfPods int) (string, error) {
				var pods = &corev1.PodList{}
				err := clientset.List(ctx, pods, client.InNamespace(namespace),
					labels, client.Limit(1))
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				if len(pods.Items) != numberOfPods {
					return "", fmt.Errorf("got %s %d pods", pods.Items[0].Name, len(pods.Items))
				}
				if pods.Items[0].Status.Phase != "Running" {
					return "", fmt.Errorf("%s pod in %s status", pods.Items[0].Name, pods.Items[0].Status.Phase)
				}
				fmt.Printf("pod %s running on %s\n", pods.Items[0].Name, pods.Items[0].Spec.NodeName)

				return pods.Items[0].Spec.NodeName, nil
			}
			verifyControllerMgrPods := func() error {
				_, e := verifyRunningPods(namespace, client.MatchingLabels{
					"app.kubernetes.io/name": "eviction-autoscaler",
				}, 1)
				return e
			}
			EventuallyWithOffset(1, verifyControllerMgrPods, time.Minute, time.Second).Should(Succeed())

			By("validating that the nginx pod is running as expected")
			var nodeName string
			verifyNginxPods := func() error {
				nodeName, err = verifyRunningPods("ingress-nginx", client.MatchingLabels{}, 1)
				return err
			}

			EventuallyWithOffset(1,
				verifyNginxPods,
				time.Minute, time.Second).Should(Succeed())

			var deployment = &appsv1.Deployment{}

			err = clientset.Get(ctx, client.ObjectKey{Name: "ingress-nginx", Namespace: "ingress-nginx"}, deployment)
			Expect(err).NotTo(HaveOccurred())
			fmt.Printf("Deployment after create '%s' at generation %d\n", deployment.Name, deployment.Generation)

			//this is different than the eviction-autoscaler manager in that the pdb is generated
			By("Verify PDB and PDBWatcher exist")
			verifyPdbExists := func() error {
				var pdbList = &policy.PodDisruptionBudgetList{}
				err := clientset.List(ctx, pdbList, client.InNamespace("ingress-nginx"), client.Limit(1))
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d pdbs in namespace ingress-nginx\n", len(pdbList.Items))
				for _, pdb := range pdbList.Items {
					//fmt.Printf("found pdb name: %s \n", pdb.Name)
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
				err = clientset.List(ctx, evictionAutoScalerList,
					client.InNamespace("ingress-nginx"), &client.ListOptions{Limit: 1})
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
				err = clientset.Get(ctx, client.ObjectKey{Name: "ingress-nginx", Namespace: "ingress-nginx"}, deployment)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				if val, ok := deployment.Annotations["evictionSurgeReplicas"]; !ok {
					return fmt.Errorf("Annotation: \"evictionSurgeReplicas\" is not  added")
				} else {
					fmt.Printf("Annotation evictionSurgeReplicas has value %s set\n", val)
				}
				fmt.Printf("Deployment after cordon '%s' at generation %d\n", deployment.Name, deployment.Generation)
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
				fmt.Printf("Deployment after eviction '%s' at generation %d\n", deployment.Name, deployment.Generation)
				return nil
			}
			//have to wait longer than pdbwatchers cooldown
			EventuallyWithOffset(1, verifyDeploymentReplicas, 2*time.Minute, time.Second).Should(Succeed())
			By("Verifying we only have one pod left")
			EventuallyWithOffset(1,
				verifyNginxPods,
				time.Minute, time.Second).Should(Succeed())

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
				deployment.Spec.Replicas = ptr.To(int32(2))
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
				//Eviction Autoscaler minavailable only updates lazily on an eviction or resync so ignore
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
				//fmt.Printf("found %d pdbs in namespace ingress-nginx\n", len(pdbList.Items))
				if len(pdbList.Items) != 0 {
					return fmt.Errorf("nginx pdb is still present on cluster")
				}
				return nil
			}
			verifyEvictionAutoScalerNotExists := func() error {
				var evictionAutoScalerList = &types.EvictionAutoScalerList{}
				err = clientset.List(ctx, evictionAutoScalerList, client.InNamespace("ingress-nginx"),
					&client.ListOptions{Limit: 1})
				Expect(err).NotTo(HaveOccurred())
				fmt.Printf("found %d evictionautoscalers in namespace ingress-nginx \n", len(evictionAutoScalerList.Items))
				if len(evictionAutoScalerList.Items) != 0 {
					return fmt.Errorf("nginx evictionautoscaler is still present on cluster")
				}
				return nil
			}
			EventuallyWithOffset(1, verifyPdbNotExists, time.Minute, time.Second).Should(Succeed())
			EventuallyWithOffset(1, verifyEvictionAutoScalerNotExists, time.Minute, time.Second).Should(Succeed())

			By("creating a test namespace for annotation-based PDB control")
			testNs := "eviction-autoscaler-test"
			cmd = exec.Command("kubectl", "create", "namespace", testNs)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("annotating the test namespace to enable eviction autoscaler")
			cmd = exec.Command("kubectl", "annotate", "namespace", testNs,
				"eviction-autoscaler.azure.com/enable=true")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a test deployment in the test namespace with annotation")
			err = createDeployment(deploymentConfig{
				Name:           "nginx-test",
				Namespace:      testNs,
				Replicas:       1,
				MaxUnavailable: 0,
				Annotations: map[string]string{
					"eviction-autoscaler.azure.com/pdb-create": "false",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("removing pdb-create annotation from the deployment and verifying PDB is created")
			cmd = exec.Command("kubectl", "annotate", "deployment/nginx-test", "--namespace", testNs,
				"eviction-autoscaler.azure.com/pdb-create-", "--overwrite")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, testNs, "nginx-test")
			}, time.Minute, time.Second).Should(Succeed())

			By("deleting the test deployment and verifying PDB is deleted")
			cmd = exec.Command("kubectl", "delete", "deployment/nginx-test", "--namespace", testNs)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			EventuallyWithOffset(1, func() error {
				return verifyNoPdb(ctx, clientset, testNs, "nginx-test")
			}, time.Minute, time.Second).Should(Succeed())

			By("creating a new deployment with PDB to test annotation removal behavior")
			err = createDeployment(deploymentConfig{
				Name:           "nginx-annotation-test",
				Namespace:      testNs,
				Replicas:       3,
				MaxUnavailable: 0,
			})
			Expect(err).NotTo(HaveOccurred())

			// Wait for PDB to be created
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, testNs, "nginx-annotation-test")
			}, time.Minute, time.Second).Should(Succeed())

			By("removing ownedBy annotation from PDB")
			cmd = exec.Command("kubectl", "annotate", "pdb/nginx-annotation-test", "--namespace", testNs,
				"ownedBy-", "--overwrite")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("scaling deployment to 5 replicas and verifying PDB minAvailable is NOT updated")
			cmd = exec.Command("kubectl", "scale", "deployment/nginx-annotation-test", "--namespace", testNs, "--replicas=5")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Wait a bit for controller to potentially react
			time.Sleep(10 * time.Second)

			// Verify PDB minAvailable is still 3 (not updated to 5)
			EventuallyWithOffset(1, func() error {
				return verifyPdbMinAvailable(ctx, clientset, testNs, "nginx-annotation-test", 3)
			}, time.Minute, time.Second).Should(Succeed())

			By("verifying owner reference was removed from PDB after annotation removal")
			EventuallyWithOffset(1, func() error {
				return verifyNoOwnerReference(ctx, clientset, testNs, "nginx-annotation-test")
			}, time.Minute, time.Second).Should(Succeed())

			By("deleting deployment and verifying PDB is NOT deleted (user has taken ownership)")
			cmd = exec.Command("kubectl", "delete", "deployment/nginx-annotation-test", "--namespace", testNs)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Wait a bit to ensure controller doesn't delete PDB
			time.Sleep(10 * time.Second)

			// PDB should still exist since owner reference was removed
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, testNs, "nginx-annotation-test")
			}, time.Minute, time.Second).Should(Succeed())

			By("cleaning up the orphaned PDB")
			cmd = exec.Command("kubectl", "delete", "pdb/nginx-annotation-test", "--namespace", testNs)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("testing bidirectional ownership transfer")
			By("creating a new deployment with PDB")
			err = createDeployment(deploymentConfig{
				Name:           "nginx-ownership-test",
				Namespace:      testNs,
				Replicas:       3,
				MaxUnavailable: 0,
			})
			Expect(err).NotTo(HaveOccurred())

			// Wait for PDB to be created
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, testNs, "nginx-ownership-test")
			}, time.Minute, time.Second).Should(Succeed())

			By("verifying PDB has owner reference initially")
			EventuallyWithOffset(1, func() error {
				return verifyHasOwnerReference(ctx, clientset, testNs, "nginx-ownership-test")
			}, time.Minute, time.Second).Should(Succeed())

			By("removing ownedBy annotation from PDB (user takes ownership)")
			cmd = exec.Command("kubectl", "annotate", "pdb/nginx-ownership-test", "--namespace", testNs,
				"ownedBy-", "--overwrite")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying owner reference was removed after annotation removal")
			EventuallyWithOffset(1, func() error {
				return verifyNoOwnerReference(ctx, clientset, testNs, "nginx-ownership-test")
			}, time.Minute, time.Second).Should(Succeed())

			By("adding ownedBy annotation back to PDB (user returns control)")
			cmd = exec.Command("kubectl", "annotate", "pdb/nginx-ownership-test", "--namespace", testNs,
				"ownedBy=EvictionAutoScaler", "--overwrite")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying owner reference was added back after annotation re-added")
			EventuallyWithOffset(1, func() error {
				return verifyHasOwnerReference(ctx, clientset, testNs, "nginx-ownership-test")
			}, time.Minute, time.Second).Should(Succeed())

			By("deleting deployment and verifying PDB is now deleted (controller has control again)")
			cmd = exec.Command("kubectl", "delete", "deployment/nginx-ownership-test", "--namespace", testNs)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// PDB should be deleted since owner reference is back
			EventuallyWithOffset(1, func() error {
				return verifyNoPdb(ctx, clientset, testNs, "nginx-ownership-test")
			}, time.Minute, time.Second).Should(Succeed())

			By("testing maxUnavailable behavior - deployments with maxUnavailable != 0 should not get PDBs")
			By("creating a deployment with maxUnavailable=1")
			err = createDeployment(deploymentConfig{
				Name:           "nginx-maxunavailable",
				Namespace:      testNs,
				Replicas:       3,
				MaxUnavailable: 1,
			})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment with maxUnavailable to be ready")
			err = waitForDeployment("nginx-maxunavailable", testNs)
			Expect(err).NotTo(HaveOccurred())

			By("verifying NO PDB is created for deployment with maxUnavailable != 0")
			time.Sleep(10 * time.Second) // Give controller time to potentially create PDB
			EventuallyWithOffset(1, func() error {
				return verifyNoPdb(ctx, clientset, testNs, "nginx-maxunavailable")
			}, 30*time.Second, time.Second).Should(Succeed())

			By("cleaning up maxUnavailable test deployment")
			deleteDeployment("nginx-maxunavailable", testNs)

			By("testing existing PDB detection - should not create duplicate PDBs")
			By("manually creating a PDB for a deployment")
			err = createPDB("nginx-existing-pdb", testNs, 2, map[string]string{"app": "nginx-existing-pdb"})
			Expect(err).NotTo(HaveOccurred())

			By("creating a deployment that matches the existing PDB")
			err = createDeployment(deploymentConfig{
				Name:           "nginx-existing-pdb",
				Namespace:      testNs,
				Replicas:       3,
				MaxUnavailable: 0,
			})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			err = waitForDeployment("nginx-existing-pdb", testNs)
			Expect(err).NotTo(HaveOccurred())

			By("verifying only ONE PDB exists (the manually created one)")
			time.Sleep(10 * time.Second) // Give controller time to potentially create duplicate
			var pdbList = &policy.PodDisruptionBudgetList{}
			err = clientset.List(ctx, pdbList, client.InNamespace(testNs))
			Expect(err).NotTo(HaveOccurred())

			matchingPdbs := 0
			for _, pdb := range pdbList.Items {
				if pdb.Name == "nginx-existing-pdb" {
					matchingPdbs++
				}
			}
			Expect(matchingPdbs).To(Equal(1), "Expected exactly one PDB, found %d", matchingPdbs)

			By("verifying the PDB was not modified by eviction-autoscaler (no ownedBy annotation)")
			var existingPdb policy.PodDisruptionBudget
			err = clientset.Get(ctx, client.ObjectKey{Namespace: testNs, Name: "nginx-existing-pdb"}, &existingPdb)
			Expect(err).NotTo(HaveOccurred())
			_, hasOwnedBy := existingPdb.Annotations["ownedBy"]
			Expect(hasOwnedBy).To(BeFalse(), "Existing PDB should not have ownedBy annotation")

			By("cleaning up existing PDB test resources")
			deleteDeployment("nginx-existing-pdb", testNs)
			deletePDB("nginx-existing-pdb", testNs)

			// Cleanup test resources from first test suite
			By("cleaning up eviction-autoscaler-test namespace")
			cmd = exec.Command("kubectl", "delete", "namespace", testNs)
			_, _ = utils.Run(cmd)
		})

		// Test 2: Namespace filtering modes - opt-in vs opt-out
		It("should respect enabledByDefault and actionedNamespaces configuration", func() {
			ctx := context.Background()

			By("uninstalling the existing eviction-autoscaler to reconfigure")
			cmd := exec.Command("helm", "uninstall", "eviction-autoscaler", "--namespace", namespace)
			_, _ = utils.Run(cmd)

			// Wait for resources to be cleaned up
			time.Sleep(10 * time.Second)

			By("reinstalling eviction-autoscaler with opt-out mode (enabledByDefault=true)")
			// Use the same image that was built in the first test
			projectimage := "evictionautoscaler:e2etest"
			imgParts := strings.Split(projectimage, ":")
			Expect(imgParts).To(HaveLen(2), "expected image to be of the form <repository>:<tag>")
			repo := imgParts[0]
			tag := imgParts[1]

			helmArgs := []string{
				"upgrade", "--install", "eviction-autoscaler", "helm/eviction-autoscaler",
				"--namespace", namespace, "--create-namespace",
				"--set", fmt.Sprintf("image.repository=%s", repo),
				"--set", fmt.Sprintf("image.tag=%s", tag),
				"--set", "image.pullPolicy=IfNotPresent",
				"--set", "controllerConfig.pdb.create=true",
				"--set", "controllerConfig.namespaces.enabledByDefault=true",
			}
			cmd = exec.Command("helm", helmArgs...)
			_, err := utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			cmd = exec.Command("kubectl", "wait", "--for=condition=available",
				"deployment/eviction-autoscaler",
				"--namespace", namespace, "--timeout=300s")
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(homedir.HomeDir(), ".kube", "config"))
			Expect(err).NotTo(HaveOccurred())

			clientset, err := client.New(config, client.Options{
				Scheme: scheme,
			})
			Expect(err).NotTo(HaveOccurred())

			// Test 1: Opt-out mode - namespace without annotation should be enabled
			testNsOptOut := "test-opt-out-mode"
			By("creating a namespace without annotation (should be enabled in opt-out mode)")
			cmd = exec.Command("kubectl", "create", "namespace", testNsOptOut)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a deployment in opt-out namespace")
			err = createDeployment(deploymentConfig{
				Name:           "nginx-opt-out",
				Namespace:      testNsOptOut,
				Replicas:       2,
				MaxUnavailable: 0,
			})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			err = waitForDeployment("nginx-opt-out", testNsOptOut)
			Expect(err).NotTo(HaveOccurred())

			By("verifying PDB IS created in opt-out mode without annotation")
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, testNsOptOut, "nginx-opt-out")
			}, time.Minute, time.Second).Should(Succeed())

			// Test 2: Opt-out mode - namespace with enable=false should be disabled
			By("disabling the namespace with annotation enable=false")
			cmd = exec.Command("kubectl", "annotate", "namespace", testNsOptOut,
				"eviction-autoscaler.azure.com/enable=false")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying PDB is deleted after setting enable=false")
			EventuallyWithOffset(1, func() error {
				return verifyNoPdb(ctx, clientset, testNsOptOut, "nginx-opt-out")
			}, time.Minute, time.Second).Should(Succeed())

			// Test 3: Actioned namespace in opt-in mode - should be enabled regardless of annotation
			testNsActioned := "actioned-test"
			By("creating the actioned-test namespace")
			cmd = exec.Command("kubectl", "create", "namespace", testNsActioned)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a deployment in actioned namespace")
			err = createDeployment(deploymentConfig{
				Name:           "nginx-actioned",
				Namespace:      testNsActioned,
				Replicas:       2,
				MaxUnavailable: 0,
			})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			err = waitForDeployment("nginx-actioned", testNsActioned)
			Expect(err).NotTo(HaveOccurred())

			By("verifying PDB IS created in actioned namespace (opt-out mode, all enabled by default)")
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, testNsActioned, "nginx-actioned")
			}, time.Minute, time.Second).Should(Succeed())

			// Test 4: Switch back to opt-in mode and verify behavior
			By("reinstalling eviction-autoscaler with opt-in mode (enabledByDefault=false)")
			cmd = exec.Command("helm", "uninstall", "eviction-autoscaler", "--namespace", namespace)
			_, _ = utils.Run(cmd)
			time.Sleep(10 * time.Second)

			helmArgsOptIn := []string{
				"upgrade", "--install", "eviction-autoscaler", "helm/eviction-autoscaler",
				"--namespace", namespace, "--create-namespace",
				"--set", fmt.Sprintf("image.repository=%s", repo),
				"--set", fmt.Sprintf("image.tag=%s", tag),
				"--set", "image.pullPolicy=IfNotPresent",
				"--set", "controllerConfig.pdb.create=true",
				"--set", "controllerConfig.namespaces.enabledByDefault=false",
				"--set", "controllerConfig.namespaces.actionedNamespaces[0]=kube-system",
				"--set", "controllerConfig.namespaces.actionedNamespaces[1]=actioned-test",
			}
			cmd = exec.Command("helm", helmArgsOptIn...)
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			cmd = exec.Command("kubectl", "wait", "--for=condition=available",
				"deployment/eviction-autoscaler",
				"--namespace", namespace, "--timeout=300s")
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())

			// Test 5: Opt-in mode - namespace without annotation should be disabled
			testNsOptIn := "test-opt-in-mode"
			By("creating a namespace without annotation (should be disabled in opt-in mode)")
			cmd = exec.Command("kubectl", "create", "namespace", testNsOptIn)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a deployment in opt-in namespace")
			err = createDeployment(deploymentConfig{
				Name:           "nginx-opt-in",
				Namespace:      testNsOptIn,
				Replicas:       2,
				MaxUnavailable: 0,
			})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			err = waitForDeployment("nginx-opt-in", testNsOptIn)
			Expect(err).NotTo(HaveOccurred())

			By("verifying NO PDB is created in opt-in mode without annotation")
			time.Sleep(10 * time.Second)
			EventuallyWithOffset(1, func() error {
				return verifyNoPdb(ctx, clientset, testNsOptIn, "nginx-opt-in")
			}, 30*time.Second, time.Second).Should(Succeed())

			// Test 6: Actioned namespace in opt-in mode - should be enabled
			By("verifying actioned-test namespace has PDB in opt-in mode")
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, testNsActioned, "nginx-actioned")
			}, time.Minute, time.Second).Should(Succeed())

			// Test 7: kube-system should always be enabled
			By("verifying kube-system deployment has PDB (always enabled)")
			err = createDeployment(deploymentConfig{
				Name:           "test-kube-opt",
				Namespace:      "kube-system",
				Replicas:       1,
				MaxUnavailable: 0,
			})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready in kube-system")
			err = waitForDeployment("test-kube-opt", "kube-system")
			Expect(err).NotTo(HaveOccurred())

			By("verifying PDB IS created in kube-system (always enabled)")
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, "kube-system", "test-kube-opt")
			}, time.Minute, time.Second).Should(Succeed())

			// Test 8: Create new deployment with annotation in opt-in mode namespace
			testNsOptInWithAnnotation := "test-opt-in-with-anno"
			By("creating a namespace with enable=true annotation in opt-in mode")
			cmd = exec.Command("kubectl", "create", "namespace", testNsOptInWithAnnotation)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("annotating the namespace to enable eviction autoscaler")
			cmd = exec.Command("kubectl", "annotate", "namespace", testNsOptInWithAnnotation,
				"eviction-autoscaler.azure.com/enable=true")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a deployment in opt-in namespace with annotation")
			err = createDeployment(deploymentConfig{
				Name:           "nginx-opt-in-anno",
				Namespace:      testNsOptInWithAnnotation,
				Replicas:       2,
				MaxUnavailable: 0,
			})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for deployment to be ready")
			err = waitForDeployment("nginx-opt-in-anno", testNsOptInWithAnnotation)
			Expect(err).NotTo(HaveOccurred())

			By("verifying PDB IS created in opt-in mode with annotation")
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, testNsOptInWithAnnotation, "nginx-opt-in-anno")
			}, time.Minute, time.Second).Should(Succeed())

			// Test 9: Dynamic annotation changes in opt-in mode
			By("testing dynamic annotation addition to existing namespace")
			By("annotating the opt-in namespace (that had no annotation) to enable")
			cmd = exec.Command("kubectl", "annotate", "namespace", testNsOptIn,
				"eviction-autoscaler.azure.com/enable=true")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("scaling deployment to trigger reconciliation")
			cmd = exec.Command("kubectl", "scale", "deployment/nginx-opt-in", "--replicas=3",
				"--namespace", testNsOptIn)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying PDB IS now created after annotation is added")
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, testNsOptIn, "nginx-opt-in")
			}, time.Minute, time.Second).Should(Succeed())

			By("verifying EvictionAutoScaler IS now created after annotation is added")
			EventuallyWithOffset(1, func() error {
				return verifyEvictionAutoScalerCreated(ctx, clientset, testNsOptIn, "nginx-opt-in")
			}, time.Minute, time.Second).Should(Succeed())

			// Test 10: Disabling and re-enabling via annotation
			By("testing disable/enable annotation lifecycle")
			By("disabling eviction autoscaler by setting annotation to false")
			cmd = exec.Command("kubectl", "annotate", "namespace", testNsOptInWithAnnotation,
				"eviction-autoscaler.azure.com/enable=false", "--overwrite")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying PDB is deleted after setting enable=false")
			EventuallyWithOffset(1, func() error {
				return verifyNoPdb(ctx, clientset, testNsOptInWithAnnotation, "nginx-opt-in-anno")
			}, time.Minute, time.Second).Should(Succeed())

			By("verifying EvictionAutoScaler is deleted after setting enable=false")
			EventuallyWithOffset(1, func() error {
				return verifyNoEvictionAutoScaler(ctx, clientset, testNsOptInWithAnnotation, "nginx-opt-in-anno")
			}, time.Minute, time.Second).Should(Succeed())

			By("re-enabling eviction autoscaler by setting annotation back to true")
			cmd = exec.Command("kubectl", "annotate", "namespace", testNsOptInWithAnnotation,
				"eviction-autoscaler.azure.com/enable=true", "--overwrite")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("scaling deployment to trigger reconciliation")
			cmd = exec.Command("kubectl", "scale", "deployment/nginx-opt-in-anno", "--replicas=3",
				"--namespace", testNsOptInWithAnnotation)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying PDB is recreated after annotation is enabled again")
			EventuallyWithOffset(1, func() error {
				return verifyPdbCreated(ctx, clientset, testNsOptInWithAnnotation, "nginx-opt-in-anno")
			}, time.Minute, time.Second).Should(Succeed())

			By("verifying EvictionAutoScaler is recreated after annotation is enabled again")
			EventuallyWithOffset(1, func() error {
				return verifyEvictionAutoScalerCreated(ctx, clientset, testNsOptInWithAnnotation, "nginx-opt-in-anno")
			}, time.Minute, time.Second).Should(Succeed())

			By("removing the annotation entirely")
			cmd = exec.Command("kubectl", "annotate", "namespace", testNsOptInWithAnnotation,
				"eviction-autoscaler.azure.com/enable-")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying PDB is deleted after annotation is removed")
			EventuallyWithOffset(1, func() error {
				return verifyNoPdb(ctx, clientset, testNsOptInWithAnnotation, "nginx-opt-in-anno")
			}, time.Minute, time.Second).Should(Succeed())

			By("verifying EvictionAutoScaler is deleted after annotation is removed")
			EventuallyWithOffset(1, func() error {
				return verifyNoEvictionAutoScaler(ctx, clientset, testNsOptInWithAnnotation, "nginx-opt-in-anno")
			}, time.Minute, time.Second).Should(Succeed())

			// Cleanup
			By("cleaning up test namespaces")
			cmd = exec.Command("kubectl", "delete", "namespace", testNsOptOut)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "namespace", testNsOptIn)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "namespace", testNsActioned)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "namespace", testNsOptInWithAnnotation)
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "deployment", "test-kube-opt", "--namespace", "kube-system")
			_, _ = utils.Run(cmd)

			By("Scraping controller metrics at the end of the e2e test")

			scrapeMetrics := func() error {
				// Fetch controller-manager pod using clientset and label selector
				var pods = &corev1.PodList{}
				err := clientset.List(ctx, pods, client.InNamespace(namespace),
					client.MatchingLabels{"app.kubernetes.io/name": "eviction-autoscaler"},
					client.Limit(1))
				if err != nil {
					return err
				}
				if len(pods.Items) == 0 {
					return fmt.Errorf("unable to locate controller-manager pod")
				}
				podName := pods.Items[0].Name

				// TODO Use clientset with proxy and HTTP GET instead of kubectl and use a Prometheus client
				// to get structured data for assertions. Try to confirm that we get a
				// MinAvailableEqualsDesiredSignal from nginx ingress pod

				// Scrape metrics directly using the Kubernetes API server proxy
				metricsPath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:8080/proxy/metrics", namespace, podName)
				cmd = exec.Command("kubectl", "-n", namespace, "get", "--raw", metricsPath)
				metricsOutput, err := utils.Run(cmd)
				if err != nil {
					return err
				}

				// Print a subset of interesting metrics for visibility
				fmt.Println("===== Eviction Autoscaler Metrics =====")
				metricsLines := strings.Split(string(metricsOutput), "\n")
				for _, line := range metricsLines {
					// Only show our eviction autoscaler and controller runtime metrics, skip comments and empty lines
					if strings.HasPrefix(line, "eviction_autoscaler_") || strings.HasPrefix(line, "controller_runtime_") {
						fmt.Println(line)
					}
				}
				fmt.Println("======================================")
				return nil
			}

			Expect(scrapeMetrics()).To(Succeed())
		})
	})
})
