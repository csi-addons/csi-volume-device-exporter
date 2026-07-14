//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	exporterNamespace  = "csi-volume-device-exporter"
	exporterLabel      = "app.kubernetes.io/name=csi-volume-device-exporter"
	testNamespace      = "csi-exporter-e2e-test"
	metricsPort        = 9710
	defaultPollTimeout = 2 * time.Minute
	defaultPollPeriod  = 5 * time.Second
)

// sharedNamespace ensures the test namespace is created once and deleted at
// process exit, preventing "namespace is terminating" races between tests.
var (
	namespaceOnce    sync.Once
	sharedClient     kubernetes.Interface
	sharedKubeconfig string
)

func initSharedClient(t *testing.T) {
	t.Helper()
	namespaceOnce.Do(func() {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			home, _ := os.UserHomeDir()
			kubeconfig = home + "/.kube/config"
		}
		sharedKubeconfig = kubeconfig

		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			t.Fatalf("failed to build kubeconfig: %v", err)
		}
		client, err := kubernetes.NewForConfig(config)
		if err != nil {
			t.Fatalf("failed to create kubernetes client: %v", err)
		}
		sharedClient = client

		ns := os.Getenv("E2E_TEST_NAMESPACE")
		if ns == "" {
			ns = testNamespace
		}

		// Create the shared test namespace (idempotent)
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		_, err = client.CoreV1().Namespaces().Create(context.TODO(), nsObj, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			t.Fatalf("failed to create namespace %s: %v", ns, err)
		}
	})
}

type framework struct {
	client       kubernetes.Interface
	namespace    string
	storageClass string
}

func newFramework(t *testing.T) *framework {
	t.Helper()
	initSharedClient(t)

	ns := os.Getenv("E2E_TEST_NAMESPACE")
	if ns == "" {
		ns = testNamespace
	}

	return &framework{
		client:       sharedClient,
		namespace:    ns,
		storageClass: os.Getenv("E2E_STORAGE_CLASS"),
	}
}

func (f *framework) createPVC(t *testing.T, name string, volumeMode corev1.PersistentVolumeMode) *corev1.PersistentVolumeClaim {
	t.Helper()

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
			VolumeMode: &volumeMode,
		},
	}

	if f.storageClass != "" {
		pvc.Spec.StorageClassName = &f.storageClass
	}

	created, err := f.client.CoreV1().PersistentVolumeClaims(f.namespace).Create(
		context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create PVC %s: %v", name, err)
	}
	return created
}

func (f *framework) createPodWithPVC(t *testing.T, name string, pvcName string, volumeMode corev1.PersistentVolumeMode) *corev1.Pod {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "test",
					Image:   "registry.k8s.io/pause:3.9",
					Command: []string{"/pause"},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "test-vol",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}

	if volumeMode == corev1.PersistentVolumeBlock {
		pod.Spec.Containers[0].VolumeDevices = []corev1.VolumeDevice{
			{Name: "test-vol", DevicePath: "/dev/testblock"},
		}
	} else {
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{Name: "test-vol", MountPath: "/mnt/test"},
		}
	}

	created, err := f.client.CoreV1().Pods(f.namespace).Create(
		context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create pod %s: %v", name, err)
	}
	return created
}

func (f *framework) waitForPodRunning(t *testing.T, name string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pod, err := f.client.CoreV1().Pods(f.namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return pod.Status.Phase == corev1.PodRunning, nil
		})
	if err != nil {
		t.Fatalf("pod %s did not reach Running phase within %v", name, defaultPollTimeout)
	}
}

func (f *framework) waitForPVCBound(t *testing.T, name string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pvc, err := f.client.CoreV1().PersistentVolumeClaims(f.namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return pvc.Status.Phase == corev1.ClaimBound, nil
		})
	if err != nil {
		t.Fatalf("PVC %s did not reach Bound phase within %v", name, defaultPollTimeout)
	}
}

func (f *framework) deletePod(t *testing.T, name string) {
	t.Helper()
	err := f.client.CoreV1().Pods(f.namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		t.Fatalf("failed to delete pod %s: %v", name, err)
	}
}

