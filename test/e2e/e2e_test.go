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
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policy "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	"github.com/azure/eviction-autoscaler/test/utils"
)

// SYNC with kustomize file
const namespace = "k8s-pdb-autoscaler"
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
		})
	})
})
