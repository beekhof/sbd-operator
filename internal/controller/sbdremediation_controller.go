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
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
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

	// Retry configuration
	maxRetryAttempts = 3
	baseRetryDelay   = 5 * time.Second
	maxRetryDelay    = 30 * time.Second

	// Status update retry configuration
	maxStatusUpdateRetries = 5
	statusUpdateDelay      = 100 * time.Millisecond

	// Event reasons for SBDRemediation operations
	ReasonFencingInitiated     = "FencingInitiated"
	ReasonNodeFenced           = "NodeFenced"
	ReasonFencingFailed        = "FencingFailed"
	ReasonSBDDeviceError       = "SBDDeviceError"
	ReasonNodeIDMappingError   = "NodeIDMappingError"
	ReasonLeadershipWaiting    = "LeadershipWaiting"
	ReasonRemediationCompleted = "RemediationCompleted"
	ReasonRemediationFailed    = "RemediationFailed"
	ReasonRemediationInitiated = "RemediationInitiated"
	ReasonFinalizerProcessed   = "FinalizerProcessed"
)

// SBDRemediationReconciler reconciles a SBDRemediation object
type SBDRemediationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Leadership tracking - simple approach using environment variable or config
	leaderElectionEnabled bool

	// SBD device configuration
	sbdDevicePath string
	sbdDevice     *blockdevice.Device
}

// emitEvent is a helper function to emit Kubernetes events for the SBDRemediation controller
func (r *SBDRemediationReconciler) emitEvent(object client.Object, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(object, eventType, reason, message)
	}
}

// emitEventf is a helper function to emit formatted Kubernetes events for the SBDRemediation controller
func (r *SBDRemediationReconciler) emitEventf(object client.Object, eventType, reason, messageFmt string, args ...interface{}) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, eventType, reason, messageFmt, args...)
	}
}

// FencingError represents an error that occurred during the fencing process
type FencingError struct {
	Operation  string
	Underlying error
	Retryable  bool
	NodeName   string
	NodeID     uint16
}

func (e *FencingError) Error() string {
	retryableStr := "non-retryable"
	if e.Retryable {
		retryableStr = "retryable"
	}
	return fmt.Sprintf("fencing error during %s for node %s (ID: %d): %v (%s)",
		e.Operation, e.NodeName, e.NodeID, e.Underlying, retryableStr)
}

// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdremediations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdremediations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdremediations/finalizers,verbs=update
// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;create;update;patch;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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
		if apierrors.IsNotFound(err) {
			// SBDRemediation resource not found, probably deleted
			logger.Info("SBDRemediation resource not found, probably deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get SBDRemediation")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !sbdRemediation.DeletionTimestamp.IsZero() {
		logger.Info("SBDRemediation is being deleted, processing finalizers")
		r.emitEventf(&sbdRemediation, EventTypeNormal, ReasonFinalizerProcessed,
			"Processing deletion of SBDRemediation for node '%s'", sbdRemediation.Spec.NodeName)
		return r.handleDeletion(ctx, &sbdRemediation)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&sbdRemediation, SBDRemediationFinalizer) {
		controllerutil.AddFinalizer(&sbdRemediation, SBDRemediationFinalizer)
		if err := r.Update(ctx, &sbdRemediation); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		logger.Info("Added finalizer to SBDRemediation")
	}

	// Emit initial event for remediation initiation
	if sbdRemediation.Status.Phase == "" {
		r.emitEventf(&sbdRemediation, EventTypeNormal, ReasonRemediationInitiated,
			"SBD remediation initiated for node '%s'", sbdRemediation.Spec.NodeName)
	}

	// Check if we already completed this remediation
	if sbdRemediation.Status.Phase == medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully {
		logger.Info("SBDRemediation already completed successfully")
		return ctrl.Result{}, nil
	}

	// Leadership check - only the leader should perform fencing operations
	if r.leaderElectionEnabled && !r.IsLeader() {
		logger.Info("⏳ Not the leader - waiting for leadership to perform fencing operations")
		r.emitEventf(&sbdRemediation, EventTypeNormal, ReasonLeadershipWaiting,
			"Waiting for leadership to perform fencing for node '%s'", sbdRemediation.Spec.NodeName)
		return r.updateStatusRobust(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseWaitingForLeadership,
			"Waiting for operator leadership to perform fencing operations")
	}

	logger.Info("🎯 Leader confirmed - proceeding with fencing operations", "nodeName", sbdRemediation.Spec.NodeName)

	// Convert node name to node ID
	targetNodeID, err := r.nodeNameToNodeID(sbdRemediation.Spec.NodeName)
	if err != nil {
		logger.Error(err, "Failed to map node name to node ID", "nodeName", sbdRemediation.Spec.NodeName)
		r.emitEventf(&sbdRemediation, EventTypeWarning, ReasonNodeIDMappingError,
			"Failed to map node name '%s' to node ID: %v", sbdRemediation.Spec.NodeName, err)
		return r.updateStatusRobust(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseFailed,
			fmt.Sprintf("Failed to map node name to node ID: %v", err))
	}

	// Initialize SBD device if needed
	if r.sbdDevice == nil {
		if err := r.initializeSBDDevice(ctx); err != nil {
			logger.Error(err, "Failed to initialize SBD device")
			r.emitEventf(&sbdRemediation, EventTypeWarning, ReasonSBDDeviceError,
				"Failed to initialize SBD device for fencing node '%s': %v", sbdRemediation.Spec.NodeName, err)
			return r.updateStatusRobust(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseFailed,
				fmt.Sprintf("Failed to initialize SBD device: %v", err))
		}
	}

	// Update status to indicate fencing is in progress
	if sbdRemediation.Status.Phase != medik8sv1alpha1.SBDRemediationPhaseFencingInProgress {
		// Set NodeID in status before updating
		sbdRemediation.Status.NodeID = &targetNodeID
		sbdRemediation.Status.OperatorInstance = r.getOperatorInstanceID()

		if result, err := r.updateStatusRobust(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseFencingInProgress,
			fmt.Sprintf("Initiating fencing for node %s (ID: %d)", sbdRemediation.Spec.NodeName, targetNodeID)); err != nil {
			return result, err
		}

		// Emit event for fencing initiation
		r.emitEventf(&sbdRemediation, EventTypeNormal, ReasonFencingInitiated,
			"Fencing initiated for node '%s' via SBD", sbdRemediation.Spec.NodeName)
	}

	// Perform the fencing operation with retry logic
	if err := r.performFencingWithRetry(ctx, &sbdRemediation, targetNodeID); err != nil {
		logger.Error(err, "Failed to fence node", "nodeName", sbdRemediation.Spec.NodeName, "nodeID", targetNodeID)

		// Emit failure event
		r.emitEventf(&sbdRemediation, EventTypeWarning, ReasonFencingFailed,
			"Failed to fence node '%s' via SBD: %s", sbdRemediation.Spec.NodeName, err.Error())

		return r.updateStatusRobust(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseFailed,
			fmt.Sprintf("Fencing failed: %v", err))
	}

	logger.Info("✅ Successfully fenced node", "nodeName", sbdRemediation.Spec.NodeName, "nodeID", targetNodeID)

	// Emit success event
	r.emitEventf(&sbdRemediation, EventTypeNormal, ReasonNodeFenced,
		"Node '%s' successfully fenced via SBD", sbdRemediation.Spec.NodeName)

	// Update status to indicate successful fencing
	return r.updateStatusRobust(ctx, &sbdRemediation, medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully,
		fmt.Sprintf("Node %s (ID: %d) successfully fenced via SBD device", sbdRemediation.Spec.NodeName, targetNodeID))
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
		return &FencingError{
			Operation:  "SBD device initialization",
			Underlying: fmt.Errorf("failed to open SBD device %s: %w", r.sbdDevicePath, err),
			Retryable:  true, // Device issues might be temporary
		}
	}

	r.sbdDevice = device
	return nil
}

