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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SBDRemediationPhase represents the current phase of the remediation process
type SBDRemediationPhase string

const (
	// SBDRemediationPhasePending indicates the remediation is waiting to be processed
	SBDRemediationPhasePending SBDRemediationPhase = "Pending"
	// SBDRemediationPhaseWaitingForLeadership indicates waiting for operator leadership
	SBDRemediationPhaseWaitingForLeadership SBDRemediationPhase = "WaitingForLeadership"
	// SBDRemediationPhaseFencingInProgress indicates the fencing operation is in progress
	SBDRemediationPhaseFencingInProgress SBDRemediationPhase = "FencingInProgress"
	// SBDRemediationPhaseFencedSuccessfully indicates the node was successfully fenced
	SBDRemediationPhaseFencedSuccessfully SBDRemediationPhase = "FencedSuccessfully"
	// SBDRemediationPhaseFailed indicates the remediation failed
	SBDRemediationPhaseFailed SBDRemediationPhase = "Failed"
)

// SBDRemediationReason represents the reason for the current remediation state
type SBDRemediationReason string

const (
	// SBDRemediationReasonHeartbeatTimeout indicates the node stopped sending heartbeats
	SBDRemediationReasonHeartbeatTimeout SBDRemediationReason = "HeartbeatTimeout"
	// SBDRemediationReasonNodeUnresponsive indicates the node is unresponsive
	SBDRemediationReasonNodeUnresponsive SBDRemediationReason = "NodeUnresponsive"
	// SBDRemediationReasonManualFencing indicates manual fencing was requested
	SBDRemediationReasonManualFencing SBDRemediationReason = "ManualFencing"
)

// SBDRemediationSpec defines the desired state of SBDRemediation.
type SBDRemediationSpec struct {
	// NodeName is the name of the Kubernetes node to be fenced
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`

	// Reason specifies why this node needs to be fenced
	// +kubebuilder:validation:Enum=HeartbeatTimeout;NodeUnresponsive;ManualFencing
	// +kubebuilder:default=NodeUnresponsive
	Reason SBDRemediationReason `json:"reason,omitempty"`

	// TimeoutSeconds specifies how long to wait before considering the fencing failed
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=300
	// +kubebuilder:default=60
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// SBDRemediationStatus defines the observed state of SBDRemediation.
type SBDRemediationStatus struct {
	// Phase indicates the current phase of the remediation process
	Phase SBDRemediationPhase `json:"phase,omitempty"`

	// Message provides a human-readable description of the current state
	Message string `json:"message,omitempty"`

	// LastUpdateTime is the time when this status was last updated
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// NodeID is the numeric ID assigned to the target node for SBD operations
	NodeID *uint16 `json:"nodeID,omitempty"`

	// FenceMessageWritten indicates if the fence message was successfully written to the SBD device
	FenceMessageWritten bool `json:"fenceMessageWritten,omitempty"`

	// OperatorInstance identifies which operator instance is handling this remediation
	OperatorInstance string `json:"operatorInstance,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="NodeID",type="integer",JSONPath=".status.nodeID"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SBDRemediation is the Schema for the sbdremediations API.
type SBDRemediation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SBDRemediationSpec   `json:"spec,omitempty"`
	Status SBDRemediationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SBDRemediationList contains a list of SBDRemediation.
type SBDRemediationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SBDRemediation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SBDRemediation{}, &SBDRemediationList{})
}
