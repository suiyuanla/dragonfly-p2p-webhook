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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"d7y.io/dragonfly-p2p-webhook/internal/webhook/v1/injector"
	"d7y.io/dragonfly-p2p-webhook/test/utils"
	corev1 "k8s.io/api/core/v1"
)

// namespace where the project is deployed in
const namespace = "dragonfly-p2p-webhook-system"

// serviceAccountName created for the project
const serviceAccountName = "dragonfly-p2p-webhook-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "dragonfly-p2p-webhook-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "dragonfly-p2p-webhook-metrics-binding"

const testNamespace = "webhook-test-ns"
const testPodName = "test-pod"

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
	})

	// +kubebuilder:scaffold:e2e-webhooks-checks
	Context("Webhook Injection", func() {

		// Create a new namespace for each test to ensure isolation.
		BeforeEach(func() {
			By(fmt.Sprintf("creating test namespace %s", testNamespace))
			cmd := exec.Command("kubectl", "create", "ns", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		// Clean up the namespace after each test.
		AfterEach(func() {
			By(fmt.Sprintf("deleting test namespace %s", testNamespace))
			cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		// Test case 1: Injection is triggered by a namespace label.
		It("should inject initContainer and volumes when namespace is labeled", func() {
			By("labeling the namespace to enable injection")
			cmd := exec.Command("kubectl", "label", "ns", testNamespace,
				fmt.Sprintf("%s=%s", injector.NamespaceInjectLabelName, injector.NamespaceInjectLabelValue))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a pod in the labeled namespace")
			pod, err := createTestPod(nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod).NotTo(BeNil())

			By("verifying the pod has been injected correctly")
			defaultConfig := injector.NewDefaultInjectConf()
			verifyPodInjected(pod, defaultConfig)
		})

		// Test case 2: Injection is triggered by a pod annotation.
		It("should inject initContainer and volumes when pod is annotated", func() {
			By("creating a pod with injection annotation")
			annotations := map[string]string{
				injector.PodInjectAnnotationName: injector.PodInjectAnnotationValue,
			}
			pod, err := createTestPod(annotations)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod).NotTo(BeNil())

			By("verifying the pod has been injected correctly")
			defaultConfig := injector.NewDefaultInjectConf()
			verifyPodInjected(pod, defaultConfig)
		})

		// Test case 3: Injection is skipped when no labels or annotations are present.
		It("should not inject when neither namespace nor pod is configured for injection", func() {
			By("creating a standard pod")
			pod, err := createTestPod(nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod).NotTo(BeNil())

			By("verifying the pod has not been injected")
			verifyPodNotInjected(pod)
		})

		// Test case 4: Injection uses custom image from pod annotation.
		It("should use custom cli-tools image from pod annotation", func() {
			customImage := "dragonflyoss/cli-tools:custom-test-tag"
			By("creating a pod with custom image annotation")
			annotations := map[string]string{
				injector.PodInjectAnnotationName: injector.PodInjectAnnotationValue,
				injector.CliToolsImageAnnotation: customImage,
			}
			pod, err := createTestPod(annotations)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod).NotTo(BeNil())

			By("verifying the pod has been injected with the custom image")
			customConfig := injector.NewDefaultInjectConf()
			customConfig.CliToolsImage = customImage // The expected image is now the custom one.
			verifyPodInjected(pod, customConfig)
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

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// createTestPod is a helper function to create a simple pod for testing.
// It applies a YAML manifest via kubectl and waits for the pod to be created.
func createTestPod(annotations map[string]string) (*corev1.Pod, error) {
	name := testPodName
	ns := testNamespace
	podYAML := `
apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  containers:
  - name: busybox
    image: busybox:latest
    command: ["sleep", "3600"]
`
	podYAML = fmt.Sprintf(podYAML, name)

	// A temporary pod object for adding annotations
	tempPod := &corev1.Pod{}
	err := json.Unmarshal([]byte(podYAML), tempPod)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal base pod YAML: %w", err)
	}
	if len(annotations) > 0 {
		tempPod.Annotations = annotations
	}
	// Marshal it back to JSON (as kubectl apply -f - prefers json)
	finalPodBytes, err := json.Marshal(tempPod)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal final pod: %w", err)
	}

	cmd := exec.Command("kubectl", "apply", "-n", ns, "-f", "-")
	cmd.Stdin = bytes.NewBuffer(finalPodBytes)
	_, err = utils.Run(cmd)
	if err != nil {
		return nil, err
	}

	var pod *corev1.Pod
	Eventually(func(g Gomega) error {
		pod, err = getPod(context.Background(), name, ns)
		return err
	}).Should(Succeed())

	return pod, nil
}

// getPod fetches a pod resource from the cluster.
func getPod(ctx context.Context, name, ns string) (*corev1.Pod, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "get", "pod", name, "-n", ns, "-o", "json")
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, err
	}

	var pod corev1.Pod
	if err := json.Unmarshal([]byte(output), &pod); err != nil {
		return nil, err
	}
	return &pod, nil
}

// verifyPodInjected checks if a pod has been correctly mutated by the webhook.
func verifyPodInjected(pod *corev1.Pod, config *injector.InjectConf) {
	// 1. Verify InitContainer
	Expect(pod.Spec.InitContainers).To(HaveLen(1), "should have one initContainer")
	initContainer := pod.Spec.InitContainers[0]
	Expect(initContainer.Name).To(Equal(injector.CliToolsInitContainerName))
	Expect(initContainer.Image).To(Equal(config.CliToolsImage))

	cliToolsVolumeMountPath := filepath.Clean(config.CliToolsDirPath) + "-mount"
	expectedCmd := []string{"cp", "-rf", config.CliToolsDirPath + "/.", cliToolsVolumeMountPath + "/"}
	Expect(initContainer.Command).To(Equal(expectedCmd))
	Expect(initContainer.VolumeMounts).To(ContainElement(HaveField("Name", injector.CliToolsVolumeName)))
	Expect(initContainer.VolumeMounts).To(ContainElement(HaveField("MountPath", cliToolsVolumeMountPath)))

	// 2. Verify Volumes
	Expect(pod.Spec.Volumes).To(ContainElement(HaveField("Name", injector.DfdaemonUnixSockVolumeName)))
	Expect(pod.Spec.Volumes).To(ContainElement(HaveField("Name", injector.CliToolsVolumeName)))

	// 3. Verify Main Container
	Expect(pod.Spec.Containers).NotTo(BeEmpty())
	mainContainer := pod.Spec.Containers[0]

	// 3.1. Verify Main Container VolumeMounts
	Expect(mainContainer.VolumeMounts).To(ContainElement(HaveField("Name", injector.DfdaemonUnixSockVolumeName)))
	Expect(mainContainer.VolumeMounts).To(ContainElement(HaveField("MountPath", injector.DfdaemonUnixSockPath)))
	Expect(mainContainer.VolumeMounts).To(ContainElement(HaveField("Name", injector.CliToolsVolumeName)))
	Expect(mainContainer.VolumeMounts).To(ContainElement(HaveField("MountPath", cliToolsVolumeMountPath)))

	// 3.2. Verify Main Container Environment Variables
	Expect(mainContainer.Env).To(
		ContainElement(
			corev1.EnvVar{
				Name: injector.NodeNameEnvName,
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "spec.nodeName",
					},
				},
			},
		),
	)
	Expect(mainContainer.Env).To(
		ContainElement(
			corev1.EnvVar{
				Name:  injector.ProxyPortEnvName,
				Value: strconv.Itoa(config.ProxyPort),
			},
		),
	)
	Expect(mainContainer.Env).To(
		ContainElement(
			corev1.EnvVar{
				Name:  injector.ProxyEnvName,
				Value: "http://$(" + injector.NodeNameEnvName + "):$(" + injector.ProxyPortEnvName + ")",
			},
		),
	)
	Expect(mainContainer.Env).To(
		ContainElement(
			corev1.EnvVar{
				Name:  injector.CliToolsPathEnvName,
				Value: cliToolsVolumeMountPath,
			},
		),
	)
}

// verifyPodNotInjected checks if a pod remains unchanged.
func verifyPodNotInjected(pod *corev1.Pod) {
	// Original pod has 0 init containers.
	Expect(pod.Spec.InitContainers).To(BeEmpty())

	// Original pod has a default service account token volume.
	originalVolumeCount := 0
	for _, v := range pod.Spec.Volumes {
		if strings.HasPrefix(v.Name, "kube-api-access") {
			originalVolumeCount = 1
		}
	}
	Expect(pod.Spec.Volumes).To(HaveLen(originalVolumeCount))

	// Check main container
	Expect(pod.Spec.Containers).NotTo(BeEmpty())
	mainContainer := pod.Spec.Containers[0]

	// Check that no injected env vars are present.
	for _, env := range mainContainer.Env {
		Expect(env.Name).NotTo(Equal(injector.NodeNameEnvName))
		Expect(env.Name).NotTo(Equal(injector.ProxyPortEnvName))
		Expect(env.Name).NotTo(Equal(injector.ProxyEnvName))
		Expect(env.Name).NotTo(Equal(injector.CliToolsPathEnvName))
	}
}
