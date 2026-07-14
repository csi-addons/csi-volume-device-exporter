//go:build e2e

// Package e2e provides end-to-end tests for the csi-volume-device-exporter.
//
// Prerequisites:
//   - A running Kubernetes/OpenShift cluster with a CSI driver
//   - The exporter DaemonSet deployed (make deploy-openshift or make deploy)
//   - KUBECONFIG set (defaults to ~/.kube/config)
//
// Optional environment variables:
//   - E2E_STORAGE_CLASS: StorageClass to use (defaults to cluster default)
//   - E2E_TEST_NAMESPACE: namespace for test workloads (default: csi-exporter-e2e-test)
//   - E2E_EXPORTER_NAMESPACE: namespace where exporter runs (default: csi-volume-device-exporter)
//   - E2E_SKIP_BLOCK: set to "true" to skip block volume tests (if CSI driver doesn't support it)
//   - E2E_MOCK_FS_HANDLE: volume handle of pre-created mock FS volume (for kind/CI)
//   - E2E_MOCK_FS_HANDLE_2: second mock volume handle (for multi-volume test)
//   - E2E_KIND_NODE: kind node container name (for volume removal test)
//
// Run: make test-e2e
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

func TestExporterDaemonSetRunning(t *testing.T) {
	f := newFramework(t)
	pods := f.getExporterPods(t)

	if len(pods) == 0 {
		t.Fatal("no exporter pods found; is the DaemonSet deployed?")
	}

	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodRunning {
			t.Errorf("exporter pod %s on node %s is %s (expected Running)",
				pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		}
	}
	t.Logf("found %d exporter pods running across cluster nodes", len(pods))
}

func TestExporterHealthEndpoint(t *testing.T) {
	f := newFramework(t)
	pods := f.getExporterPods(t)
	if len(pods) == 0 {
		t.Fatal("no exporter pods found")
	}

	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		metrics := f.scrapeMetricsFromPod(t, pod)
		if metrics == "" {
			t.Errorf("empty response from pod %s", pod.Name)
		}
	}
}

func TestOperationalMetricsPresent(t *testing.T) {
	f := newFramework(t)
	nodeMetrics := f.scrapeAllExporterMetrics(t)

	for node, metrics := range nodeMetrics {
		t.Run(node, func(t *testing.T) {
			// The timestamp metric is always present after a successful cycle
			if !strings.Contains(metrics, "csiaddons_volume_device_exporter_last_successful_discovery_timestamp_seconds") {
				t.Errorf("metric csiaddons_volume_device_exporter_last_successful_discovery_timestamp_seconds not found on node %s", node)
			}

			// If mock volumes were set up, the volumes_discovered gauge should be present
			if os.Getenv("E2E_MOCK_FS_HANDLE") != "" {
				if !strings.Contains(metrics, "csiaddons_volume_device_exporter_volumes_discovered") {
					t.Errorf("metric csiaddons_volume_device_exporter_volumes_discovered not found on node %s (expected with mock volumes)", node)
				}
			}
		})
	}
}

func TestFilesystemVolumeDiscovery(t *testing.T) {
	f := newFramework(t)

	mockHandle := os.Getenv("E2E_MOCK_FS_HANDLE")
	if mockHandle != "" {
		testMockFilesystemDiscovery(t, f, mockHandle)
		return
	}

	// PVC-based test for real clusters with block-device-backed CSI drivers
	testPVCFilesystemDiscovery(t, f)
}

