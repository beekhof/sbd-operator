/*
Copyright 2025.

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

package controller

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	medik8sv1alpha1 "github.com/medik8s/sbd-operator/api/v1alpha1"
	"github.com/medik8s/sbd-operator/pkg/blockdevice"
	"github.com/medik8s/sbd-operator/pkg/sbdprotocol"
)

// testableReconciler wraps SBDRemediationReconciler to make IsLeader mockable for testing
type testableReconciler struct {
	*SBDRemediationReconciler
	isLeaderFunc func() bool
}

func (r *testableReconciler) IsLeader() bool {
	if r.isLeaderFunc != nil {
		return r.isLeaderFunc()
	}
	return r.SBDRemediationReconciler.IsLeader()
}

var _ = Describe("SBDRemediation Controller", func() {
	Context("Node ID Mapping", func() {
		var reconciler *SBDRemediationReconciler

		BeforeEach(func() {
			reconciler = &SBDRemediationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
		})

		DescribeTable("should correctly map node names to node IDs",
			func(nodeName string, expectedNodeID uint16, shouldSucceed bool) {
				nodeID, err := reconciler.nodeNameToNodeID(nodeName)
				if shouldSucceed {
					Expect(err).NotTo(HaveOccurred())
					Expect(nodeID).To(Equal(expectedNodeID))
				} else {
					Expect(err).To(HaveOccurred())
				}
			},
			Entry("node-1", "node-1", uint16(1), true),
			Entry("worker-2", "worker-2", uint16(2), true),
			Entry("k8s-node-10", "k8s-node-10", uint16(10), true),
			Entry("control-plane-3", "control-plane-3", uint16(3), true),
			Entry("invalid-node", "invalid-node", uint16(0), false),
			Entry("node-0", "node-0", uint16(0), false),     // 0 is invalid
			Entry("node-255", "node-255", uint16(0), false), // 255 is reserved
			Entry("single-name", "hostname", uint16(0), false),
		)
	})

	Context("When reconciling a SBDRemediation resource", func() {
		var (
			reconciler     *SBDRemediationReconciler
			testReconciler *testableReconciler
			ctx            context.Context
			resourceName   string
			namespacedName types.NamespacedName
			tempDir        string
			mockSBDDevice  string
		)

		BeforeEach(func() {
			ctx = context.Background()
			resourceName = fmt.Sprintf("test-remediation-%d", time.Now().UnixNano())
			namespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: "default",
			}

			// Create temporary directory for mock SBD device
			var err error
			tempDir, err = ioutil.TempDir("", "sbd-controller-test-")
			Expect(err).NotTo(HaveOccurred())

			// Create mock SBD device file (512KB to accommodate all slots)
			mockSBDDevice = filepath.Join(tempDir, "sbd-device")
			err = ioutil.WriteFile(mockSBDDevice, make([]byte, 512*1024), 0644)
			Expect(err).NotTo(HaveOccurred())

			// Create reconciler with test configuration
			reconciler = &SBDRemediationReconciler{
				Client:                k8sClient,
				Scheme:                k8sClient.Scheme(),
				leaderElectionEnabled: false, // Disable for tests
				sbdDevicePath:         mockSBDDevice,
			}

			// Create testable reconciler
			testReconciler = &testableReconciler{
				SBDRemediationReconciler: reconciler,
			}
		})

		AfterEach(func() {
			// Clean up test resource
			resource := &medik8sv1alpha1.SBDRemediation{}
			err := k8sClient.Get(ctx, namespacedName, resource)
			if err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}

			// Clean up temporary files
			if reconciler.sbdDevice != nil {
				reconciler.sbdDevice.Close()
			}
			os.RemoveAll(tempDir)
		})

		Context("with leader election disabled", func() {
			It("should successfully fence a node", func() {
				By("Creating a SBDRemediation resource")
				resource := &medik8sv1alpha1.SBDRemediation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: medik8sv1alpha1.SBDRemediationSpec{
						NodeName: "worker-5",
						Reason:   medik8sv1alpha1.SBDRemediationReasonHeartbeatTimeout,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				By("Reconciling the resource")
				result, err := reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Requeue).To(BeFalse())

				By("Verifying the resource status")
				updatedResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())

				Expect(updatedResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully))
				Expect(updatedResource.Status.NodeID).NotTo(BeNil())
				Expect(*updatedResource.Status.NodeID).To(Equal(uint16(5)))
				Expect(updatedResource.Status.FenceMessageWritten).To(BeTrue())

				By("Verifying the fence message was written to the SBD device")
				device, err := blockdevice.Open(mockSBDDevice)
				Expect(err).NotTo(HaveOccurred())
				defer device.Close()

				// Read the message from node ID 5's slot
				slotOffset := int64(5) * sbdprotocol.SBD_SLOT_SIZE
				messageBytes := make([]byte, sbdprotocol.SBD_HEADER_SIZE)
				_, err = device.ReadAt(messageBytes, slotOffset)
				Expect(err).NotTo(HaveOccurred())

				// Unmarshal and verify the message
				header, err := sbdprotocol.Unmarshal(messageBytes)
				Expect(err).NotTo(HaveOccurred())
				Expect(header.Type).To(Equal(sbdprotocol.SBD_MSG_TYPE_FENCE))
				Expect(header.NodeID).To(Equal(OperatorNodeID))
			})

			It("should handle invalid node names gracefully", func() {
				By("Creating a SBDRemediation resource with invalid node name")
				resource := &medik8sv1alpha1.SBDRemediation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: medik8sv1alpha1.SBDRemediationSpec{
						NodeName: "invalid-node-name",
						Reason:   medik8sv1alpha1.SBDRemediationReasonNodeUnresponsive,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				By("Reconciling the resource")
				_, err := reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the resource failed with appropriate error")
				updatedResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())

				Expect(updatedResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseFailed))
				Expect(updatedResource.Status.Message).To(ContainSubstring("Failed to determine node ID"))
			})

			It("should handle SBD device errors gracefully", func() {
				By("Setting an invalid SBD device path")
				reconciler.sbdDevicePath = "/nonexistent/device"

				By("Creating a SBDRemediation resource")
				resource := &medik8sv1alpha1.SBDRemediation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: medik8sv1alpha1.SBDRemediationSpec{
						NodeName: "worker-3",
						Reason:   medik8sv1alpha1.SBDRemediationReasonManualFencing,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				By("Reconciling the resource")
				_, err := reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the resource failed with device error")
				updatedResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())

				Expect(updatedResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseFailed))
				Expect(updatedResource.Status.Message).To(ContainSubstring("Failed to initialize SBD device"))
			})
		})

		Context("with leader election enabled", func() {
			BeforeEach(func() {
				reconciler.leaderElectionEnabled = true
				// Use testReconciler for leadership tests
				testReconciler.SBDRemediationReconciler = reconciler
			})

			It("should wait for leadership before fencing", func() {
				By("Creating a SBDRemediation resource")
				resource := &medik8sv1alpha1.SBDRemediation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: medik8sv1alpha1.SBDRemediationSpec{
						NodeName: "worker-7",
						Reason:   medik8sv1alpha1.SBDRemediationReasonHeartbeatTimeout,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				By("Reconciling without leadership")
				// Set testReconciler to return false for IsLeader
				testReconciler.isLeaderFunc = func() bool { return false }

				result, err := testReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10 * time.Second))

				By("Verifying the resource is waiting for leadership")
				updatedResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())

				Expect(updatedResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseWaitingForLeadership))

				By("Reconciling with leadership")
				// Set testReconciler to return true for IsLeader
				testReconciler.isLeaderFunc = func() bool { return true }

				result, err = testReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Requeue).To(BeFalse())

				By("Verifying the resource was successfully fenced")
				Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())
				Expect(updatedResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully))
			})
		})

		Context("when resource is deleted", func() {
			It("should clean up properly", func() {
				By("Creating and processing a SBDRemediation resource")
				resource := &medik8sv1alpha1.SBDRemediation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: medik8sv1alpha1.SBDRemediationSpec{
						NodeName: "worker-9",
						Reason:   medik8sv1alpha1.SBDRemediationReasonManualFencing,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				// Initial reconcile to add finalizer
				_, err := reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})
				Expect(err).NotTo(HaveOccurred())

				By("Deleting the resource")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

				By("Reconciling after deletion")
				_, err = reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying the resource is removed")
				deletedResource := &medik8sv1alpha1.SBDRemediation{}
				err = k8sClient.Get(ctx, namespacedName, deletedResource)
				Expect(errors.IsNotFound(err)).To(BeTrue())
			})
		})

		Context("when resource already exists with different phases", func() {
			It("should not reprocess completed fencing", func() {
				By("Creating a SBDRemediation resource")
				resource := &medik8sv1alpha1.SBDRemediation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: medik8sv1alpha1.SBDRemediationSpec{
						NodeName: "worker-11",
						Reason:   medik8sv1alpha1.SBDRemediationReasonHeartbeatTimeout,
					},
					Status: medik8sv1alpha1.SBDRemediationStatus{
						Phase: medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				By("Reconciling the already completed resource")
				result, err := reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Requeue).To(BeFalse())

				By("Verifying the resource status remains unchanged")
				updatedResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())
				Expect(updatedResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully))
			})
		})
	})
})