// performFencingWithRetry performs the fencing operation with retry logic for transient errors
func (r *SBDRemediationReconciler) performFencingWithRetry(ctx context.Context, sbdRemediation *medik8sv1alpha1.SBDRemediation, targetNodeID uint16) error {
	logger := logf.FromContext(ctx)

	var lastErr error
	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		logger.Info("Attempting fencing operation",
			"attempt", attempt,
			"max-attempts", maxRetryAttempts,
			"target-node", sbdRemediation.Spec.NodeName)

		err := r.performFencing(ctx, sbdRemediation, targetNodeID)
		if err == nil {
			// Success!
			return nil
		}

		lastErr = err

		// Check if this is a retryable error
		var fencingErr *FencingError
		if errors.As(err, &fencingErr) && !fencingErr.Retryable {
			logger.Error(err, "Non-retryable fencing error encountered")
			return err // Don't retry non-retryable errors
		}

		// If this is the last attempt, don't wait
		if attempt == maxRetryAttempts {
			break
		}

		// Calculate exponential backoff delay
		delay := time.Duration(attempt) * baseRetryDelay
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}

		logger.Info("Fencing attempt failed, retrying",
			"attempt", attempt,
			"error", err,
			"retry-delay", delay)

		// Wait before retrying
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			// Continue to next attempt
		}
	}

	// All attempts failed
	return fmt.Errorf("fencing failed after %d attempts, last error: %w", maxRetryAttempts, lastErr)
}

// performFencing performs the actual fencing operation by writing a fence message to the SBD device
func (r *SBDRemediationReconciler) performFencing(ctx context.Context, sbdRemediation *medik8sv1alpha1.SBDRemediation, targetNodeID uint16) error {
	logger := logf.FromContext(ctx)

	// Initialize SBD device if needed
	if err := r.initializeSBDDevice(ctx); err != nil {
		return err
	}

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
		return &FencingError{
			Operation:  "fence message marshaling",
			Underlying: fmt.Errorf("failed to marshal fence message: %w", err),
			Retryable:  false, // Marshaling errors are usually permanent
			NodeName:   sbdRemediation.Spec.NodeName,
			NodeID:     targetNodeID,
		}
	}

	// Calculate target slot offset
	slotOffset := int64(targetNodeID) * sbdprotocol.SBD_SLOT_SIZE

	// Write fence message to target node's slot
	if _, err := r.sbdDevice.WriteAt(messageBytes, slotOffset); err != nil {
		return &FencingError{
			Operation:  "SBD device write",
			Underlying: fmt.Errorf("failed to write fence message to SBD device at offset %d: %w", slotOffset, err),
			Retryable:  true, // Write errors might be temporary (device busy, I/O errors)
			NodeName:   sbdRemediation.Spec.NodeName,
			NodeID:     targetNodeID,
		}
	}

	// Ensure data is synced to disk
	if err := r.sbdDevice.Sync(); err != nil {
		return &FencingError{
			Operation:  "SBD device sync",
			Underlying: fmt.Errorf("failed to sync fence message to SBD device: %w", err),
			Retryable:  true, // Sync errors might be temporary
			NodeName:   sbdRemediation.Spec.NodeName,
			NodeID:     targetNodeID,
		}
	}

	// Verify the write by reading back the message (optional verification)
	if err := r.verifyFenceMessage(messageBytes, slotOffset); err != nil {
		logger.Error(err, "Fence message verification failed, but write was successful")
		// Don't fail the operation for verification errors, just log them
	}

	logger.Info("✅ Fence message successfully written to SBD device",
		"target-node-name", sbdRemediation.Spec.NodeName,
		"target-node-id", targetNodeID,
		"slot-offset", slotOffset,
		"message-size", len(messageBytes))

	return nil
}

