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
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	medik8sv1alpha1 "github.com/medik8s/sbd-operator/api/v1alpha1"
	"github.com/medik8s/sbd-operator/pkg/blockdevice"
	"github.com/medik8s/sbd-operator/pkg/sbdprotocol"
)

const (
	SBDRemediationFinalizer = "medik8s.io/sbd-remediation-finalizer"
	// DefaultSBDDevicePath is the default path where the SBD device is mounted in the operator pod
	DefaultSBDDevicePath = "/mnt/sbd-operator-device"
	// OperatorNodeID is the node ID used by the operator when writing fence messages
	OperatorNodeID = uint16(255)
	// ReasonCompleted indicates the remediation was completed successfully
	ReasonCompleted = "RemediationCompleted"
	// ReasonInProgress indicates the remediation is in progress
	ReasonInProgress = "RemediationInProgress"
	// ReasonFailed indicates the remediation failed
	ReasonFailed = "RemediationFailed"
	// ReasonWaitingForLeadership indicates waiting for leadership to perform fencing
	ReasonWaitingForLeadership = "WaitingForLeadership"
)

// SBDRemediationReconciler reconciles a SBDRemediation object
type SBDRemediationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Leadership tracking - simple approach using environment variable or config
	leaderElectionEnabled bool

	// SBD device configuration
	sbdDevicePath string
	sbdDevice     *blockdevice.Device
}

// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdremediations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdremediations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdremediations/finalizers,verbs=update
// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;create;update;patch;watch

// SetLeaderElectionEnabled sets whether leader election is enabled for this controller
func (r *SBDRemediationReconciler) SetLeaderElectionEnabled(enabled bool) {
	r.leaderElectionEnabled = enabled
}

// OnBecomeLeader is called when this operator instance becomes the leader
func (r *SBDRemediationReconciler) OnBecomeLeader() {
	// This method is kept for compatibility and future use
	// Leadership checking is now done through the reconciliation loop
}

// OnLoseLeadership is called when this operator instance loses leadership
func (r *SBDRemediationReconciler) OnLoseLeadership() {
	// This method is kept for compatibility and future use
	// Leadership checking is now done through the reconciliation loop
}

// IsLeader returns whether this operator instance is currently the leader
// For simplicity, we'll use a basic approach - in production, this would
// check the actual leader election lease status
func (r *SBDRemediationReconciler) IsLeader() bool {
	if !r.leaderElectionEnabled {
		// If leader election is disabled, we're always the leader
		return true
	}

	// TODO: In a production implementation, this would check the actual
	// leader election lease status. For now, we'll use a simple placeholder
	// that assumes we're the leader (since we're participating in leader election)
	return true
}

// nodeNameToNodeID converts a Kubernetes node name to a numeric node ID for SBD operations
// This implements a simple mapping strategy: extract the numeric suffix from node names like "node-1", "worker-2", etc.
// In production, this would likely come from SBDConfig or a more sophisticated mapping mechanism
func (r *SBDRemediationReconciler) nodeNameToNodeID(nodeName string) (uint16, error) {
	// Try to extract numeric suffix from node names like "node-1", "worker-2", "k8s-node-3", etc.
	parts := strings.Split(nodeName, "-")
	if len(parts) < 2 {
		return 0, fmt.Errorf("unable to determine node ID from node name %q: expected format like 'node-1' or 'worker-2'", nodeName)
	}

	// Try the last part first (most common case)
	lastPart := parts[len(parts)-1]
	if nodeID, err := strconv.ParseUint(lastPart, 10, 16); err == nil && nodeID > 0 && nodeID < 255 {
		return uint16(nodeID), nil
	}

	// If that fails, try other parts
	for i := len(parts) - 2; i >= 0; i-- {
		if nodeID, err := strconv.ParseUint(parts[i], 10, 16); err == nil && nodeID > 0 && nodeID < 255 {
			return uint16(nodeID), nil
		}
	}

	return 0, fmt.Errorf("unable to extract valid node ID from node name %q: no numeric part found in range 1-254", nodeName)
}

