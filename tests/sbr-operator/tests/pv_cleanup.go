package tests

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rh-ecosystem-edge/eco-goinfra/pkg/reportxml"

	"github.com/medik8s/system-tests/tests/internal/labels"
	. "github.com/medik8s/system-tests/tests/internal/medik8sinittools"
	"github.com/medik8s/system-tests/tests/internal/medik8sparams"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// stalePVCName returns the PVC name that testRWXSupport would create for the given SBRC name.
func stalePVCName(sbrcName string) string {
	return sbrcName + "-rwx-test"
}

// buildTestPV constructs a static NFS PV for use in the PV cleanup tests.
func buildTestPV(name, scName, nfsServer, nfsPath string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              scName,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{
					Server: nfsServer,
					Path:   nfsPath,
				},
			},
		},
	}
}

// deleteTestPV removes a PV created by this test. Best-effort: logs a warning on failure.
func deleteTestPV(pvName string) {
	err := APIClient.CoreV1Interface.PersistentVolumes().Delete(
		context.TODO(), pvName, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		GinkgoT().Logf("Warning: failed to delete test PV %q: %v", pvName, err)
	}
}

var _ = Describe(
	"SBR PV Cleanup — Unknown Provisioner",
	Ordered,
	ContinueOnFailure,
	Label(
		labels.OperatorSBR,
		labels.TierAcceptance,
		labels.FrequencyWeekly,
		labels.DisruptionNonDestructive,
		labels.PlatformAny,
		labels.ComponentPostDeploy,
	), func() {
		var sbrcReconciled bool

		BeforeAll(func() {
			By(fmt.Sprintf("Checking StorageClass %q exists (created by CI NFS bastion step)",
				sbrparams.PVCleanupSCName))

			_, scErr := APIClient.StorageV1Interface.StorageClasses().Get(
				context.TODO(), sbrparams.PVCleanupSCName, metav1.GetOptions{})
			if k8serrors.IsNotFound(scErr) {
				Skip(fmt.Sprintf("StorageClass %q not found; NFS bastion infrastructure not deployed — skipping PV cleanup tests",
					sbrparams.PVCleanupSCName))
			}

			Expect(scErr).ToNot(HaveOccurred(),
				"Failed to get StorageClass %q", sbrparams.PVCleanupSCName)

			By(fmt.Sprintf("Reading reference PV %q to discover NFS server and path",
				sbrparams.PVCleanupReferencePVName))

			refPV, pvErr := APIClient.CoreV1Interface.PersistentVolumes().Get(
				context.TODO(), sbrparams.PVCleanupReferencePVName, metav1.GetOptions{})
			if k8serrors.IsNotFound(pvErr) {
				Skip(fmt.Sprintf("Reference PV %q not found; NFS bastion infrastructure not deployed — skipping PV cleanup tests",
					sbrparams.PVCleanupReferencePVName))
			}

			Expect(pvErr).ToNot(HaveOccurred(),
				"Failed to get reference PV %q", sbrparams.PVCleanupReferencePVName)

			if refPV.Spec.NFS == nil {
				Skip(fmt.Sprintf("Reference PV %q does not use NFS volume source — skipping PV cleanup tests",
					sbrparams.PVCleanupReferencePVName))
			}

			nfsServer := refPV.Spec.NFS.Server
			nfsPath := refPV.Spec.NFS.Path

			GinkgoWriter.Printf("NFS reference: server=%s path=%s\n", nfsServer, nfsPath)

			By("Cleaning up any leftover test PVs from a prior run")

			for _, pvName := range []string{sbrparams.PVCleanupTestMainPVName, sbrparams.PVCleanupTestDecoyPVName} {
				deleteTestPV(pvName)
			}

			By("Cleaning up any stale test SBRC from a prior run")

			staleRef := buildSBRC(sbrparams.PVCleanupSBRCName, map[string]interface{}{})

			deleteErr := APIClient.Delete(context.TODO(), staleRef)
			if deleteErr != nil && !k8serrors.IsNotFound(deleteErr) {
				GinkgoT().Logf("Warning: pre-test cleanup of stale SBRC %s failed: %v",
					sbrparams.PVCleanupSBRCName, deleteErr)
			}

			if deleteErr == nil {
				By("Waiting for stale test SBRC to be fully deleted")

				Eventually(func() bool {
					getErr := APIClient.Get(context.TODO(),
						types.NamespacedName{Name: sbrparams.PVCleanupSBRCName, Namespace: medik8sparams.OperatorNs},
						buildSBRC(sbrparams.PVCleanupSBRCName, map[string]interface{}{}))

					return k8serrors.IsNotFound(getErr)
				}, medik8sparams.DefaultTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
					"Stale SBRC %s must be fully deleted", sbrparams.PVCleanupSBRCName)
			}

			By(fmt.Sprintf("Creating test PVs %q and %q (1Gi, Retain, SC %s)",
				sbrparams.PVCleanupTestMainPVName, sbrparams.PVCleanupTestDecoyPVName, sbrparams.PVCleanupSCName))

			mainPV := buildTestPV(sbrparams.PVCleanupTestMainPVName, sbrparams.PVCleanupSCName, nfsServer, nfsPath)
			_, createErr := APIClient.CoreV1Interface.PersistentVolumes().Create(
				context.TODO(), mainPV, metav1.CreateOptions{})
			Expect(createErr).ToNot(HaveOccurred(),
				"Failed to create test PV %q", sbrparams.PVCleanupTestMainPVName)

			decoyPV := buildTestPV(sbrparams.PVCleanupTestDecoyPVName, sbrparams.PVCleanupSCName, nfsServer, nfsPath)
			_, createErr = APIClient.CoreV1Interface.PersistentVolumes().Create(
				context.TODO(), decoyPV, metav1.CreateOptions{})
			Expect(createErr).ToNot(HaveOccurred(),
				"Failed to create test PV %q", sbrparams.PVCleanupTestDecoyPVName)
		})

		AfterAll(func() {
			By("Cleaning up test SBRC if still present")

			sbrcRef := buildSBRC(sbrparams.PVCleanupSBRCName, map[string]interface{}{})

			deleteErr := APIClient.Delete(context.TODO(), sbrcRef)
			if deleteErr != nil && !k8serrors.IsNotFound(deleteErr) {
				GinkgoT().Logf("Warning: AfterAll cleanup of SBRC %s failed: %v",
					sbrparams.PVCleanupSBRCName, deleteErr)
			}

			if deleteErr == nil {
				Eventually(func() bool {
					getErr := APIClient.Get(context.TODO(),
						types.NamespacedName{Name: sbrparams.PVCleanupSBRCName, Namespace: medik8sparams.OperatorNs},
						buildSBRC(sbrparams.PVCleanupSBRCName, map[string]interface{}{}))

					return k8serrors.IsNotFound(getErr)
				}, sbrparams.PVCleanupDeletionTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
					"Test SBRC %s must be fully deleted in AfterAll", sbrparams.PVCleanupSBRCName)
			}

			By("Deleting test PVs")

			deleteTestPV(sbrparams.PVCleanupTestMainPVName)
			deleteTestPV(sbrparams.PVCleanupTestDecoyPVName)
		})

		// TODO: create Polarion test case and replace placeholder ID
		It("Verify operator handles a stale RWX test PVC and reconciles successfully (RHWA-1017)",
			reportxml.ID("TODO"),
			func() {
				testPVCName := stalePVCName(sbrparams.PVCleanupSBRCName)
				fakeSC := sbrparams.PVCleanupStalePVCSC

				By(fmt.Sprintf("Pre-creating stale PVC %q with non-matching SC %q to simulate leftover from crashed operator",
					testPVCName, fakeSC))

				stalePVC := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testPVCName,
						Namespace: medik8sparams.OperatorNs,
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
						StorageClassName: &fakeSC,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				}

				_, createErr := APIClient.CoreV1Interface.PersistentVolumeClaims(medik8sparams.OperatorNs).Create(
					context.TODO(), stalePVC, metav1.CreateOptions{})
				Expect(createErr).ToNot(HaveOccurred(),
					"Failed to pre-create stale PVC %q", testPVCName)

				DeferCleanup(func() {
					_ = APIClient.CoreV1Interface.PersistentVolumeClaims(medik8sparams.OperatorNs).Delete(
						context.TODO(), testPVCName, metav1.DeleteOptions{})
				})

				By(fmt.Sprintf("Creating SBRC %q pointing to SC %q (unknown provisioner triggers testRWXSupport)",
					sbrparams.PVCleanupSBRCName, sbrparams.PVCleanupSCName))

				sbrc := buildSBRC(sbrparams.PVCleanupSBRCName, map[string]interface{}{
					"sharedStorageClass": sbrparams.PVCleanupSCName,
				})
				Expect(APIClient.Create(context.TODO(), sbrc)).To(Succeed(),
					"SBRC %q must be created successfully", sbrparams.PVCleanupSBRCName)

				By("Waiting for SBRC agent DaemonSet to become ready " +
					"(operator must delete the stale PVC and recreate it via testRWXSupport)")

				dsName := sbrparams.SBRAgentDaemonSetPrefix + sbrparams.PVCleanupSBRCName

				Eventually(func() error {
					agentDS, err := APIClient.DaemonSets(medik8sparams.OperatorNs).Get(
						context.TODO(), dsName, metav1.GetOptions{})
					if err != nil {
						return fmt.Errorf("DaemonSet %s not found — operator likely blocked "+
							"by stale PVC %q (AlreadyExists on testRWXSupport): %w",
							dsName, testPVCName, err)
					}

					if agentDS.Status.DesiredNumberScheduled == 0 ||
						agentDS.Status.NumberReady < agentDS.Status.DesiredNumberScheduled {
						return fmt.Errorf("DaemonSet %s: %d/%d pods ready",
							dsName, agentDS.Status.NumberReady, agentDS.Status.DesiredNumberScheduled)
					}

					return nil
				}, sbrparams.PVCleanupReconcileTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"SBRC %q reconciliation failed — operator could not handle stale PVC %q "+
						"(expected best-effort delete before PVC creation, see PR #60)",
					sbrparams.PVCleanupSBRCName, testPVCName)

				sbrcReconciled = true
			})

		// TODO: create Polarion test case and replace placeholder ID
		It("Verify PVs are not left in Released state after operator cleanup (RHWA-1046/1047)",
			reportxml.ID("TODO"),
			func() {
				if !sbrcReconciled {
					Skip("SBRC did not reconcile in the previous test (RHWA-1017 failed) — " +
						"PVs were never consumed, so Released-state checks would be meaningless")
				}

				By(fmt.Sprintf("Checking test decoy PV %q is not stuck in Released state "+
					"(testRWXSupport should have patched its reclaimPolicy to Delete)",
					sbrparams.PVCleanupTestDecoyPVName))

				Eventually(func() error {
					pv, getErr := APIClient.CoreV1Interface.PersistentVolumes().Get(
						context.TODO(), sbrparams.PVCleanupTestDecoyPVName, metav1.GetOptions{})
					if k8serrors.IsNotFound(getErr) {
						GinkgoWriter.Println("Decoy PV deleted (reclaimPolicy was patched to Delete) — fix verified")

						return nil
					}

					if getErr != nil {
						return getErr
					}

					if pv.Status.Phase == corev1.VolumeReleased {
						return fmt.Errorf("decoy PV %q is in Released state — "+
							"reclaimPolicy was NOT patched to Delete (bug present, see PR #62)",
							sbrparams.PVCleanupTestDecoyPVName)
					}

					return nil
				}, sbrparams.PVCleanupPVSettleTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Decoy PV should not remain in Released state after testRWXSupport cleanup")

				By(fmt.Sprintf("Deleting SBRC %q to trigger handleDeletion PV cleanup",
					sbrparams.PVCleanupSBRCName))

				sbrcRef := buildSBRC(sbrparams.PVCleanupSBRCName, map[string]interface{}{})
				Expect(APIClient.Delete(context.TODO(), sbrcRef)).To(Succeed(),
					"Failed to delete SBRC %q", sbrparams.PVCleanupSBRCName)

				Eventually(func() bool {
					getErr := APIClient.Get(context.TODO(),
						types.NamespacedName{Name: sbrparams.PVCleanupSBRCName, Namespace: medik8sparams.OperatorNs},
						buildSBRC(sbrparams.PVCleanupSBRCName, map[string]interface{}{}))

					return k8serrors.IsNotFound(getErr)
				}, sbrparams.PVCleanupDeletionTimeout, sbrparams.DefaultPollInterval).Should(BeTrue(),
					"SBRC %s must be fully deleted", sbrparams.PVCleanupSBRCName)

				By(fmt.Sprintf("Checking test main PV %q is not stuck in Released state "+
					"(handleDeletion should have patched its reclaimPolicy to Delete)",
					sbrparams.PVCleanupTestMainPVName))

				Eventually(func() error {
					pv, getErr := APIClient.CoreV1Interface.PersistentVolumes().Get(
						context.TODO(), sbrparams.PVCleanupTestMainPVName, metav1.GetOptions{})
					if k8serrors.IsNotFound(getErr) {
						GinkgoWriter.Println("Main PV deleted (reclaimPolicy was patched to Delete) — fix verified")

						return nil
					}

					if getErr != nil {
						return getErr
					}

					if pv.Status.Phase == corev1.VolumeReleased {
						return fmt.Errorf("main PV %q is in Released state — "+
							"reclaimPolicy was NOT patched to Delete (bug present, see PR #62)",
							sbrparams.PVCleanupTestMainPVName)
					}

					return nil
				}, sbrparams.PVCleanupPVSettleTimeout, sbrparams.DefaultPollInterval).Should(Succeed(),
					"Main PV should not remain in Released state after SBRC deletion")
			})
	})
