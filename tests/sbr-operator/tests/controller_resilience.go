package tests

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/deployment"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/pod"
	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
)

// listSchedulableWorkerNodes returns worker nodes that are Ready, schedulable, and do NOT
// carry a control-plane or master role label. This prevents the resilience test from
// cordoning control-plane nodes on compact clusters.
func listSchedulableWorkerNodes() ([]corev1.Node, error) {
	nodeList, err := APIClient.CoreV1Interface.Nodes().List(
		context.TODO(),
		metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/worker"},
	)
	if err != nil {
		return nil, err
	}

	var eligible []corev1.Node

	for i := range nodeList.Items {
		node := &nodeList.Items[i]

		if node.Spec.Unschedulable {
			continue
		}

		if !helpers.IsNodeReady(node) {
			continue
		}

		if _, hasMaster := node.Labels["node-role.kubernetes.io/master"]; hasMaster {
			continue
		}

		if _, hasCP := node.Labels["node-role.kubernetes.io/control-plane"]; hasCP {
			continue
		}

		eligible = append(eligible, *node)
	}

	return eligible, nil
}

// setNodeUnschedulable patches a node's spec.unschedulable field.
func setNodeUnschedulable(nodeName string, unschedulable bool) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, unschedulable))

	_, err := APIClient.CoreV1Interface.Nodes().Patch(
		context.TODO(), nodeName, types.MergePatchType, patch, metav1.PatchOptions{})

	return err
}

// uncordonNodesBestEffort uncordons every node in the list, logging warnings on failure.
// Intended for DeferCleanup/AfterAll where masking the original failure is worse than
// leaving a node cordoned.
func uncordonNodesBestEffort(nodeNames []string) {
	for _, name := range nodeNames {
		if err := setNodeUnschedulable(name, false); err != nil {
			GinkgoWriter.Printf("Warning: cleanup failed to uncordon node %s: %v\n", name, err)
		}
	}
}

// listRunningControllerPods returns SBR controller pods that are Running, not terminating,
// and have all containers ready.
func listRunningControllerPods() ([]*pod.Builder, error) {
	controllerPods, err := pod.List(
		APIClient, medik8sparams.OperatorNs,
		metav1.ListOptions{LabelSelector: sbrparams.OperatorControllerPodLabelSelector})
	if err != nil {
		return nil, err
	}

	return helpers.FilterRunningPods(controllerPods), nil
}

// uniqueNodeNames returns the set of distinct node names that the given pods are scheduled on.
func uniqueNodeNames(pods []*pod.Builder) map[string]bool {
	nodes := make(map[string]bool, len(pods))
	for _, p := range pods {
		if p.Object.Spec.NodeName != "" {
			nodes[p.Object.Spec.NodeName] = true
		}
	}

	return nodes
}