// verifyFenceMessage verifies that the fence message was written correctly by reading it back
func (r *SBDRemediationReconciler) verifyFenceMessage(expectedBytes []byte, slotOffset int64) error {
	readBuffer := make([]byte, len(expectedBytes))
	if _, err := r.sbdDevice.ReadAt(readBuffer, slotOffset); err != nil {
		return fmt.Errorf("failed to read back fence message for verification: %w", err)
	}

	// Compare the written and read data
	for i, b := range expectedBytes {
		if i < len(readBuffer) && readBuffer[i] != b {
			return fmt.Errorf("fence message verification failed: byte mismatch at position %d (expected %d, got %d)", i, b, readBuffer[i])
		}
	}

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

// updateStatusRobust updates the status of the SBDRemediation resource with robust error handling and retry logic
func (r *SBDRemediationReconciler) updateStatusRobust(ctx context.Context, sbdRemediation *medik8sv1alpha1.SBDRemediation,
	phase medik8sv1alpha1.SBDRemediationPhase, message string) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// Check if status actually needs updating (idempotent)
	if sbdRemediation.Status.Phase == phase && sbdRemediation.Status.Message == message {
		logger.V(1).Info("Status already up to date, skipping update", "phase", phase)
		return ctrl.Result{}, nil
	}

	// Update status fields
	sbdRemediation.Status.Phase = phase
	sbdRemediation.Status.Message = message
	now := metav1.Now()
	sbdRemediation.Status.LastUpdateTime = &now

	// Set operator instance if not already set
	if sbdRemediation.Status.OperatorInstance == "" {
		sbdRemediation.Status.OperatorInstance = r.getOperatorInstanceID()
	}

	// Update fence message written flag if we're marking as successfully fenced
	if phase == medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully {
		sbdRemediation.Status.FenceMessageWritten = true
	}

	// Update the status with retry logic
	if err := r.updateStatusWithRetry(ctx, sbdRemediation); err != nil {
		logger.Error(err, "Failed to update SBDRemediation status", "phase", phase, "message", message)
		return ctrl.Result{RequeueAfter: baseRetryDelay}, err
	}

	logger.Info("Status updated successfully", "phase", phase, "message", message)

	// Determine requeue behavior based on phase
	switch phase {
	case medik8sv1alpha1.SBDRemediationPhaseWaitingForLeadership:
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	case medik8sv1alpha1.SBDRemediationPhaseFailed:
		// For failed status, we don't requeue automatically - manual intervention may be needed
		return ctrl.Result{}, nil
	case medik8sv1alpha1.SBDRemediationPhaseFencedSuccessfully:
		// Success - no requeue needed
		return ctrl.Result{}, nil
	default:
		// For other phases, requeue for continued processing
		return ctrl.Result{Requeue: true}, nil
	}
}

// updateStatusWithRetry updates the status with retry logic to handle conflicts
func (r *SBDRemediationReconciler) updateStatusWithRetry(ctx context.Context, sbdRemediation *medik8sv1alpha1.SBDRemediation) error {
	logger := logf.FromContext(ctx)

	return wait.ExponentialBackoff(wait.Backoff{
		Duration: statusUpdateDelay,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    maxStatusUpdateRetries,
		Cap:      time.Second,
	}, func() (bool, error) {
		// Get the latest version to avoid conflicts
		latest := &medik8sv1alpha1.SBDRemediation{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sbdRemediation), latest); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("SBDRemediation was deleted during status update")
				return true, nil // Stop retrying
			}
			logger.Error(err, "Failed to get latest SBDRemediation for status update")
			return false, err // Retry
		}

		// Copy our status changes to the latest version
		latest.Status = sbdRemediation.Status

		// Attempt the status update
		if err := r.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) {
				logger.V(1).Info("Conflict during status update, retrying")
				// Update our in-memory copy for the next retry
				*sbdRemediation = *latest
				return false, nil // Retry
			}

			// For other errors, decide whether to retry
			if apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) {
				logger.V(1).Info("Temporary error during status update, retrying", "error", err)
				return false, nil // Retry
			}

			// Permanent error
			logger.Error(err, "Permanent error during status update")
			return false, err
		}

		// Success!
		logger.V(1).Info("Status update successful")
		return true, nil
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *SBDRemediationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&medik8sv1alpha1.SBDRemediation{}).
		Named("sbdremediation").
		Complete(r)
}
