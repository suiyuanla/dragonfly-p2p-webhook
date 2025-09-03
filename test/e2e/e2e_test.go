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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"d7y.io/dragonfly-p2p-webhook/test/utils"
)

// namespace where the project is deployed in
const namespace = "dragonfly-p2p-webhook-system"

// serviceAccountName created for the project
const serviceAccountName = "dragonfly-p2p-webhook-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "dragonfly-p2p-webhook-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "dragonfly-p2p-webhook-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		// By("installing CRDs")
		// cmd = exec.Command("make", "install")
		// _, err = utils.Run(cmd)
		// Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		// By("uninstalling CRDs")
		// cmd = exec.Command("make", "uninstall")
		// _, _ = utils.Run(cmd)

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
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=dragonfly-p2p-webhook-metrics-reader",
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
							"imagePullPolicy": "IfNotPresent",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"readOnlyRootFilesystem": true,
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
						"serviceAccountName": "%s"
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
				"controller_runtime_webhook_requests_total",
			))
		})

		It("should provisioned cert-manager", func() {
			By("validating that cert-manager has the certificate Secret")
			verifyCertManager := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secrets", "webhook-server-cert", "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyCertManager).Should(Succeed())
		})

		It("should have webhook configuration ready", func() {
			By("validating that the webhook service is available")
			verifyWebhookService := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", "dragonfly-p2p-webhook-webhook-service", "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyWebhookService).Should(Succeed())

			By("validating webhook service has endpoints")
			verifyWebhookEndpoints := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", "dragonfly-p2p-webhook-webhook-service", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("443"))
			}
			Eventually(verifyWebhookEndpoints).Should(Succeed())
		})

		It("should inject dragonfly configuration via namespace label", func() {
			By("creating a test namespace with dragonfly injection label")
			testNamespace := "test-dragonfly-injection"

			By("ensuring test namespace is clean")
			cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found=true", "--wait=true")
			_, _ = utils.Run(cmd)

			defer func() {
				cmd = exec.Command("kubectl", "delete", "ns", testNamespace,
					"--ignore-not-found=true", "--wait=true")
				_, _ = utils.Run(cmd)
			}()

			By("creating test namespace")
			cmd = exec.Command("kubectl", "create", "ns", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("labeling namespace for dragonfly injection")
			cmd = exec.Command("kubectl", "label", "namespace", testNamespace,
				"dragonfly.io/inject=enabled")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating test pod in labeled namespace")
			podCfg := `{"spec":{"containers":[{"name":"test","image":"nginx:latest"}]}}`
			createTestNamespaceAndPod(testNamespace, "test-pod", podCfg)

			By("waiting for the pod to be created and checking for dragonfly injection")
			verifyInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", "test-pod", "-n", testNamespace,
					"-o", "jsonpath={.spec.initContainers}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("dragonfly-cli-tools"))
			}
			Eventually(verifyInjection, 30*time.Second).Should(Succeed())
		})

		It("should inject dragonfly configuration via pod annotation", func() {
			By("creating a test namespace and pod with dragonfly injection annotation")
			testNamespace := "test-dragonfly-pod-annotation"

			defer func() {
				cmd := exec.Command("kubectl", "delete", "ns", testNamespace,
					"--ignore-not-found=true", "--wait=true")
				_, _ = utils.Run(cmd)
			}()

			podCfg := `{"metadata":{"annotations":{"dragonfly.io/inject":"enabled"}},` +
				`"spec":{"containers":[{"name":"test","image":"nginx:latest"}]}}`
			createTestNamespaceAndPod(testNamespace, "test-pod-annotated", podCfg)

			By("checking for dragonfly injection in the annotated pod")
			verifyInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", "test-pod-annotated", "-n", testNamespace,
					"-o", "jsonpath={.spec.volumes}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("dfdaemon-unix-socket"))
			}
			Eventually(verifyInjection, 30*time.Second).Should(Succeed())
		})

		It("should not inject dragonfly when injection is disabled", func() {
			By("creating a test namespace and pod without injection annotations")
			testNamespace := "test-dragonfly-no-injection"

			defer func() {
				cmd := exec.Command("kubectl", "delete", "ns", testNamespace,
					"--ignore-not-found=true", "--wait=true")
				_, _ = utils.Run(cmd)
			}()

			podCfg := `{"spec":{"containers":[{"name":"test","image":"nginx:latest"}]}}`
			createTestNamespaceAndPod(testNamespace, "test-pod-no-inject", podCfg)

			By("verifying no dragonfly injection occurred")
			verifyNoInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", "test-pod-no-inject", "-n", testNamespace,
					"-o", "jsonpath={.spec.initContainers}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(ContainSubstring("dragonfly-cli-tools"))
			}
			Eventually(verifyNoInjection, 30*time.Second).Should(Succeed())
		})

		It("should have valid inject configuration configmap", func() {
			By("validating the inject-config configmap exists and has valid data")
			verifyConfigMap := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", "inject-config", "-n", namespace,
					"-o", "jsonpath={.data.config-yaml}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("enable: true"))
				g.Expect(output).To(ContainSubstring("proxy_port: 4001"))
				g.Expect(output).To(ContainSubstring("cli_tools_image: dragonflyoss/cli-tools:latest"))
			}
			Eventually(verifyConfigMap).Should(Succeed())
		})

		It("should exclude injection when explicitly disabled", func() {
			By("creating a test namespace and pod with injection disabled annotation")
			testNamespace := "test-dragonfly-exclude"

			defer func() {
				cmd := exec.Command("kubectl", "delete", "ns", testNamespace,
					"--ignore-not-found=true", "--wait=true")
				_, _ = utils.Run(cmd)
			}()

			podCfg := `{"metadata":{"annotations":{"dragonfly.io/inject":"disabled"}},` +
				`"spec":{"containers":[{"name":"test","image":"nginx:latest"}]}}`
			createTestNamespaceAndPod(testNamespace, "test-pod-exclude", podCfg)

			By("verifying injection is excluded")
			verifyNoInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", "test-pod-exclude", "-n", testNamespace,
					"-o", "jsonpath={.spec.volumes}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(ContainSubstring("dfdaemon-unix-socket"))
			}
			Eventually(verifyNoInjection, 30*time.Second).Should(Succeed())
		})

		It("should have CA injection for mutating webhooks", func() {
			By("checking CA injection for mutating webhooks")
			verifyCAInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"mutatingwebhookconfigurations.admissionregistration.k8s.io",
					"dragonfly-p2p-webhook-mutating-webhook-configuration",
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				mwhOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(mwhOutput)).To(BeNumerically(">", 10))
			}
			Eventually(verifyCAInjection).Should(Succeed())
		})

		It("should validate webhook metrics are comprehensive", func() {
			By("collecting and validating webhook metrics")

			metricsOutput := getMetricsOutput()

			// Validate webhook-specific metrics
			webhookMetrics := []string{
				"controller_runtime_webhook_requests_total",
				"controller_runtime_webhook_request_duration_seconds",
				"controller_runtime_webhook_requests_in_flight",
				"workqueue_depth",
				"workqueue_adds_total",
			}

			for _, metric := range webhookMetrics {
				Expect(metricsOutput).To(ContainSubstring(metric), fmt.Sprintf("Expected metric %s not found", metric))
			}

			By("ensuring metrics contain webhook-specific labels")
			Expect(metricsOutput).To(ContainSubstring(`webhook="pod-defaulter"`))
			Expect(metricsOutput).To(ContainSubstring(`webhook="pod-validator"`))
		})

		It("should handle custom CLI tools image configuration", func() {
			By("updating the inject-config configmap with custom image")

			customImage := "custom-registry/dragonfly-cli-tools:v1.0.0"
			patchData := fmt.Sprintf(`{"data":{"config-yaml":`+
				`"enable: true\nproxy_port: 4001\ncli_tools_image: %s\ncli_tools_dir_path: /dragonfly-tools"}}`, customImage)

			cmd := exec.Command("kubectl", "patch", "configmap", "inject-config", "-n", namespace, "-p", patchData)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a test namespace with dragonfly injection")
			testNamespace := "test-dragonfly-custom-config"

			defer func() {
				cmd = exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found=true", "--wait=true")
				_, _ = utils.Run(cmd)
			}()

			By("ensuring test namespace is clean")
			cmd = exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found=true", "--wait=true")
			_, _ = utils.Run(cmd)

			By("creating test namespace")
			cmd = exec.Command("kubectl", "create", "ns", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("labeling namespace for dragonfly injection")
			cmd = exec.Command("kubectl", "label", "namespace", testNamespace, "dragonfly.io/inject=enabled")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating test pod in labeled namespace")
			createTestNamespaceAndPod(testNamespace, "test-pod-custom",
				`{"spec": {"containers": [{"name": "test", "image": "nginx:latest"}]}}`)

			By("verifying the custom image is used in the injected init container")
			verifyCustomImage := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", "test-pod-custom", "-n", testNamespace,
					"-o", "jsonpath={.spec.initContainers[?(@.name==\"dragonfly-cli-tools\")].image}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(customImage))
			}
			Eventually(verifyCustomImage, 60*time.Second).Should(Succeed())
		})

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput := getMetricsOutput()
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
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

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// createTestNamespaceAndPod creates a test namespace and pod with the specified configuration
// Note: Caller is responsible for cleanup
func createTestNamespaceAndPod(namespace, podName, podOverrides string) {
	By(fmt.Sprintf("ensuring test namespace %s is clean", namespace))
	cmd := exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found=true", "--wait=true")
	_, _ = utils.Run(cmd)

	By(fmt.Sprintf("creating test namespace %s", namespace))
	cmd = exec.Command("kubectl", "create", "ns", namespace)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By(fmt.Sprintf("creating test pod %s/%s", namespace, podName))
	cmd = exec.Command("kubectl", "run", podName,
		"--namespace", namespace,
		"--image=nginx:latest",
		"--overrides", podOverrides)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}
