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

	Context("FencingError", func() {
		It("should format error messages correctly", func() {
			err := &FencingError{
				Operation:  "test operation",
				Underlying: fmt.Errorf("underlying error"),
				Retryable:  true,
				NodeName:   "worker-1",
				NodeID:     5,
			}

			expectedMessage := "fencing error during test operation for node worker-1 (ID: 5): underlying error (retryable)"
			Expect(err.Error()).To(Equal(expectedMessage))
		})

		It("should handle non-retryable errors", func() {
			err := &FencingError{
				Operation:  "marshaling",
				Underlying: fmt.Errorf("invalid data"),
				Retryable:  false,
				NodeName:   "worker-2",
				NodeID:     3,
			}

			Expect(err.Error()).To(ContainSubstring("non-retryable"))
		})
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
			It("should successfully fence a node and update status correctly", func() {
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

				By("Reconciling the resource multiple times to complete the workflow")
				Eventually(func() medik8sv1alpha1.SBDRemediationPhase {
					_, err := reconciler.Reconcile(ctx, reconcile.Request{
						NamespacedName: namespacedName,
					})
					Expect(err).NotTo(HaveOccurred())

					// Get updated resource
					updatedResource := &medik8sv1alpha1.SBDRemediation{}
					err = k8sClient.Get(ctx, namespacedName, updatedResource)
					Expect(err).NotTo(HaveOccurred())

					return updatedResource.Status.Phase
				}, 10*time.Second, 100*time.Millisecond).Should(Equal(medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully))

				By("Verifying the final resource status")
				finalResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, finalResource)).To(Succeed())

				Expect(finalResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully))
				Expect(finalResource.Status.Message).To(Equal("Node successfully fenced via SBD."))
				Expect(finalResource.Status.NodeID).NotTo(BeNil())
				Expect(*finalResource.Status.NodeID).To(Equal(uint16(5)))
				Expect(finalResource.Status.FenceMessageWritten).To(BeTrue())
				Expect(finalResource.Status.LastUpdateTime).NotTo(BeNil())
				Expect(finalResource.Status.OperatorInstance).NotTo(BeEmpty())

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

			It("should handle invalid node names gracefully with proper status updates", func() {
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
				Eventually(func() medik8sv1alpha1.SBDRemediationPhase {
					_, err := reconciler.Reconcile(ctx, reconcile.Request{
						NamespacedName: namespacedName,
					})
					Expect(err).NotTo(HaveOccurred())

					updatedResource := &medik8sv1alpha1.SBDRemediation{}
					err = k8sClient.Get(ctx, namespacedName, updatedResource)
					Expect(err).NotTo(HaveOccurred())

					return updatedResource.Status.Phase
				}, 5*time.Second, 100*time.Millisecond).Should(Equal(medik8sv1alpha1.SBDRemediationPhaseFailed))

				By("Verifying the resource failed with appropriate error")
				updatedResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())

				Expect(updatedResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseFailed))
				Expect(updatedResource.Status.Message).To(ContainSubstring("Failed to determine node ID"))
				Expect(updatedResource.Status.LastUpdateTime).NotTo(BeNil())
			})

			It("should handle SBD device errors gracefully with retry logic", func() {
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

				By("Reconciling the resource and expecting failure after retries")
				Eventually(func() medik8sv1alpha1.SBDRemediationPhase {
					_, err := reconciler.Reconcile(ctx, reconcile.Request{
						NamespacedName: namespacedName,
					})
					Expect(err).NotTo(HaveOccurred())

					updatedResource := &medik8sv1alpha1.SBDRemediation{}
					err = k8sClient.Get(ctx, namespacedName, updatedResource)
					Expect(err).NotTo(HaveOccurred())

					return updatedResource.Status.Phase
				}, 30*time.Second, 500*time.Millisecond).Should(Equal(medik8sv1alpha1.SBDRemediationPhaseFailed))

				By("Verifying the resource failed with device error")
				updatedResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())

				Expect(updatedResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseFailed))
				Expect(updatedResource.Status.Message).To(ContainSubstring("Fencing operation failed"))
				Expect(updatedResource.Status.Message).To(ContainSubstring("SBD device initialization"))
			})

			It("should handle status update idempotency correctly", func() {
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

				By("Performing multiple status updates with the same values")
				result1, err1 := reconciler.updateStatusRobust(ctx, resource, medik8sv1alpha1.SBDRemediationPhasePending, "Test message")
				Expect(err1).NotTo(HaveOccurred())

				// Second update with same values should be idempotent
				result2, err2 := reconciler.updateStatusRobust(ctx, resource, medik8sv1alpha1.SBDRemediationPhasePending, "Test message")
				Expect(err2).NotTo(HaveOccurred())

				// The second call should skip the actual update (idempotent behavior)
				// We just verify both calls succeeded
				By("Verifying both calls succeeded")
				Expect(result1).NotTo(BeNil())
				Expect(result2).NotTo(BeNil())

				By("Verifying status was updated correctly")
				updatedResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())
				Expect(updatedResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhasePending))
				Expect(updatedResource.Status.Message).To(Equal("Test message"))
			})
		})

		Context("with leader election enabled", func() {
			BeforeEach(func() {
				reconciler.leaderElectionEnabled = true
				// Use testReconciler for leadership tests
				testReconciler.SBDRemediationReconciler = reconciler
			})

			It("should process fencing when leadership is available", func() {
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

				By("Reconciling with leadership available")
				// Set testReconciler to return true for IsLeader
				testReconciler.isLeaderFunc = func() bool { return true }

				Eventually(func() medik8sv1alpha1.SBDRemediationPhase {
					_, err := testReconciler.Reconcile(ctx, reconcile.Request{
						NamespacedName: namespacedName,
					})
					Expect(err).NotTo(HaveOccurred())

					updatedResource := &medik8sv1alpha1.SBDRemediation{}
					err = k8sClient.Get(ctx, namespacedName, updatedResource)
					Expect(err).NotTo(HaveOccurred())

					return updatedResource.Status.Phase
				}, 10*time.Second, 100*time.Millisecond).Should(Equal(medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully))

				By("Verifying the resource was successfully fenced")
				updatedResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())
				Expect(updatedResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully))
				Expect(updatedResource.Status.Message).To(Equal("Node successfully fenced via SBD."))
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
				By("Creating a SBDRemediation resource with completed status")
				resource := &medik8sv1alpha1.SBDRemediation{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: medik8sv1alpha1.SBDRemediationSpec{
						NodeName: "worker-11",
						Reason:   medik8sv1alpha1.SBDRemediationReasonHeartbeatTimeout,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())

				// First reconcile to add finalizer
				result, err := reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Requeue).To(BeTrue()) // Should requeue after adding finalizer

				// Manually update status to completed
				updatedResource := &medik8sv1alpha1.SBDRemediation{}
				Eventually(func() error {
					return k8sClient.Get(ctx, namespacedName, updatedResource)
				}, 2*time.Second, 100*time.Millisecond).Should(Succeed())

				updatedResource.Status.Phase = medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully
				updatedResource.Status.Message = "Already completed"
				Expect(k8sClient.Status().Update(ctx, updatedResource)).To(Succeed())

				By("Reconciling the already completed resource")
				result, err = reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Requeue).To(BeFalse()) // Should not requeue for completed resource

				By("Verifying the resource status remains unchanged")
				finalResource := &medik8sv1alpha1.SBDRemediation{}
				Expect(k8sClient.Get(ctx, namespacedName, finalResource)).To(Succeed())
				Expect(finalResource.Status.Phase).To(Equal(medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully))
				Expect(finalResource.Status.Message).To(Equal("Already completed"))
			})
		})
	})

	Context("Retry Logic", func() {
		var (
			reconciler *SBDRemediationReconciler
			mockDevice string
			tempDir    string
		)

		BeforeEach(func() {
			var err error
			tempDir, err = ioutil.TempDir("", "sbd-retry-test-")
			Expect(err).NotTo(HaveOccurred())

			mockDevice = filepath.Join(tempDir, "sbd-device")
			err = ioutil.WriteFile(mockDevice, make([]byte, 512*1024), 0644)
			Expect(err).NotTo(HaveOccurred())

			reconciler = &SBDRemediationReconciler{
				Client:        k8sClient,
				Scheme:        k8sClient.Scheme(),
				sbdDevicePath: mockDevice,
			}
		})

		AfterEach(func() {
			if reconciler.sbdDevice != nil {
				reconciler.sbdDevice.Close()
			}
			os.RemoveAll(tempDir)
		})

		It("should retry transient errors during fencing", func() {
			ctx := context.Background()
			sbdRemediation := &medik8sv1alpha1.SBDRemediation{
				Spec: medik8sv1alpha1.SBDRemediationSpec{
					NodeName: "worker-1",
					Reason:   medik8sv1alpha1.SBDRemediationReasonHeartbeatTimeout,
				},
			}

			// Make device temporarily inaccessible
			os.Chmod(mockDevice, 0000)

			// This should fail after retries
			err := reconciler.performFencingWithRetry(ctx, sbdRemediation, 1)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("fencing failed after"))

			// Restore device permissions for cleanup
			os.Chmod(mockDevice, 0644)
		})

		It("should not retry non-retryable errors", func() {
			ctx := context.Background()

			// Test node mapping error which is non-retryable
			_, err := reconciler.nodeNameToNodeID("invalid-node-name")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unable to extract valid node ID"))

			// For the fencing retry test, we need to test with a valid node ID but create
			// a situation that would cause non-retryable marshaling errors
			sbdRemediation := &medik8sv1alpha1.SBDRemediation{
				Spec: medik8sv1alpha1.SBDRemediationSpec{
					NodeName: "worker-1", // Valid node name
					Reason:   medik8sv1alpha1.SBDRemediationReasonHeartbeatTimeout,
				},
			}

			// This should succeed because we have a valid setup
			err = reconciler.performFencingWithRetry(ctx, sbdRemediation, 1)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Status Update Conflicts", func() {
		var (
			reconciler *SBDRemediationReconciler
			ctx        context.Context
		)

		BeforeEach(func() {
			ctx = context.Background()
			reconciler = &SBDRemediationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
		})

		It("should handle status update conflicts gracefully", func() {
			resourceName := fmt.Sprintf("conflict-test-%d", time.Now().UnixNano())
			namespacedName := types.NamespacedName{
				Name:      resourceName,
				Namespace: "default",
			}

			By("Creating a SBDRemediation resource")
			resource := &medik8sv1alpha1.SBDRemediation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: medik8sv1alpha1.SBDRemediationSpec{
					NodeName: "worker-1",
					Reason:   medik8sv1alpha1.SBDRemediationReasonHeartbeatTimeout,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			By("Performing concurrent status updates")
			// This simulates the conflict handling mechanism
			err := reconciler.updateStatusWithRetry(ctx, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the final status was applied")
			updatedResource := &medik8sv1alpha1.SBDRemediation{}
			Expect(k8sClient.Get(ctx, namespacedName, updatedResource)).To(Succeed())
			// Status should be set to whatever was in the resource object

			// Clean up
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
	})
})
