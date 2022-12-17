/*
Copyright 2018 The Kubernetes Authors.

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
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-cmp/cmp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	taintutils "k8s.io/kubernetes/pkg/util/taints"
	"k8s.io/kubernetes/test/e2e/framework"
	e2elog "k8s.io/kubernetes/test/e2e/framework"
	e2enetwork "k8s.io/kubernetes/test/e2e/framework/network"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	admissionapi "k8s.io/pod-security-admission/api"

	nfdv1alpha1 "sigs.k8s.io/node-feature-discovery/pkg/apis/nfd/v1alpha1"
	nfdclient "sigs.k8s.io/node-feature-discovery/pkg/generated/clientset/versioned"
	"sigs.k8s.io/node-feature-discovery/source/custom"
	testutils "sigs.k8s.io/node-feature-discovery/test/e2e/utils"
	testds "sigs.k8s.io/node-feature-discovery/test/e2e/utils/daemonset"
	testpod "sigs.k8s.io/node-feature-discovery/test/e2e/utils/pod"
)

var (
	dockerRepo  = flag.String("nfd.repo", "gcr.io/k8s-staging-nfd/node-feature-discovery", "Docker repository to fetch image from")
	dockerTag   = flag.String("nfd.tag", "master", "Docker tag to use")
	dockerImage = fmt.Sprintf("%s:%s", *dockerRepo, *dockerTag)
)

// cleanupNode deletes all NFD-related metadata from the Node object, i.e.
// labels and annotations
func cleanupNode(cs clientset.Interface) {
	nodeList, err := cs.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	for _, n := range nodeList.Items {
		var err error
		var node *corev1.Node
		for retry := 0; retry < 5; retry++ {
			node, err = cs.CoreV1().Nodes().Get(context.TODO(), n.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			update := false
			// Remove labels
			for key := range node.Labels {
				if strings.HasPrefix(key, nfdv1alpha1.FeatureLabelNs) {
					delete(node.Labels, key)
					update = true
				}
			}

			// Remove annotations
			for key := range node.Annotations {
				if strings.HasPrefix(key, nfdv1alpha1.AnnotationNs) {
					delete(node.Annotations, key)
					update = true
				}
			}

			if !update {
				break
			}

			By("Deleting NFD labels and annotations from node " + node.Name)
			_, err = cs.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
			if err != nil {
				time.Sleep(100 * time.Millisecond)
			} else {
				break
			}

		}
		Expect(err).NotTo(HaveOccurred())
	}

}

func cleanupCRDs(cli *nfdclient.Clientset) {
	// Drop NodeFeatureRule objects
	nfrs, err := cli.NfdV1alpha1().NodeFeatureRules().List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	By("Deleting NodeFeatureRule objects from the cluster")
	for _, nfr := range nfrs.Items {
		err = cli.NfdV1alpha1().NodeFeatureRules().Delete(context.TODO(), nfr.Name, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())
	}
}

// Actual test suite
var _ = SIGDescribe("Node Feature Discovery", func() {
	f := framework.NewDefaultFramework("node-feature-discovery")

	Context("when deploying a single nfd-master pod", Ordered, func() {
		var (
			masterPod *corev1.Pod
			crds      []*apiextensionsv1.CustomResourceDefinition
			extClient *extclient.Clientset
			nfdClient *nfdclient.Clientset
		)

		BeforeAll(func() {
			// Create clients for apiextensions and our CRD api
			extClient = extclient.NewForConfigOrDie(f.ClientConfig())
			nfdClient = nfdclient.NewForConfigOrDie(f.ClientConfig())

			By("Creating NFD CRDs")
			var err error
			crds, err = testutils.CreateNfdCRDs(extClient)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			for _, crd := range crds {
				err := extClient.ApiextensionsV1().CustomResourceDefinitions().Delete(context.TODO(), crd.Name, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		})

		BeforeEach(func() {
			// Drop the pod security admission label as nfd-worker needs host mounts
			if _, ok := f.Namespace.Labels[admissionapi.EnforceLevelLabel]; ok {
				e2elog.Logf("Deleting %s label from the test namespace", admissionapi.EnforceLevelLabel)
				delete(f.Namespace.Labels, admissionapi.EnforceLevelLabel)
				_, err := f.ClientSet.CoreV1().Namespaces().Update(context.TODO(), f.Namespace, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			err := testutils.ConfigureRBAC(f.ClientSet, f.Namespace.Name)
			Expect(err).NotTo(HaveOccurred())

			// Remove pre-existing stale annotations and labels etc and CRDs
			cleanupNode(f.ClientSet)
			cleanupCRDs(nfdClient)

			// Launch nfd-master
			By("Creating nfd master pod and nfd-master service")
			imageOpt := testpod.SpecWithContainerImage(dockerImage)
			masterPod = e2epod.NewPodClient(f).CreateSync(testpod.NFDMaster(imageOpt))

			// Create nfd-master service
			nfdSvc, err := testutils.CreateService(f.ClientSet, f.Namespace.Name)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for the nfd-master pod to be running")
			Expect(e2epod.WaitTimeoutForPodRunningInNamespace(f.ClientSet, masterPod.Name, masterPod.Namespace, time.Minute)).NotTo(HaveOccurred())

			By("Verifying the node where nfd-master is running")
			// Get updated masterPod object (we want to know where it was scheduled)
			masterPod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), masterPod.ObjectMeta.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			// Node running nfd-master should have master version annotation
			masterPodNode, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), masterPod.Spec.NodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(masterPodNode.Annotations).To(HaveKey(nfdv1alpha1.AnnotationNs + "/master.version"))

			By("Waiting for the nfd-master service to be up")
			Expect(e2enetwork.WaitForService(f.ClientSet, f.Namespace.Name, nfdSvc.ObjectMeta.Name, true, time.Second, 10*time.Second)).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			Expect(testutils.DeconfigureRBAC(f.ClientSet, f.Namespace.Name)).NotTo(HaveOccurred())

			cleanupNode(f.ClientSet)
			cleanupCRDs(nfdClient)
		})

		//
		// Simple test with only the fake source enabled
		//
		Context("and a single worker pod with fake source enabled", func() {
			It("it should decorate the node with the fake feature labels", func() {

				fakeFeatureLabels := map[string]string{
					nfdv1alpha1.FeatureLabelNs + "/fake-fakefeature1": "true",
					nfdv1alpha1.FeatureLabelNs + "/fake-fakefeature2": "true",
					nfdv1alpha1.FeatureLabelNs + "/fake-fakefeature3": "true",
				}

				// Launch nfd-worker
				By("Creating a nfd worker pod")
				podSpecOpts := []testpod.SpecOption{
					testpod.SpecWithRestartPolicy(corev1.RestartPolicyNever),
					testpod.SpecWithContainerImage(dockerImage),
					testpod.SpecWithContainerExtraArgs("-oneshot", "-label-sources=fake"),
				}
				workerPod := testpod.NFDWorker(podSpecOpts...)
				workerPod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Create(context.TODO(), workerPod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("Waiting for the nfd-worker pod to succeed")
				Expect(e2epod.WaitForPodSuccessInNamespace(f.ClientSet, workerPod.ObjectMeta.Name, f.Namespace.Name)).NotTo(HaveOccurred())
				workerPod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.TODO(), workerPod.ObjectMeta.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				By(fmt.Sprintf("Making sure '%s' was decorated with the fake feature labels", workerPod.Spec.NodeName))
				node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), workerPod.Spec.NodeName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				for k, v := range fakeFeatureLabels {
					Expect(node.Labels[k]).To(Equal(v))
				}

				// Check that there are no unexpected NFD labels
				for k := range node.Labels {
					if strings.HasPrefix(k, nfdv1alpha1.FeatureLabelNs) {
						Expect(fakeFeatureLabels).Should(HaveKey(k))
					}
				}

				By("Deleting the node-feature-discovery worker pod")
				err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(context.TODO(), workerPod.ObjectMeta.Name, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		//
		// More comprehensive test when --e2e-node-config is enabled
		//
		Context("and nfd-workers as a daemonset with default sources enabled", func() {
			It("the node labels and annotations listed in the e2e config should be present", func() {
				cfg, err := testutils.GetConfig()
				Expect(err).ToNot(HaveOccurred())

				if cfg == nil {
					Skip("no e2e-config was specified")
				}
				if cfg.DefaultFeatures == nil {
					Skip("no 'defaultFeatures' specified in e2e-config")
				}
				fConf := cfg.DefaultFeatures

				By("Creating nfd-worker daemonset")
				podSpecOpts := []testpod.SpecOption{testpod.SpecWithContainerImage(dockerImage)}
				workerDS := testds.NFDWorker(podSpecOpts...)
				workerDS, err = f.ClientSet.AppsV1().DaemonSets(f.Namespace.Name).Create(context.TODO(), workerDS, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("Waiting for daemonset pods to be ready")
				Expect(testpod.WaitForReady(f.ClientSet, f.Namespace.Name, workerDS.Spec.Template.Labels["name"], 5)).NotTo(HaveOccurred())

				By("Getting node objects")
				nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())

				for _, node := range nodeList.Items {
					nodeConf := testutils.FindNodeConfig(cfg, node.Name)
					if nodeConf == nil {
						e2elog.Logf("node %q has no matching rule in e2e-config, skipping...", node.Name)
						continue
					}

					// Check labels
					e2elog.Logf("verifying labels of node %q...", node.Name)
					for k, v := range nodeConf.ExpectedLabelValues {
						Expect(node.Labels).To(HaveKeyWithValue(k, v))
					}
					for k := range nodeConf.ExpectedLabelKeys {
						Expect(node.Labels).To(HaveKey(k))
					}
					for k := range node.Labels {
						if strings.HasPrefix(k, nfdv1alpha1.FeatureLabelNs) {
							if _, ok := nodeConf.ExpectedLabelValues[k]; ok {
								continue
							}
							if _, ok := nodeConf.ExpectedLabelKeys[k]; ok {
								continue
							}
							// Ignore if the label key was not whitelisted
							Expect(fConf.LabelWhitelist).NotTo(HaveKey(k))
						}
					}

					// Check annotations
					e2elog.Logf("verifying annotations of node %q...", node.Name)
					for k, v := range nodeConf.ExpectedAnnotationValues {
						Expect(node.Annotations).To(HaveKeyWithValue(k, v))
					}
					for k := range nodeConf.ExpectedAnnotationKeys {
						Expect(node.Annotations).To(HaveKey(k))
					}
					for k := range node.Annotations {
						if strings.HasPrefix(k, nfdv1alpha1.AnnotationNs) {
							if _, ok := nodeConf.ExpectedAnnotationValues[k]; ok {
								continue
							}
							if _, ok := nodeConf.ExpectedAnnotationKeys[k]; ok {
								continue
							}
							// Ignore if the annotation was not whitelisted
							Expect(fConf.AnnotationWhitelist).NotTo(HaveKey(k))
						}
					}

				}

				By("Deleting nfd-worker daemonset")
				err = f.ClientSet.AppsV1().DaemonSets(f.Namespace.Name).Delete(context.TODO(), workerDS.ObjectMeta.Name, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		//
		// Test custom nodename source configured in 2 additional ConfigMaps
		//
		Context("and nfd-workers as a daemonset with 2 additional configmaps for the custom source configured", func() {
			It("the nodename matching features listed in the configmaps should be present", func() {
				By("Getting a worker node")

				// We need a valid nodename for the configmap
				nodes, err := getNonControlPlaneNodes(f.ClientSet)
				Expect(err).NotTo(HaveOccurred())

				targetNodeName := nodes[0].Name
				Expect(targetNodeName).ToNot(BeEmpty(), "No worker node found")

				// create a wildcard name as well for this node
				targetNodeNameWildcard := fmt.Sprintf("%s.*%s", targetNodeName[:2], targetNodeName[4:])

				By("Creating the configmaps")
				targetLabelName := "nodename-test"
				targetLabelValue := "true"

				targetLabelNameWildcard := "nodename-test-wildcard"
				targetLabelValueWildcard := "customValue"

				targetLabelNameNegative := "nodename-test-negative"

				// create 2 configmaps
				data1 := `
- name: ` + targetLabelName + `
  matchOn:
  # default value is true
  - nodename:
    - ` + targetNodeName

				cm1 := testutils.NewConfigMap("custom-config-extra-1", "custom.conf", data1)
				cm1, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(context.TODO(), cm1, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				data2 := `
- name: ` + targetLabelNameWildcard + `
  value: ` + targetLabelValueWildcard + `
  matchOn:
  - nodename:
    - ` + targetNodeNameWildcard + `
- name: ` + targetLabelNameNegative + `
  matchOn:
  - nodename:
    - "thisNameShouldNeverMatch"`

				cm2 := testutils.NewConfigMap("custom-config-extra-2", "custom.conf", data2)
				cm2, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(context.TODO(), cm2, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("Creating nfd-worker daemonset with configmap mounted")
				podSpecOpts := []testpod.SpecOption{
					testpod.SpecWithContainerImage(dockerImage),
					testpod.SpecWithConfigMap(cm1.Name, filepath.Join(custom.Directory, "cm1")),
					testpod.SpecWithConfigMap(cm2.Name, filepath.Join(custom.Directory, "cm2")),
				}
				workerDS := testds.NFDWorker(podSpecOpts...)

				workerDS, err = f.ClientSet.AppsV1().DaemonSets(f.Namespace.Name).Create(context.TODO(), workerDS, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("Waiting for daemonset pods to be ready")
				Expect(testpod.WaitForReady(f.ClientSet, f.Namespace.Name, workerDS.Spec.Template.Labels["name"], 5)).NotTo(HaveOccurred())

				By("Getting target node and checking labels")
				targetNode, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), targetNodeName, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())

				labelFound := false
				labelWildcardFound := false
				labelNegativeFound := false
				for k := range targetNode.Labels {
					if strings.Contains(k, targetLabelName) {
						if targetNode.Labels[k] == targetLabelValue {
							labelFound = true
						}
					}
					if strings.Contains(k, targetLabelNameWildcard) {
						if targetNode.Labels[k] == targetLabelValueWildcard {
							labelWildcardFound = true
						}
					}
					if strings.Contains(k, targetLabelNameNegative) {
						labelNegativeFound = true
					}
				}

				Expect(labelFound).To(BeTrue(), "label not found!")
				Expect(labelWildcardFound).To(BeTrue(), "label for wildcard nodename not found!")
				Expect(labelNegativeFound).To(BeFalse(), "label for not existing nodename found!")

				By("Deleting nfd-worker daemonset")
				err = f.ClientSet.AppsV1().DaemonSets(f.Namespace.Name).Delete(context.TODO(), workerDS.ObjectMeta.Name, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
			})
		})

		//
		// Test NodeFeatureRule
		//
		Context("and nfd-worker and NodeFeatureRules objects deployed", func() {
			It("custom labels from the NodeFeatureRule rules should be created", func() {
				By("Creating nfd-worker config")
				cm := testutils.NewConfigMap("nfd-worker-conf", "nfd-worker.conf", `
core:
  sleepInterval: "1s"
  featureSources: ["fake"]
  labelSources: []
`)
				cm, err := f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(context.TODO(), cm, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("Creating nfd-worker daemonset")
				podSpecOpts := []testpod.SpecOption{
					testpod.SpecWithContainerImage(dockerImage),
					testpod.SpecWithConfigMap(cm.Name, "/etc/kubernetes/node-feature-discovery"),
				}
				workerDS := testds.NFDWorker(podSpecOpts...)
				workerDS, err = f.ClientSet.AppsV1().DaemonSets(f.Namespace.Name).Create(context.TODO(), workerDS, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("Waiting for daemonset pods to be ready")
				Expect(testpod.WaitForReady(f.ClientSet, f.Namespace.Name, workerDS.Spec.Template.Labels["name"], 5)).NotTo(HaveOccurred())

				expected := map[string]string{
					"feature.node.kubernetes.io/e2e-flag-test-1":      "true",
					"feature.node.kubernetes.io/e2e-attribute-test-1": "true",
					"feature.node.kubernetes.io/e2e-instance-test-1":  "true"}

				By("Creating NodeFeatureRules #1")
				Expect(testutils.CreateNodeFeatureRulesFromFile(nfdClient, "nodefeaturerule-1.yaml")).NotTo(HaveOccurred())

				By("Verifying node labels from NodeFeatureRules #1")
				Expect(waitForNfdNodeLabels(f.ClientSet, expected)).NotTo(HaveOccurred())

				By("Creating NodeFeatureRules #2")
				Expect(testutils.CreateNodeFeatureRulesFromFile(nfdClient, "nodefeaturerule-2.yaml")).NotTo(HaveOccurred())

				// Add features from NodeFeatureRule #2
				expected["feature.node.kubernetes.io/e2e-matchany-test-1"] = "true"
				expected["feature.node.kubernetes.io/e2e-template-test-1-instance_1"] = "found"
				expected["feature.node.kubernetes.io/e2e-template-test-1-instance_2"] = "found"

				By("Verifying node labels from NodeFeatureRules #1 and #2")
				Expect(waitForNfdNodeLabels(f.ClientSet, expected)).NotTo(HaveOccurred())
			})
		})
	})
})

// waitForNfdNodeLabels waits for node to be labeled as expected.
func waitForNfdNodeLabels(cli clientset.Interface, expected map[string]string) error {
	poll := func() error {
		nodes, err := getNonControlPlaneNodes(cli)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			labels := nfdLabels(node.Labels)
			if !cmp.Equal(expected, labels) {
				return fmt.Errorf("node %q labels do not match expected, diff (expected vs. received): %s", node.Name, cmp.Diff(expected, labels))
			}
		}
		return nil
	}

	// Simple and stupid re-try loop
	var err error
	for retry := 0; retry < 3; retry++ {
		if err = poll(); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return err
}

// getNonControlPlaneNodes gets the nodes that are not tainted for exclusive control-plane usage
func getNonControlPlaneNodes(cli clientset.Interface) ([]corev1.Node, error) {
	nodeList, err := cli.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	if len(nodeList.Items) == 0 {
		return nil, fmt.Errorf("no nodes found in the cluster")
	}

	controlPlaneTaint := corev1.Taint{
		Effect: corev1.TaintEffectNoSchedule,
		Key:    "node-role.kubernetes.io/control-plane",
	}
	out := []corev1.Node{}
	for _, node := range nodeList.Items {
		if !taintutils.TaintExists(node.Spec.Taints, &controlPlaneTaint) {
			out = append(out, node)
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no non-control-plane nodes found in the cluster")
	}
	return out, nil
}

// nfdLabels gets labels that are in the nfd label namespace.
func nfdLabels(labels map[string]string) map[string]string {
	ret := map[string]string{}

	for key, val := range labels {
		if strings.HasPrefix(key, nfdv1alpha1.FeatureLabelNs) {
			ret[key] = val
		}
	}
	return ret

}