func testMockFilesystemDiscovery(t *testing.T, f *framework, volumeHandle string) {
	t.Helper()
	t.Logf("using mock volume handle: %s", volumeHandle)

	err := wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods := f.getExporterPods(t)
			for _, p := range pods {
				if p.Status.Phase != corev1.PodRunning {
					continue
				}
				metrics := f.scrapeMetricsFromPod(t, p)
				if metricsContainVolumeHandle(metrics, volumeHandle) {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("metric for mock volume_handle=%s not found within %v", volumeHandle, defaultPollTimeout)
	}

	pods := f.getExporterPods(t)
	for _, p := range pods {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		metrics := f.scrapeMetricsFromPod(t, p)
		device, found := metricsContainDevice(metrics, volumeHandle)
		if !found {
			continue
		}
		if device == "" {
			t.Error("device label is empty for mock volume")
		}
		t.Logf("SUCCESS: mock volume_handle=%s mapped to device=%s", volumeHandle, device)

		if !strings.Contains(metrics, "driver=\"mock.csi.test\"") {
			t.Error("expected driver=mock.csi.test in metrics")
		}
	}
}

func testPVCFilesystemDiscovery(t *testing.T, f *framework) {
	t.Helper()

	pvcName := "e2e-fs-vol"
	podName := "e2e-fs-pod"

	f.createPVC(t, pvcName, corev1.PersistentVolumeFilesystem)
	f.createPodWithPVC(t, podName, pvcName, corev1.PersistentVolumeFilesystem)

	t.Cleanup(func() {
		f.deletePod(t, podName)
		f.waitForPodGone(t, podName)
		_ = f.client.CoreV1().PersistentVolumeClaims(f.namespace).Delete(
			context.TODO(), pvcName, metav1.DeleteOptions{})
	})

	f.waitForPVCBound(t, pvcName)
	f.waitForPodRunning(t, podName)

	pvc, err := f.client.CoreV1().PersistentVolumeClaims(f.namespace).Get(
		context.TODO(), pvcName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get PVC: %v", err)
	}
	pvName := pvc.Spec.VolumeName

	pv, err := f.client.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get PV %s: %v", pvName, err)
	}

	if pv.Spec.CSI == nil {
		t.Skipf("PV %s is not a CSI volume (possibly NFS/local), skipping", pvName)
	}
	volumeHandle := pv.Spec.CSI.VolumeHandle
	csiDriver := pv.Spec.CSI.Driver

	t.Logf("created filesystem PVC %s → PV %s (handle=%s, driver=%s)",
		pvcName, pvName, volumeHandle, csiDriver)

	node := f.getPodNode(t, podName)
	t.Logf("pod scheduled on node %s, waiting for metric discovery...", node)

	err = wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods := f.getExporterPods(t)
			for _, p := range pods {
				if p.Spec.NodeName != node || p.Status.Phase != corev1.PodRunning {
					continue
				}
				metrics := f.scrapeMetricsFromPod(t, p)
				if metricsContainVolumeHandle(metrics, volumeHandle) {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("metric for volume_handle=%s not found on node %s within %v",
			volumeHandle, node, defaultPollTimeout)
	}

	pods := f.getExporterPods(t)
	for _, p := range pods {
		if p.Spec.NodeName != node || p.Status.Phase != corev1.PodRunning {
			continue
		}
		metrics := f.scrapeMetricsFromPod(t, p)
		device, found := metricsContainDevice(metrics, volumeHandle)
		if !found {
			t.Fatalf("volume_handle=%s metric disappeared", volumeHandle)
		}
		if device == "" {
			t.Error("device label is empty")
		}
		t.Logf("SUCCESS: volume_handle=%s mapped to device=%s on node=%s", volumeHandle, device, node)

		for _, line := range strings.Split(metrics, "\n") {
			if strings.Contains(line, fmt.Sprintf("volume_handle=\"%s\"", volumeHandle)) {
				if !strings.Contains(line, fmt.Sprintf("driver=\"%s\"", csiDriver)) {
					t.Errorf("expected driver=%s in metric line: %s", csiDriver, line)
				}
				break
			}
		}
	}
}

func TestMultipleVolumesOnSamePod(t *testing.T) {
	f := newFramework(t)

	mockHandle1 := os.Getenv("E2E_MOCK_FS_HANDLE")
	mockHandle2 := os.Getenv("E2E_MOCK_FS_HANDLE_2")

	if mockHandle1 != "" && mockHandle2 != "" {
		testMockMultipleVolumes(t, f, mockHandle1, mockHandle2)
		return
	}

	testPVCMultipleVolumes(t, f)
}

func TestVolumeRemoval(t *testing.T) {
	f := newFramework(t)

	kindNode := os.Getenv("E2E_KIND_NODE")
	mockHandle := os.Getenv("E2E_MOCK_FS_HANDLE_2")

	if kindNode != "" && mockHandle != "" {
		testMockVolumeRemoval(t, f, kindNode, mockHandle)
		return
	}

	testPVCVolumeRemoval(t, f)
}

func TestBlockVolumeDiscovery(t *testing.T) {
	if os.Getenv("E2E_SKIP_BLOCK") == "true" {
		t.Skip("E2E_SKIP_BLOCK=true, skipping block volume test")
	}

	// Block volume tests require a real CSI driver that provisions actual
	// block devices. The hostpath driver used in kind creates files, not
	// real device nodes, so block discovery can't be validated there.
	if os.Getenv("E2E_MOCK_FS_HANDLE") != "" {
		t.Skip("block volume tests require a real block-device-backed CSI driver; skipping in mock/kind environment")
	}

	f := newFramework(t)

	pvcName := "e2e-block-vol"
	podName := "e2e-block-pod"

	f.createPVC(t, pvcName, corev1.PersistentVolumeBlock)
	f.createPodWithPVC(t, podName, pvcName, corev1.PersistentVolumeBlock)

	t.Cleanup(func() {
		f.deletePod(t, podName)
		f.waitForPodGone(t, podName)
		_ = f.client.CoreV1().PersistentVolumeClaims(f.namespace).Delete(
			context.TODO(), pvcName, metav1.DeleteOptions{})
	})

	f.waitForPVCBound(t, pvcName)
	f.waitForPodRunning(t, podName)

	pvc, err := f.client.CoreV1().PersistentVolumeClaims(f.namespace).Get(
		context.TODO(), pvcName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get PVC: %v", err)
	}
	pvName := pvc.Spec.VolumeName

	pv, err := f.client.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get PV %s: %v", pvName, err)
	}

	if pv.Spec.CSI == nil {
		t.Skipf("PV %s is not a CSI volume, skipping", pvName)
	}
	volumeHandle := pv.Spec.CSI.VolumeHandle
	csiDriver := pv.Spec.CSI.Driver

	t.Logf("created block PVC %s → PV %s (handle=%s, driver=%s)",
		pvcName, pvName, volumeHandle, csiDriver)

	node := f.getPodNode(t, podName)
	t.Logf("pod scheduled on node %s, waiting for metric discovery...", node)

	err = wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods := f.getExporterPods(t)
			for _, p := range pods {
				if p.Spec.NodeName != node || p.Status.Phase != corev1.PodRunning {
					continue
				}
				metrics := f.scrapeMetricsFromPod(t, p)
				if metricsContainVolumeHandle(metrics, volumeHandle) {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("metric for block volume_handle=%s not found on node %s within %v",
			volumeHandle, node, defaultPollTimeout)
	}

	pods := f.getExporterPods(t)
	for _, p := range pods {
		if p.Spec.NodeName != node || p.Status.Phase != corev1.PodRunning {
			continue
		}
		metrics := f.scrapeMetricsFromPod(t, p)
		device, found := metricsContainDevice(metrics, volumeHandle)
		if !found {
			t.Fatalf("volume_handle=%s metric disappeared", volumeHandle)
		}
		t.Logf("SUCCESS: block volume_handle=%s mapped to device=%s on node=%s", volumeHandle, device, node)
	}
}

func testMockMultipleVolumes(t *testing.T, f *framework, handle1, handle2 string) {
	t.Helper()
	t.Logf("verifying both mock volumes are discovered: %s, %s", handle1, handle2)

	err := wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods := f.getExporterPods(t)
			for _, p := range pods {
				if p.Status.Phase != corev1.PodRunning {
					continue
				}
				metrics := f.scrapeMetricsFromPod(t, p)
				if metricsContainVolumeHandle(metrics, handle1) &&
					metricsContainVolumeHandle(metrics, handle2) {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("not all mock volume metrics appeared within %v", defaultPollTimeout)
	}
	t.Logf("SUCCESS: both mock volumes discovered")
}

func testPVCMultipleVolumes(t *testing.T, f *framework) {
	t.Helper()

	pvc1Name := "e2e-multi-vol-1"
	pvc2Name := "e2e-multi-vol-2"
	podName := "e2e-multi-pod"

	f.createPVC(t, pvc1Name, corev1.PersistentVolumeFilesystem)
	f.createPVC(t, pvc2Name, corev1.PersistentVolumeFilesystem)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: f.namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "test",
					Image:   "registry.k8s.io/pause:3.9",
					Command: []string{"/pause"},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "vol-1", MountPath: "/mnt/vol1"},
						{Name: "vol-2", MountPath: "/mnt/vol2"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "vol-1",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvc1Name,
						},
					},
				},
				{
					Name: "vol-2",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvc2Name,
						},
					},
				},
			},
		},
	}

	_, err := f.client.CoreV1().Pods(f.namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	t.Cleanup(func() {
		f.deletePod(t, podName)
		f.waitForPodGone(t, podName)
		_ = f.client.CoreV1().PersistentVolumeClaims(f.namespace).Delete(
			context.TODO(), pvc1Name, metav1.DeleteOptions{})
		_ = f.client.CoreV1().PersistentVolumeClaims(f.namespace).Delete(
			context.TODO(), pvc2Name, metav1.DeleteOptions{})
	})

	f.waitForPVCBound(t, pvc1Name)
	f.waitForPVCBound(t, pvc2Name)
	f.waitForPodRunning(t, podName)

	var handles []string
	for _, pvcName := range []string{pvc1Name, pvc2Name} {
		pvc, err := f.client.CoreV1().PersistentVolumeClaims(f.namespace).Get(
			context.TODO(), pvcName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get PVC %s: %v", pvcName, err)
		}
		pv, err := f.client.CoreV1().PersistentVolumes().Get(context.TODO(), pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get PV: %v", err)
		}
		if pv.Spec.CSI == nil {
			t.Skip("PV is not CSI-backed")
		}
		handles = append(handles, pv.Spec.CSI.VolumeHandle)
	}

	node := f.getPodNode(t, podName)
	t.Logf("pod on node %s with volume handles: %v", node, handles)

	err = wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods := f.getExporterPods(t)
			for _, p := range pods {
				if p.Spec.NodeName != node || p.Status.Phase != corev1.PodRunning {
					continue
				}
				metrics := f.scrapeMetricsFromPod(t, p)
				allFound := true
				for _, h := range handles {
					if !metricsContainVolumeHandle(metrics, h) {
						allFound = false
						break
					}
				}
				if allFound {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("not all volume metrics appeared within %v", defaultPollTimeout)
	}
	t.Logf("SUCCESS: both volumes discovered on node %s", node)
}

func testMockVolumeRemoval(t *testing.T, f *framework, kindNode, volumeHandle string) {
	t.Helper()
	t.Logf("testing volume removal with mock handle=%s on node=%s", volumeHandle, kindNode)

	// Verify the mock volume is discovered
	err := wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods := f.getExporterPods(t)
			for _, p := range pods {
				if p.Status.Phase != corev1.PodRunning {
					continue
				}
				metrics := f.scrapeMetricsFromPod(t, p)
				if metricsContainVolumeHandle(metrics, volumeHandle) {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("metric never appeared for mock volume %s", volumeHandle)
	}
	t.Logf("metric appeared for volume_handle=%s, now removing mock volume...", volumeHandle)

	// Remove the mock volume by unmounting and cleaning up
	cmd := exec.Command("docker", "exec", kindNode, "bash", "-c",
		`umount /var/lib/kubelet/pods/e2e-00000000-0000-0000-0000-111111111111/volumes/kubernetes.io~csi/e2e-mock-fs-pv2/mount && `+
			`rm -rf /var/lib/kubelet/pods/e2e-00000000-0000-0000-0000-111111111111/volumes/kubernetes.io~csi/e2e-mock-fs-pv2`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to remove mock volume: %v: %s", err, string(out))
	}

	// Wait for the metric to disappear (next reconcile cycle)
	err = wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, 90*time.Second, true,
		func(ctx context.Context) (bool, error) {
			pods := f.getExporterPods(t)
			for _, p := range pods {
				if p.Status.Phase != corev1.PodRunning {
					continue
				}
				metrics := f.scrapeMetricsFromPod(t, p)
				if !metricsContainVolumeHandle(metrics, volumeHandle) {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("metric for volume_handle=%s still present after removal (waited 90s)", volumeHandle)
	}
	t.Logf("SUCCESS: metric for volume_handle=%s disappeared after removal", volumeHandle)
}

func testPVCVolumeRemoval(t *testing.T, f *framework) {
	t.Helper()

	pvcName := "e2e-removal-vol"
	podName := "e2e-removal-pod"

	f.createPVC(t, pvcName, corev1.PersistentVolumeFilesystem)
	f.createPodWithPVC(t, podName, pvcName, corev1.PersistentVolumeFilesystem)

	t.Cleanup(func() {
		f.deletePod(t, podName)
		f.waitForPodGone(t, podName)
		_ = f.client.CoreV1().PersistentVolumeClaims(f.namespace).Delete(
			context.TODO(), pvcName, metav1.DeleteOptions{})
	})

	f.waitForPVCBound(t, pvcName)
	f.waitForPodRunning(t, podName)

	pvc, err := f.client.CoreV1().PersistentVolumeClaims(f.namespace).Get(
		context.TODO(), pvcName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get PVC: %v", err)
	}
	pvName := pvc.Spec.VolumeName

	pv, err := f.client.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get PV: %v", err)
	}
	if pv.Spec.CSI == nil {
		t.Skip("PV is not CSI-backed")
	}
	volumeHandle := pv.Spec.CSI.VolumeHandle
	node := f.getPodNode(t, podName)

	err = wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods := f.getExporterPods(t)
			for _, p := range pods {
				if p.Spec.NodeName != node || p.Status.Phase != corev1.PodRunning {
					continue
				}
				metrics := f.scrapeMetricsFromPod(t, p)
				if metricsContainVolumeHandle(metrics, volumeHandle) {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("metric never appeared for volume %s", volumeHandle)
	}
	t.Logf("metric appeared for volume_handle=%s, now deleting pod...", volumeHandle)

	f.deletePod(t, podName)
	f.waitForPodGone(t, podName)

	err = wait.PollUntilContextTimeout(context.TODO(), defaultPollPeriod, defaultPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods := f.getExporterPods(t)
			for _, p := range pods {
				if p.Spec.NodeName != node || p.Status.Phase != corev1.PodRunning {
					continue
				}
				metrics := f.scrapeMetricsFromPod(t, p)
				if !metricsContainVolumeHandle(metrics, volumeHandle) {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("metric for volume_handle=%s still present after pod deletion (waited %v)",
			volumeHandle, defaultPollTimeout)
	}
	t.Logf("SUCCESS: metric for volume_handle=%s disappeared after pod deletion", volumeHandle)
}

func TestNetworkFSNotExposed(t *testing.T) {
	f := newFramework(t)

	nfsServer := os.Getenv("E2E_NFS_SERVER")
	nfsPath := os.Getenv("E2E_NFS_PATH")
	if nfsServer == "" || nfsPath == "" {
		t.Skip("E2E_NFS_SERVER and E2E_NFS_PATH not set; skipping NFS exclusion test")
	}

	pvName := "e2e-nfs-pv"
	pvcName := "e2e-nfs-pvc"
	podName := "e2e-nfs-pod"

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: *mustParseQuantity("1Gi"),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{
					Server: nfsServer,
					Path:   nfsPath,
				},
			},
		},
	}

	_, err := f.client.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create NFS PV: %v", err)
	}
	t.Cleanup(func() {
		_ = f.client.CoreV1().PersistentVolumes().Delete(context.TODO(), pvName, metav1.DeleteOptions{})
	})

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: f.namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *mustParseQuantity("1Gi"),
				},
			},
			VolumeName: pvName,
		},
	}

	_, err = f.client.CoreV1().PersistentVolumeClaims(f.namespace).Create(context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create NFS PVC: %v", err)
	}
	t.Cleanup(func() {
		_ = f.client.CoreV1().PersistentVolumeClaims(f.namespace).Delete(
			context.TODO(), pvcName, metav1.DeleteOptions{})
	})

	f.createPodWithPVC(t, podName, pvcName, corev1.PersistentVolumeFilesystem)
	t.Cleanup(func() {
		f.deletePod(t, podName)
		f.waitForPodGone(t, podName)
	})

	f.waitForPodRunning(t, podName)

	time.Sleep(70 * time.Second)

	node := f.getPodNode(t, podName)
	pods := f.getExporterPods(t)
	for _, p := range pods {
		if p.Spec.NodeName != node || p.Status.Phase != corev1.PodRunning {
			continue
		}
		metrics := f.scrapeMetricsFromPod(t, p)
		for _, line := range strings.Split(metrics, "\n") {
			if strings.HasPrefix(line, "csiaddons_volume_node_device_info{") &&
				strings.Contains(line, pvName) {
				t.Errorf("NFS volume %s should NOT produce a device_info metric, but found: %s", pvName, line)
			}
		}
	}
	t.Logf("SUCCESS: NFS volume not exposed as device_info metric (as expected)")
}
