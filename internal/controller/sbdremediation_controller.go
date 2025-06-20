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
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
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

	// SBD device configuration - these would typically come from SBDConfig
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
		r.updateStatus(ctx, &sbdRemediation, "Waiting", ReasonWaitingForLeadership,
			"Waiting for operator instance to become leader before performing fencing operations")
		// Requeue after a short delay to check leadership status
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	logger.Info("🎯 Processing SBDRemediation as leader", "target", sbdRemediation.Spec.Foo)

	// Initialize SBD device if not already done
	if err := r.initializeSBDDevice(ctx); err != nil {
		logger.Error(err, "Failed to initialize SBD device")
		r.updateStatus(ctx, &sbdRemediation, "Failed", ReasonFailed,
			fmt.Sprintf("Failed to initialize SBD device: %v", err))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Perform the fencing operation
	if err := r.performFencing(ctx, &sbdRemediation); err != nil {
		logger.Error(err, "Failed to perform fencing operation")
		r.updateStatus(ctx, &sbdRemediation, "Failed", ReasonFailed,
			fmt.Sprintf("Fencing operation failed: %v", err))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Mark as completed
	r.updateStatus(ctx, &sbdRemediation, "Completed", ReasonCompleted,
		"Fencing operation completed successfully")

	logger.Info("✅ SBDRemediation completed successfully")
	return ctrl.Result{}, nil
}

// initializeSBDDevice initializes the SBD device connection if not already done
func (r *SBDRemediationReconciler) initializeSBDDevice(ctx context.Context) error {
	if r.sbdDevice != nil {
		return nil // Already initialized
	}

	// Get SBD configuration from SBDConfig resource
	// For now, we'll use a placeholder - in a real implementation,
	// this would fetch the actual SBDConfig resource
	if r.sbdDevicePath == "" {
		// This would typically come from environment variables or SBDConfig
		r.sbdDevicePath = "/dev/sbd-shared" // Placeholder
	}

	device, err := blockdevice.Open(r.sbdDevicePath)
	if err != nil {
		return fmt.Errorf("failed to open SBD device %s: %w", r.sbdDevicePath, err)
	}

	r.sbdDevice = device
	return nil
}

// performFencing performs the actual fencing operation by writing a fence message to the SBD device
func (r *SBDRemediationReconciler) performFencing(ctx context.Context, sbdRemediation *medik8sv1alpha1.SBDRemediation) error {
	logger := logf.FromContext(ctx)

	// TODO: Parse target node information from SBDRemediation spec
	// For now, using placeholder values
	targetNodeID := uint16(1)   // This would come from spec
	senderNodeID := uint16(255) // Operator node ID
	sequence := uint64(time.Now().Unix())
	reason := uint8(1) // Generic fencing reason

	logger.Info("🔥 Writing fence message to SBD device",
		"target-node-id", targetNodeID,
		"sender-node-id", senderNodeID,
		"sequence", sequence)

	// Create fence message
	header := sbdprotocol.NewFence(senderNodeID, targetNodeID, sequence, reason)
	fenceMessage := sbdprotocol.SBDFenceMessage{
		Header:       header,
		TargetNodeID: targetNodeID,
		Reason:       reason,
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

	logger.Info("✅ Fence message successfully written to SBD device",
		"target-node-id", targetNodeID,
		"slot-offset", slotOffset)

	return nil
}

// handleDeletion handles the deletion of SBDRemediation resources
func (r *SBDRemediationReconciler) handleDeletion(ctx context.Context, sbdRemediation *medik8sv1alpha1.SBDRemediation) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// Perform any cleanup if needed
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
	phase, reason, message string) {
	logger := logf.FromContext(ctx)

	// TODO: Update actual status fields when they're defined in the API
	// For now, just log the status change
	logger.Info("Status update", "phase", phase, "reason", reason, "message", message)

	// In a real implementation, this would update sbdRemediation.Status fields
	// and call r.Status().Update(ctx, sbdRemediation)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SBDRemediationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&medik8sv1alpha1.SBDRemediation{}).
		Named("sbdremediation").
		Complete(r)
}