func (f *framework) waitForPodGone(t *testing.T, name string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := f.client.CoreV1().Pods(f.namespace).Get(ctx, name, metav1.GetOptions{})
			if errors.IsNotFound(err) {
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("pod %s was not deleted within %v", name, defaultPollTimeout)
	}
}

func (f *framework) getExporterPods(t *testing.T) []corev1.Pod {
	t.Helper()

	ns := os.Getenv("E2E_EXPORTER_NAMESPACE")
	if ns == "" {
		ns = exporterNamespace
	}

	pods, err := f.client.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{
		LabelSelector: exporterLabel,
	})
	if err != nil {
		t.Fatalf("failed to list exporter pods: %v", err)
	}
	return pods.Items
}

// scrapeMetricsFromPod uses kubectl port-forward to reliably reach the pod
// metrics endpoint regardless of network topology (works in kind, minikube,
// remote clusters).
func (f *framework) scrapeMetricsFromPod(t *testing.T, pod corev1.Pod) string {
	t.Helper()

	ns := os.Getenv("E2E_EXPORTER_NAMESPACE")
	if ns == "" {
		ns = exporterNamespace
	}

	// Use kubectl port-forward with :0 to get a random local port
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		fmt.Sprintf("pod/%s", pod.Name),
		fmt.Sprintf(":9710"),
		"-n", ns,
	)
	if sharedKubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+sharedKubeconfig)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to create stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start kubectl port-forward: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Read the local port from kubectl output: "Forwarding from 127.0.0.1:XXXXX -> 9710"
	scanner := bufio.NewScanner(stdout)
	var localPort string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "Forwarding from") {
			// Parse "Forwarding from 127.0.0.1:12345 -> 9710"
			parts := strings.Split(line, ":")
			if len(parts) >= 2 {
				portAndRest := strings.Fields(parts[len(parts)-1])
				if len(portAndRest) > 0 {
					localPort = strings.Split(parts[1], " ")[0]
					// Extract port from "127.0.0.1:12345" or just "12345 -> ..."
					addrParts := strings.Split(strings.TrimSpace(parts[1]), " ")
					if len(addrParts) > 0 {
						localPort = addrParts[0]
					}
				}
			}
			break
		}
	}

	if localPort == "" {
		t.Fatalf("failed to parse local port from kubectl port-forward output for pod %s", pod.Name)
	}

	// Scrape metrics via the forwarded port
	client := &http.Client{Timeout: 10 * time.Second}
	metricsURL := fmt.Sprintf("http://127.0.0.1:%s/metrics", localPort)
	resp, err := client.Get(metricsURL)
	if err != nil {
		t.Fatalf("failed to scrape metrics from pod %s via port-forward at %s: %v", pod.Name, metricsURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read metrics response: %v", err)
	}
	return string(body)
}

func (f *framework) scrapeAllExporterMetrics(t *testing.T) map[string]string {
	t.Helper()
	pods := f.getExporterPods(t)
	if len(pods) == 0 {
		t.Fatal("no exporter pods found; is the DaemonSet deployed?")
	}

	results := make(map[string]string, len(pods))
	for i := range pods {
		if pods[i].Status.Phase != corev1.PodRunning {
			continue
		}
		results[pods[i].Spec.NodeName] = f.scrapeMetricsFromPod(t, pods[i])
	}
	return results
}

func (f *framework) getPodNode(t *testing.T, name string) string {
	t.Helper()
	pod, err := f.client.CoreV1().Pods(f.namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get pod %s: %v", name, err)
	}
	return pod.Spec.NodeName
}

func metricsContainVolumeHandle(metrics string, volumeHandle string) bool {
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "csiaddons_volume_node_device_info{") &&
			strings.Contains(line, fmt.Sprintf("volume_handle=\"%s\"", volumeHandle)) {
			return true
		}
	}
	return false
}

func metricsContainDevice(metrics string, volumeHandle string) (device string, found bool) {
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "csiaddons_volume_node_device_info{") &&
			strings.Contains(line, fmt.Sprintf("volume_handle=\"%s\"", volumeHandle)) {
			idx := strings.Index(line, "device=\"")
			if idx >= 0 {
				rest := line[idx+len("device=\""):]
				end := strings.Index(rest, "\"")
				if end >= 0 {
					return rest[:end], true
				}
			}
		}
	}
	return "", false
}