// getOperatorInstanceID returns a unique identifier for this operator instance
func (r *SBDRemediationReconciler) getOperatorInstanceID() string {
	// Use pod name if available, otherwise hostname
	if podName := os.Getenv("POD_NAME"); podName != "" {
		return podName
	}
	if hostname, err := os.Hostname(); err == nil {
		return hostname
	}
	return "unknown-operator-instance"
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// The SBDRemediation controller is responsible for:
// 1. Validating that this operator instance is the leader before performing fencing
// 2. Writing fence messages to the shared SBD device for target nodes
// 3. Monitoring the status of fencing operations
// 4. Updating the SBDRemediation status to reflect the current state
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *SBDRemediationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("sbdremediation", req.NamespacedName)

	// Fetch the SBDRemediation instance
	var sbdRemediation medik8sv1alpha1.SBDRemediation
	if err := r.Get(ctx, req.NamespacedName, &sbdRemediation); err != nil {
		if errors.IsNotFound(err) {
			// SBDRemediation resource not found, probably deleted
			logger.Info("SBDRemediation resource not found, probably deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get SBDRemediation")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !sbdRemediation.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &sbdRemediation)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&sbdRemediation, SBDRemediationFinalizer) {
		controllerutil.AddFinalizer(&sbdRemediation, SBDRemediationFinalizer)
		if err := r.Update(ctx, &sbdRemediation); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize status if empty
	if sbdRemediation.Status.Phase == "" {
		return r.updateStatus(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhasePending,
			"SBD remediation request received and queued for processing")
	}

	/*
	 * LEADERSHIP CHECK - CRITICAL FOR FENCING SAFETY
	 *
	 * Fencing operations must only be performed by the leader operator instance
	 * to prevent race conditions, conflicting decisions, and potential data corruption.
	 * Non-leader instances will wait and requeue until they become leader or another
	 * leader processes the remediation.
	 */
	if r.leaderElectionEnabled && !r.IsLeader() {
		logger.Info("⏳ Not the leader - waiting for leadership to perform fencing operations")
		return r.updateStatus(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseWaitingForLeadership,
			"Waiting for operator instance to become leader before performing fencing operations")
	}

	// Check if remediation is already completed or failed
	switch sbdRemediation.Status.Phase {
	case medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully:
		logger.Info("✅ SBDRemediation already completed successfully")
		return ctrl.Result{}, nil
	case medik8sv1alpha1.SBDRemediationPhaseFailed:
		logger.Info("❌ SBDRemediation previously failed")
		return ctrl.Result{}, nil
	}

	logger.Info("🎯 Processing SBDRemediation as leader",
		"target-node", sbdRemediation.Spec.NodeName,
		"reason", sbdRemediation.Spec.Reason)

	// Determine target node ID
	targetNodeID, err := r.nodeNameToNodeID(sbdRemediation.Spec.NodeName)
	if err != nil {
		logger.Error(err, "Failed to determine node ID for target node", "node-name", sbdRemediation.Spec.NodeName)
		return r.updateStatus(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseFailed,
			fmt.Sprintf("Failed to determine node ID for node %q: %v", sbdRemediation.Spec.NodeName, err))
	}

	// Update status with node ID if not already set
	if sbdRemediation.Status.NodeID == nil || *sbdRemediation.Status.NodeID != targetNodeID {
		sbdRemediation.Status.NodeID = &targetNodeID
		sbdRemediation.Status.OperatorInstance = r.getOperatorInstanceID()
		if err := r.Status().Update(ctx, &sbdRemediation); err != nil {
			logger.Error(err, "Failed to update status with node ID")
			return ctrl.Result{}, err
		}
	}

	// Initialize SBD device if not already done
	if err := r.initializeSBDDevice(ctx); err != nil {
		logger.Error(err, "Failed to initialize SBD device")
		return r.updateStatus(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseFailed,
			fmt.Sprintf("Failed to initialize SBD device: %v", err))
	}

	// Update status to indicate fencing is in progress
	if sbdRemediation.Status.Phase != medik8sv1alpha1.SBDRemediationPhaseFencingInProgress {
		if _, err := r.updateStatus(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseFencingInProgress,
			fmt.Sprintf("Writing fence message for node %s (NodeID: %d) to SBD device", sbdRemediation.Spec.NodeName, targetNodeID)); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Perform the fencing operation
	if err := r.performFencing(ctx, &sbdRemediation, targetNodeID); err != nil {
		logger.Error(err, "Failed to perform fencing operation")
		return r.updateStatus(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseFailed,
			fmt.Sprintf("Fencing operation failed: %v", err))
	}

	// Mark as completed
	_, err = r.updateStatus(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully,
		fmt.Sprintf("Successfully fenced node %s (NodeID: %d) - fence message written to SBD device",
			sbdRemediation.Spec.NodeName, targetNodeID))

	logger.Info("✅ SBDRemediation completed successfully",
		"target-node", sbdRemediation.Spec.NodeName,
		"node-id", targetNodeID)
	return ctrl.Result{}, err
}

// initializeSBDDevice initializes the SBD device connection if not already done
func (r *SBDRemediationReconciler) initializeSBDDevice(ctx context.Context) error {
	if r.sbdDevice != nil {
		return nil // Already initialized
	}

	// Determine SBD device path
	if r.sbdDevicePath == "" {
		// Check environment variable first
		if envPath := os.Getenv("SBD_DEVICE_PATH"); envPath != "" {
			r.sbdDevicePath = envPath
		} else {
			r.sbdDevicePath = DefaultSBDDevicePath
		}
	}

	device, err := blockdevice.Open(r.sbdDevicePath)
	if err != nil {
		return fmt.Errorf("failed to open SBD device %s: %w", r.sbdDevicePath, err)
	}

	r.sbdDevice = device
	return nil
}

// performFencing performs the actual fencing operation by writing a fence message to the SBD device
func (r *SBDRemediationReconciler) performFencing(ctx context.Context, sbdRemediation *medik8sv1alpha1.SBDRemediation, targetNodeID uint16) error {
	logger := logf.FromContext(ctx)

	// Convert reason to numeric value
	var reasonCode uint8 = 1 // Default to generic fencing reason
	switch sbdRemediation.Spec.Reason {
	case medik8sv1alpha1.SBDRemediationReasonHeartbeatTimeout:
		reasonCode = 2
	case medik8sv1alpha1.SBDRemediationReasonNodeUnresponsive:
		reasonCode = 3
	case medik8sv1alpha1.SBDRemediationReasonManualFencing:
		reasonCode = 4
	}

	senderNodeID := OperatorNodeID
	sequence := uint64(time.Now().Unix())

	logger.Info("🔥 Writing fence message to SBD device",
		"target-node-name", sbdRemediation.Spec.NodeName,
		"target-node-id", targetNodeID,
		"sender-node-id", senderNodeID,
		"sequence", sequence,
		"reason", sbdRemediation.Spec.Reason,
		"reason-code", reasonCode)

	// Create fence message
	header := sbdprotocol.NewFence(senderNodeID, targetNodeID, sequence, reasonCode)
	fenceMessage := sbdprotocol.SBDFenceMessage{
		Header:       header,
		TargetNodeID: targetNodeID,
		Reason:       reasonCode,
	}

	// Marshal the fence message
	messageBytes, err := sbdprotocol.MarshalFence(fenceMessage)
	if err != nil {
		return fmt.Errorf("failed to marshal fence message: %w", err)
	}

	// Calculate target slot offset
	slotOffset := int64(targetNodeID) * sbdprotocol.SBD_SLOT_SIZE

	// Write fence message to target node's slot
	if _, err := r.sbdDevice.WriteAt(messageBytes, slotOffset); err != nil {
		return fmt.Errorf("failed to write fence message to SBD device at offset %d: %w", slotOffset, err)
	}

	// Ensure data is synced to disk
	if err := r.sbdDevice.Sync(); err != nil {
		return fmt.Errorf("failed to sync fence message to SBD device: %w", err)
	}

	logger.Info("✅ Fence message successfully written to SBD device",
		"target-node-name", sbdRemediation.Spec.NodeName,
		"target-node-id", targetNodeID,
		"slot-offset", slotOffset,
		"message-size", len(messageBytes))

	// Update the fence message written flag
	sbdRemediation.Status.FenceMessageWritten = true

	return nil
}

// handleDeletion handles the deletion of SBDRemediation resources
func (r *SBDRemediationReconciler) handleDeletion(ctx context.Context, sbdRemediation *medik8sv1alpha1.SBDRemediation) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// Perform any cleanup if needed
	// Note: SBD fence messages are usually persistent and we don't clear them on deletion
	logger.Info("🗑️  Cleaning up SBDRemediation resource")

	// Remove finalizer
	controllerutil.RemoveFinalizer(sbdRemediation, SBDRemediationFinalizer)
	if err := r.Update(ctx, sbdRemediation); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// updateStatus updates the status of the SBDRemediation resource
func (r *SBDRemediationReconciler) updateStatus(ctx context.Context, sbdRemediation *medik8sv1alpha1.SBDRemediation,
	phase medik8sv1alpha1.SBDRemediationPhase, message string) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// Update status fields
	sbdRemediation.Status.Phase = phase
	sbdRemediation.Status.Message = message
	now := metav1.Now()
	sbdRemediation.Status.LastUpdateTime = &now

	// Set operator instance if not already set
	if sbdRemediation.Status.OperatorInstance == "" {
		sbdRemediation.Status.OperatorInstance = r.getOperatorInstanceID()
	}

	// Update the status
	if err := r.Status().Update(ctx, sbdRemediation); err != nil {
		logger.Error(err, "Failed to update SBDRemediation status", "phase", phase, "message", message)
		return ctrl.Result{}, err
	}

	logger.Info("Status updated", "phase", phase, "message", message)

	// Requeue with delay if waiting for leadership
	if phase == medik8sv1alpha1.SBDRemediationPhaseWaitingForLeadership {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Requeue with delay if failed (for retry logic)
	if phase == medik8sv1alpha1.SBDRemediationPhaseFailed {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SBDRemediationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&medik8sv1alpha1.SBDRemediation{}).
		Named("sbdremediation").
		Complete(r)
}