var _ = Describe(
	"SBR Controller Resilience",
	Ordered,
	ContinueOnFailure,
	Serial,
	Label(labels.OperatorSBR), func() {
		BeforeAll(func() {
			By("Verifying SBR controller deployment is Ready")

			sbrDeployment, err := deployment.Pull(
				APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
			Expect(err).ToNot(HaveOccurred(), "Failed to get SBR deployment")
			Expect(sbrDeployment.IsReady(medik8sparams.DefaultTimeout)).To(BeTrue(),
				"SBR deployment is not Ready")
		})

		It("should maintain controller availability with one worker",
			reportxml.ID("90306"),
			Label(
				labels.DisruptionDestructive,
				labels.TierAcceptance,
				labels.FrequencyWeekly,
				labels.PlatformAny,
				labels.ComponentController,
			), func() {
				By("Listing schedulable worker-only nodes")

				workerNodes, err := listSchedulableWorkerNodes()
				Expect(err).ToNot(HaveOccurred(), "Failed to list worker nodes")

				if len(workerNodes) < sbrparams.MinWorkerNodesForResilienceTest {
					Skip(fmt.Sprintf(
						"Test requires at least %d schedulable worker-only nodes, found %d",
						sbrparams.MinWorkerNodesForResilienceTest, len(workerNodes)))
				}

				By("Verifying SBR controller starts with expected replicas on different nodes")

				var initialPods []*pod.Builder

				Eventually(func(assertion Gomega) {
					pods, listErr := listRunningControllerPods()
					assertion.Expect(listErr).ToNot(HaveOccurred())
					assertion.Expect(len(pods)).To(Equal(int(sbrparams.ExpectedReplicas)),
						"expected %d running controller pods", sbrparams.ExpectedReplicas)
					assertion.Expect(len(uniqueNodeNames(pods))).To(Equal(int(sbrparams.ExpectedReplicas)),
						"controller pods must run on different nodes for HA")

					initialPods = pods
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"SBR controller did not stabilize at %d ready replicas on distinct nodes",
					sbrparams.ExpectedReplicas)

				By("Selecting one worker to keep (one already hosting a controller pod)")

				controllerNodeNames := uniqueNodeNames(initialPods)
				workerNodeNames := make(map[string]bool, len(workerNodes))

				for _, node := range workerNodes {
					workerNodeNames[node.Name] = true
				}

				var keeperNode string

				for _, p := range initialPods {
					if workerNodeNames[p.Object.Spec.NodeName] {
						keeperNode = p.Object.Spec.NodeName

						break
					}
				}

				Expect(keeperNode).ToNot(BeEmpty(),
					"No controller pod runs on an eligible worker node (controller nodes: %v, workers: %v)",
					controllerNodeNames, workerNodeNames)

				GinkgoWriter.Printf("Keeper node: %s\n", keeperNode)

				var nodesToCordon []string

				for _, node := range workerNodes {
					if node.Name != keeperNode {
						nodesToCordon = append(nodesToCordon, node.Name)
					}
				}

				var cordonedNodes []string

				DeferCleanup(func() {
					By("DeferCleanup: uncordoning all nodes modified by the test")

					uncordonNodesBestEffort(cordonedNodes)

					By("DeferCleanup: waiting for controller to return to expected replicas")

					Eventually(func() int32 {
						dep, pullErr := deployment.Pull(
							APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
						if pullErr != nil {
							return 0
						}

						return dep.Object.Status.ReadyReplicas
					}, sbrparams.ControllerScaleBackTimeout, sbrparams.DefaultPollInterval).Should(
						Equal(sbrparams.ExpectedReplicas),
						"Controller deployment did not recover to %d ready replicas during cleanup",
						sbrparams.ExpectedReplicas)
				})

				By(fmt.Sprintf("Cordoning %d worker node(s), leaving %s schedulable",
					len(nodesToCordon), keeperNode))

				for _, nodeName := range nodesToCordon {
					Expect(setNodeUnschedulable(nodeName, true)).To(Succeed(),
						"Failed to cordon node %s", nodeName)
					cordonedNodes = append(cordonedNodes, nodeName)
				}

				By("Verifying cordoned nodes report Unschedulable before proceeding")

				for _, nodeName := range cordonedNodes {
					Eventually(func() bool {
						node, getErr := APIClient.CoreV1Interface.Nodes().Get(
							context.TODO(), nodeName, metav1.GetOptions{})
						if getErr != nil {
							return false
						}

						return node.Spec.Unschedulable
					}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
						"Node %s did not become unschedulable", nodeName)
				}

				By("Deleting controller pods from cordoned nodes")

				for _, controllerPod := range initialPods {
					if controllerPod.Object.Spec.NodeName == keeperNode {
						continue
					}

					podName := controllerPod.Object.Name
					podNode := controllerPod.Object.Spec.NodeName

					GinkgoWriter.Printf("Deleting controller pod %s from cordoned node %s\n",
						podName, podNode)

					delErr := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).Delete(
						context.TODO(), podName, metav1.DeleteOptions{})
					Expect(delErr).ToNot(HaveOccurred(),
						"Failed to delete controller pod %s", podName)
				}

				By("Waiting for deleted pods to terminate")

				for _, controllerPod := range initialPods {
					if controllerPod.Object.Spec.NodeName == keeperNode {
						continue
					}

					Eventually(func() bool {
						_, getErr := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).Get(
							context.TODO(), controllerPod.Object.Name, metav1.GetOptions{})

						return k8serrors.IsNotFound(getErr)
					}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
						"Deleted pod %s did not terminate", controllerPod.Object.Name)
				}

				By("Verifying at least one controller pod is Running on the keeper node")

				Eventually(func(assertion Gomega) {
					pods, listErr := listRunningControllerPods()
					assertion.Expect(listErr).ToNot(HaveOccurred())
					assertion.Expect(len(pods)).To(BeNumerically(">=", 1),
						"expected at least 1 running controller pod")

					hasKeeperPod := false

					for _, p := range pods {
						if p.Object.Spec.NodeName == keeperNode {
							hasKeeperPod = true

							break
						}
					}

					assertion.Expect(hasKeeperPod).To(BeTrue(),
						"No running controller pod on keeper node %s", keeperNode)
				}, sbrparams.ControllerRescheduleTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Controller did not stabilize with at least 1 ready replica on the keeper node")

				By("Verifying deployment reports at least 1 ready replica")

				Eventually(func() int32 {
					dep, pullErr := deployment.Pull(
						APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
					if pullErr != nil {
						return 0
					}

					return dep.Object.Status.ReadyReplicas
				}, sbrparams.ControllerRescheduleTimeout, sbrparams.DefaultPollInterval).Should(
					BeNumerically(">=", int32(1)),
					"Deployment did not report at least 1 ready replica while degraded")

				By("Consistently verifying controller stays available during degraded phase")

				Consistently(func(assertion Gomega) {
					dep, pullErr := deployment.Pull(
						APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
					assertion.Expect(pullErr).ToNot(HaveOccurred())
					assertion.Expect(dep.Object.Status.AvailableReplicas).To(BeNumerically(">=", int32(1)),
						"Controller deployment must maintain at least 1 available replica")
				}, sbrparams.ControllerDegradedConsistentDuration, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Controller availability was not sustained during the single-worker degraded phase")

				By("Uncordoning all worker nodes")

				uncordonNodesBestEffort(cordonedNodes)

				By("Verifying controller scales back to expected replicas on different nodes")

				Eventually(func(assertion Gomega) {
					dep, pullErr := deployment.Pull(
						APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
					assertion.Expect(pullErr).ToNot(HaveOccurred())
					assertion.Expect(dep.Object.Status.ReadyReplicas).To(Equal(sbrparams.ExpectedReplicas),
						"expected %d ready replicas after uncordoning", sbrparams.ExpectedReplicas)

					pods, listErr := listRunningControllerPods()
					assertion.Expect(listErr).ToNot(HaveOccurred())
					assertion.Expect(len(pods)).To(Equal(int(sbrparams.ExpectedReplicas)))
					assertion.Expect(len(uniqueNodeNames(pods))).To(Equal(int(sbrparams.ExpectedReplicas)),
						"Controller pods must run on different nodes after recovery")
				}, sbrparams.ControllerScaleBackTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Controller deployment did not scale back to %d ready replicas on distinct nodes",
					sbrparams.ExpectedReplicas)
			})

		It("should transfer controller leadership when active pod is deleted",
			reportxml.ID("90307"),
			Label(
				labels.DisruptionNonDestructive,
				labels.TierAcceptance,
				labels.FrequencyWeekly,
				labels.PlatformAny,
				labels.ComponentController,
			), func() {
				ctx := context.Background()

				By("Verifying cluster has enough workers for leadership handover")

				workerNodes, err := listSchedulableWorkerNodes()
				Expect(err).ToNot(HaveOccurred(), "Failed to list worker nodes")

				if len(workerNodes) < sbrparams.MinWorkerNodesForHandoverTest {
					Skip(fmt.Sprintf(
						"Leadership handover test requires at least %d schedulable worker-only nodes, found %d",
						sbrparams.MinWorkerNodesForHandoverTest, len(workerNodes)))
				}

				By("Verifying SBR controller deployment starts healthy")

				Eventually(func(assertion Gomega) {
					dep, pullErr := deployment.Pull(
						APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
					assertion.Expect(pullErr).ToNot(HaveOccurred())
					assertion.Expect(dep.Object.Status.ReadyReplicas).To(Equal(sbrparams.ExpectedReplicas),
						"expected %d ready replicas before handover test", sbrparams.ExpectedReplicas)
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(Succeed())

				By("Reading the leader election lease to identify the current leader")

				lease := &coordinationv1.Lease{}
				Expect(APIClient.Get(ctx, types.NamespacedName{
					Name:      sbrparams.ControllerLeaseName,
					Namespace: medik8sparams.OperatorNs,
				}, lease)).To(Succeed(), "Failed to get SBR leader election lease")

				Expect(lease.Spec.HolderIdentity).ToNot(BeNil(),
					"Leader election lease has no holder")

				oldLeaderIdentity := *lease.Spec.HolderIdentity

				GinkgoWriter.Printf("Current leader identity: %s\n", oldLeaderIdentity)

				By("Finding the leader controller pod")

				oldLeaderPodName, _, hasSuffix := strings.Cut(oldLeaderIdentity, "_")
				if !hasSuffix || oldLeaderPodName == "" {
					Fail(fmt.Sprintf(
						"unexpected leader holderIdentity format %q; expected <podname>_<uuid>",
						oldLeaderIdentity))
				}

				controllerPods, err := pod.List(APIClient, medik8sparams.OperatorNs,
					metav1.ListOptions{LabelSelector: sbrparams.OperatorControllerPodLabelSelector})
				Expect(err).ToNot(HaveOccurred(), "Failed to list controller pods")

				var leaderPod *pod.Builder

				for _, controllerPod := range controllerPods {
					if controllerPod.Object.Name == oldLeaderPodName {
						leaderPod = controllerPod

						break
					}
				}

				Expect(leaderPod).ToNot(BeNil(),
					"Could not find controller pod matching leader pod name %q (from identity %q)",
					oldLeaderPodName, oldLeaderIdentity)

				By("Deleting the leader controller pod " + leaderPod.Object.Name)

				delErr := APIClient.CoreV1Interface.Pods(medik8sparams.OperatorNs).Delete(
					context.TODO(), leaderPod.Object.Name, metav1.DeleteOptions{})
				Expect(delErr).ToNot(HaveOccurred(),
					"Failed to delete leader pod %s", leaderPod.Object.Name)

				By("Waiting for SBR controller deployment to return to full ready replicas")

				sbrDeployment, err := deployment.Pull(
					APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
				Expect(err).ToNot(HaveOccurred())
				Expect(sbrDeployment.IsReady(sbrparams.ControllerHandoverTimeout)).To(BeTrue(),
					"SBR deployment did not become ready after leader pod deletion")

				By("Verifying controller lease transferred to a different pod")

				Eventually(func(assertion Gomega) {
					updatedLease := &coordinationv1.Lease{}
					assertion.Expect(APIClient.Get(ctx, types.NamespacedName{
						Name:      sbrparams.ControllerLeaseName,
						Namespace: medik8sparams.OperatorNs,
					}, updatedLease)).To(Succeed())

					assertion.Expect(updatedLease.Spec.HolderIdentity).ToNot(BeNil(),
						"Lease has no holder after pod deletion")
					assertion.Expect(*updatedLease.Spec.HolderIdentity).ToNot(Equal(oldLeaderIdentity),
						"Lease is still held by deleted pod %s", oldLeaderIdentity)

					newLeaderIdentity := *updatedLease.Spec.HolderIdentity
					newLeaderPodName, _, _ := strings.Cut(newLeaderIdentity, "_")

					pods, listErr := listRunningControllerPods()
					assertion.Expect(listErr).ToNot(HaveOccurred())

					hasNewLeaderPod := false

					for _, runningPod := range pods {
						if runningPod.Object.Name == newLeaderPodName {
							hasNewLeaderPod = true

							break
						}
					}

					assertion.Expect(hasNewLeaderPod).To(BeTrue(),
						"New lease holder %q (pod %q) does not match any running controller pod",
						newLeaderIdentity, newLeaderPodName)
				}, sbrparams.ControllerHandoverTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Controller leadership did not transfer after pod deletion")

				By("Verifying deployment has full ready replicas after handover")

				Eventually(func() int32 {
					dep, pullErr := deployment.Pull(
						APIClient, sbrparams.OperatorDeploymentName, medik8sparams.OperatorNs)
					if pullErr != nil {
						return 0
					}

					return dep.Object.Status.ReadyReplicas
				}, sbrparams.ControllerHandoverTimeout, sbrparams.DefaultPollInterval).Should(
					Equal(sbrparams.ExpectedReplicas),
					"Deployment did not return to %d ready replicas after leadership handover",
					sbrparams.ExpectedReplicas)
			})
	})
