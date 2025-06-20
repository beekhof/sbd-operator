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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	medik8sv1alpha1 "github.com/medik8s/sbd-operator/api/v1alpha1"
)

// SBDConfigReconciler reconciles a SBDConfig object
type SBDConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=medik8s.medik8s.io,resources=sbdconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For SBDConfig, this implementation deploys and manages the SBD Agent DaemonSet.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *SBDConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Retrieve the SBDConfig object
	var sbdConfig medik8sv1alpha1.SBDConfig
	if err := r.Get(ctx, req.NamespacedName, &sbdConfig); err != nil {
		if errors.IsNotFound(err) {
			// The SBDConfig resource was deleted
			logger.Info("SBDConfig resource not found. It may have been deleted", "name", req.Name)
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request
		logger.Error(err, "Failed to get SBDConfig", "name", req.Name)
		return ctrl.Result{}, err
	}

	// Set default values if not specified
	if sbdConfig.Spec.Image == "" {
		sbdConfig.Spec.Image = "sbd-agent:latest"
	}
	if sbdConfig.Spec.Namespace == "" {
		sbdConfig.Spec.Namespace = "sbd-system"
	}
	if sbdConfig.Spec.SbdWatchdogPath == "" {
		sbdConfig.Spec.SbdWatchdogPath = "/dev/watchdog"
	}

	// Ensure the namespace exists
	if err := r.ensureNamespace(ctx, sbdConfig.Spec.Namespace); err != nil {
		logger.Error(err, "Failed to ensure namespace exists", "namespace", sbdConfig.Spec.Namespace)
		return ctrl.Result{}, err
	}

	// Define the desired DaemonSet
	desiredDaemonSet := r.buildDaemonSet(&sbdConfig)

	// Use CreateOrUpdate to manage the DaemonSet
	actualDaemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desiredDaemonSet.Name,
			Namespace: desiredDaemonSet.Namespace,
		},
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, actualDaemonSet, func() error {
		// Update the DaemonSet spec with the desired configuration
		actualDaemonSet.Spec = desiredDaemonSet.Spec
		actualDaemonSet.Labels = desiredDaemonSet.Labels
		actualDaemonSet.Annotations = desiredDaemonSet.Annotations

		// Set the controller reference
		return controllerutil.SetControllerReference(&sbdConfig, actualDaemonSet, r.Scheme)
	})
	if err != nil {
		logger.Error(err, "Failed to create or update DaemonSet", "DaemonSet", desiredDaemonSet.Name)
		return ctrl.Result{}, err
	}

	logger.Info("DaemonSet operation completed", "DaemonSet", actualDaemonSet.Name, "operation", result)

	// Update the SBDConfig status
	if err := r.updateStatus(ctx, &sbdConfig, actualDaemonSet); err != nil {
		logger.Error(err, "Failed to update SBDConfig status")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled SBDConfig", "name", sbdConfig.Name, "namespace", sbdConfig.Namespace)

	return ctrl.Result{}, nil
}

// ensureNamespace creates the namespace if it doesn't exist
func (r *SBDConfigReconciler) ensureNamespace(ctx context.Context, namespaceName string) error {
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespaceName,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, namespace, func() error {
		// No changes needed for namespace
		return nil
	})

	return err
}

