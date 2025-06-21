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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/medik8s/sbd-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "sbd-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "sbd-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "sbd-operator-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "sbd-operator-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		if err != nil {
			// Check if namespace already exists
			cmd = exec.Command("kubectl", "get", "ns", namespace)
			_, getErr := utils.Run(cmd)
			if getErr != nil {
				Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")
			}
			// If namespace exists, clean it up first
			By("cleaning up existing namespace")
			cmd = exec.Command("kubectl", "delete", "all", "--all", "-n", namespace)
			_, _ = utils.Run(cmd)
		}

		// Check if we're running on OpenShift (CRC)
		isOpenShift := utils.IsCRCEnvironment() || os.Getenv("USE_CRC") == "true"

		if isOpenShift {
			By("configuring OpenShift security context constraints")
			// For OpenShift, we need to use SecurityContextConstraints instead of Pod Security Standards
			cmd = exec.Command("oc", "adm", "policy", "add-scc-to-group", "anyuid", "system:serviceaccounts:"+namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to add anyuid SCC to namespace")
		} else {
			By("labeling the namespace to enforce the restricted security policy")
			cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
				"pod-security.kubernetes.io/enforce=restricted")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")
		}

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		// Create a temporary kustomization for e2e testing with imagePullPolicy: Never
		err = createE2EKustomization(projectImage)
		Expect(err).NotTo(HaveOccurred(), "Failed to create e2e kustomization")

		cmd = exec.Command("kubectl", "apply", "-k", "test/e2e/", "--server-side=true")
		_, err = utils.Run(cmd)
		if err != nil {
			// Fallback to standard deployment if custom fails
			// Extract image components for environment variables
			parts := strings.Split(projectImage, "/")
			var registry, org, version string
			if len(parts) >= 3 {
				registry = parts[0]
				org = parts[1]
				imageVersion := strings.Split(parts[2], ":")
				if len(imageVersion) > 1 {
					version = imageVersion[1]
				} else {
					version = "latest"
				}
			}

			// Set environment and run deploy with new variables
			cmd = exec.Command("bash", "-c", fmt.Sprintf(
				"QUAY_REGISTRY=%s QUAY_ORG=%s VERSION=%s make deploy",
				registry, org, version))
			_, err = utils.Run(cmd)
		}
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("cleaning up metrics ClusterRoleBinding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			// Clean up any existing clusterrolebinding first
			cmd := exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found=true")
			_, _ = utils.Run(cmd) // Ignore errors for cleanup

			cmd = exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=sbd-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccount": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			Expect(metricsOutput).To(ContainSubstring(
				"controller_runtime_reconcile_total",
			))
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	Context("SBD Agent", func() {
		var sbdConfigName string
		var tmpFile string

		BeforeEach(func() {
			sbdConfigName = fmt.Sprintf("test-sbdconfig-%d", time.Now().UnixNano())
		})

		AfterEach(func() {
			// Clean up SBDConfig
			By("cleaning up SBDConfig resource")
			cmd := exec.Command("kubectl", "delete", "sbdconfig", sbdConfigName, "--ignore-not-found=true")
			_, _ = utils.Run(cmd)

			// Clean up any remaining DaemonSets
			By("cleaning up SBD agent DaemonSets")
			cmd = exec.Command("kubectl", "delete", "daemonset", "-l", "app.kubernetes.io/name=sbd-agent", "-n", "sbd-system", "--ignore-not-found=true")
			_, _ = utils.Run(cmd)

			// Clean up temporary file
			if tmpFile != "" {
				os.Remove(tmpFile)
			}
		})

		It("should deploy SBD agent DaemonSet when SBDConfig is created", func() {
			By("creating an SBDConfig resource")
			sbdConfigYAML := fmt.Sprintf(`
apiVersion: medik8s.medik8s.io/v1alpha1
kind: SBDConfig
metadata:
  name: %s
spec:
  sbdWatchdogPath: "/dev/null"
  image: "quay.io/medik8s/sbd-agent:latest"
  namespace: "sbd-system"
`, sbdConfigName)

			// Write SBDConfig to temporary file
			tmpFile = filepath.Join("/tmp", fmt.Sprintf("sbdconfig-%s.yaml", sbdConfigName))
			err := os.WriteFile(tmpFile, []byte(sbdConfigYAML), 0644)
			Expect(err).NotTo(HaveOccurred())

			// Apply the SBDConfig
			cmd := exec.Command("kubectl", "apply", "-f", tmpFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create SBDConfig")

			By("verifying the SBDConfig resource exists")
			verifySBDConfigExists := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sbdconfig", sbdConfigName, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(sbdConfigName))
			}
			Eventually(verifySBDConfigExists, 30*time.Second).Should(Succeed())

			By("verifying the SBD agent DaemonSet is created")
			expectedDaemonSetName := fmt.Sprintf("sbd-agent-%s", sbdConfigName)
			verifyDaemonSetCreated := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "daemonset", expectedDaemonSetName, "-n", "sbd-system", "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(expectedDaemonSetName))
			}
			Eventually(verifyDaemonSetCreated, 60*time.Second).Should(Succeed())

			By("verifying the DaemonSet has correct image and configuration")
			verifyDaemonSetConfig := func(g Gomega) {
				// Check image
				cmd := exec.Command("kubectl", "get", "daemonset", expectedDaemonSetName, "-n", "sbd-system", "-o", "jsonpath={.spec.template.spec.containers[0].image}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("quay.io/medik8s/sbd-agent:latest"))

				// Check watchdog path argument
				cmd = exec.Command("kubectl", "get", "daemonset", expectedDaemonSetName, "-n", "sbd-system", "-o", "jsonpath={.spec.template.spec.containers[0].args}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("--watchdog-path=/dev/null"))
			}
			Eventually(verifyDaemonSetConfig, 30*time.Second).Should(Succeed())

			By("verifying the DaemonSet has expected number of pods scheduled")
			verifyDaemonSetScheduled := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "daemonset", expectedDaemonSetName, "-n", "sbd-system", "-o", "jsonpath={.status.desiredNumberScheduled}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Should have at least 1 pod scheduled (for the single CRC node)
				g.Expect(output).ToNot(BeEmpty())
				g.Expect(output).ToNot(Equal("0"))
			}
			Eventually(verifyDaemonSetScheduled, 60*time.Second).Should(Succeed())

			By("verifying the SBDConfig status reflects DaemonSet creation")
			verifySBDConfigStatus := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sbdconfig", sbdConfigName, "-o", "jsonpath={.status.totalNodes}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Should have at least 1 node targeted
				g.Expect(output).ToNot(BeEmpty())
				g.Expect(output).ToNot(Equal("0"))
			}
			Eventually(verifySBDConfigStatus, 60*time.Second).Should(Succeed())
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() string {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
	return metricsOutput
}

// createE2EKustomization creates a dynamic kustomization file for e2e testing
func createE2EKustomization(imageName string) error {
	// Parse the image name to extract components
	parts := strings.Split(imageName, "/")
	var newName, newTag string

	if len(parts) >= 2 {
		imageParts := strings.Split(parts[len(parts)-1], ":")
		newName = strings.Join(parts[:len(parts)-1], "/") + "/" + imageParts[0]
		if len(imageParts) > 1 {
			newTag = imageParts[1]
		} else {
			newTag = "latest"
		}
	} else {
		imageParts := strings.Split(imageName, ":")
		newName = imageParts[0]
		if len(imageParts) > 1 {
			newTag = imageParts[1]
		} else {
			newTag = "latest"
		}
	}

	kustomizationContent := fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
- ../../config/default

images:
- name: controller:latest
  newName: %s
  newTag: %s

patches:
- path: image-pull-policy-patch.yaml
  target:
    kind: Deployment
    name: controller-manager
`, newName, newTag)

	return os.WriteFile("test/e2e/kustomization.yaml", []byte(kustomizationContent), 0644)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
