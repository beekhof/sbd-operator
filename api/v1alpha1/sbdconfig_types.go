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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SBDConfigSpec defines the desired state of SBDConfig.
type SBDConfigSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// SbdWatchdogPath is the path to the watchdog device on the host
	// +kubebuilder:validation:Required
	// +kubebuilder:default="/dev/watchdog"
	SbdWatchdogPath string `json:"sbdWatchdogPath"`

	// Image is the container image for the SBD agent DaemonSet
	// +kubebuilder:default="sbd-agent:latest"
	Image string `json:"image,omitempty"`

	// Namespace is the namespace where the SBD agent DaemonSet will be deployed
	// +kubebuilder:default="sbd-system"
	Namespace string `json:"namespace,omitempty"`
}

// SBDConfigStatus defines the observed state of SBDConfig.
type SBDConfigStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// DaemonSetReady indicates whether the SBD agent DaemonSet is ready
	DaemonSetReady bool `json:"daemonSetReady,omitempty"`

	// ReadyNodes is the number of nodes where the SBD agent is ready
	ReadyNodes int32 `json:"readyNodes,omitempty"`

	// TotalNodes is the total number of nodes where the SBD agent should be deployed
	TotalNodes int32 `json:"totalNodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// SBDConfig is the Schema for the sbdconfigs API.
type SBDConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SBDConfigSpec   `json:"spec,omitempty"`
	Status SBDConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SBDConfigList contains a list of SBDConfig.
type SBDConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SBDConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SBDConfig{}, &SBDConfigList{})
}