// buildDaemonSet constructs the desired DaemonSet based on the SBDConfig
func (r *SBDConfigReconciler) buildDaemonSet(sbdConfig *medik8sv1alpha1.SBDConfig) *appsv1.DaemonSet {
	daemonSetName := fmt.Sprintf("sbd-agent-%s", sbdConfig.Name)
	labels := map[string]string{
		"app":        "sbd-agent",
		"component":  "sbd-agent",
		"version":    "latest",
		"managed-by": "sbd-operator",
		"sbdconfig":  sbdConfig.Name,
	}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      daemonSetName,
			Namespace: sbdConfig.Spec.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":       "sbd-agent",
					"sbdconfig": sbdConfig.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"prometheus.io/scrape": "false",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "sbd-agent",
					HostNetwork:        true,
					HostPID:            true,
					DNSPolicy:          corev1.DNSClusterFirstWithHostNet,
					PriorityClassName:  "system-node-critical",
					RestartPolicy:      corev1.RestartPolicyAlways,
					NodeSelector: map[string]string{
						"kubernetes.io/os": "linux",
					},
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "kubernetes.io/os",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"linux"},
											},
										},
									},
								},
							},
						},
					},
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
						{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpExists},
						{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule},
						{Key: "node-role.kubernetes.io/master", Effect: corev1.TaintEffectNoSchedule},
					},
					Containers: []corev1.Container{
						{
							Name:            "sbd-agent",
							Image:           sbdConfig.Spec.Image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								Privileged:               &[]bool{true}[0],
								RunAsUser:                &[]int64{0}[0],
								RunAsGroup:               &[]int64{0}[0],
								RunAsNonRoot:             &[]bool{false}[0],
								ReadOnlyRootFilesystem:   &[]bool{false}[0],
								AllowPrivilegeEscalation: &[]bool{true}[0],
								Capabilities: &corev1.Capabilities{
									Add:  []corev1.Capability{"SYS_ADMIN"},
									Drop: []corev1.Capability{"ALL"},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: mustParseQuantity("128Mi"),
									corev1.ResourceCPU:    mustParseQuantity("50m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: mustParseQuantity("256Mi"),
									corev1.ResourceCPU:    mustParseQuantity("100m"),
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
									},
								},
								{
									Name: "POD_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
									},
								},
								{
									Name: "POD_NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
									},
								},
								{
									Name: "POD_IP",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
									},
								},
							},
							Args: []string{
								fmt.Sprintf("--watchdog-path=%s", sbdConfig.Spec.SbdWatchdogPath),
								"--watchdog-timeout=30s",
								"--log-level=info",
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "dev", MountPath: "/dev"},
								{Name: "sys", MountPath: "/sys", ReadOnly: true},
								{Name: "proc", MountPath: "/proc", ReadOnly: true},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "-c", "pgrep -f sbd-agent > /dev/null"},
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       30,
								TimeoutSeconds:      10,
								FailureThreshold:    3,
								SuccessThreshold:    1,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "-c", fmt.Sprintf("test -c %s && pgrep -f sbd-agent > /dev/null", sbdConfig.Spec.SbdWatchdogPath)},
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    3,
								SuccessThreshold:    1,
							},
							StartupProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "-c", "pgrep -f sbd-agent > /dev/null"},
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       5,
								TimeoutSeconds:      5,
								FailureThreshold:    6,
								SuccessThreshold:    1,
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "dev",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/dev",
									Type: &[]corev1.HostPathType{corev1.HostPathDirectory}[0],
								},
							},
						},
						{
							Name: "sys",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/sys",
									Type: &[]corev1.HostPathType{corev1.HostPathDirectory}[0],
								},
							},
						},
						{
							Name: "proc",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/proc",
									Type: &[]corev1.HostPathType{corev1.HostPathDirectory}[0],
								},
							},
						},
					},
					TerminationGracePeriodSeconds: &[]int64{30}[0],
				},
			},
		},
	}
}

// updateStatus updates the SBDConfig status based on the DaemonSet state
func (r *SBDConfigReconciler) updateStatus(ctx context.Context, sbdConfig *medik8sv1alpha1.SBDConfig, daemonSet *appsv1.DaemonSet) error {
	// Check if we need to fetch the latest DaemonSet status
	latestDaemonSet := &appsv1.DaemonSet{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      daemonSet.Name,
		Namespace: daemonSet.Namespace,
	}, latestDaemonSet)
	if err != nil {
		return err
	}

	// Update status fields
	sbdConfig.Status.TotalNodes = latestDaemonSet.Status.DesiredNumberScheduled
	sbdConfig.Status.ReadyNodes = latestDaemonSet.Status.NumberReady
	sbdConfig.Status.DaemonSetReady = latestDaemonSet.Status.NumberReady == latestDaemonSet.Status.DesiredNumberScheduled && latestDaemonSet.Status.DesiredNumberScheduled > 0

	// Update the status
	return r.Status().Update(ctx, sbdConfig)
}

// mustParseQuantity is a helper function for parsing resource quantities
func mustParseQuantity(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(err)
	}
	return q
}

// SetupWithManager sets up the controller with the Manager.
func (r *SBDConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&medik8sv1alpha1.SBDConfig{}).
		Owns(&appsv1.DaemonSet{}).
		Named("sbdconfig").
		Complete(r)
}
