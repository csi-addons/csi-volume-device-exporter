# CSIAddonsVolumeDeviceExporterDown

## Meaning

No `csi-volume-device-exporter` targets have been scraped by Prometheus for at
least 5 minutes. The exporter DaemonSet has either been removed from the cluster
or all of its pods are failing to start or stay running.

## Impact

Storage path health for CSI volumes cannot be reported. The
`CSIAddonsVolumeMultipathDegraded` alert will not fire even if multipath paths are
faulty, creating a blind spot in storage observability.

## Diagnosis

1. Check the DaemonSet status:

   ```bash
   kubectl get daemonset csi-volume-device-exporter -n <namespace>
   ```

   Look for pods that are not in the `Running` state.

2. Identify failing pods and inspect their logs:

   ```bash
   kubectl get pods -n <namespace> -l app.kubernetes.io/name=csi-volume-device-exporter
   kubectl logs -n <namespace> <pod-name> --previous
   ```

3. Check pod events for scheduling or mount failures:

   ```bash
   kubectl describe pod -n <namespace> <pod-name>
   ```

4. Verify the DaemonSet is present and not accidentally deleted:

   ```bash
   kubectl get daemonset -n <namespace>
   ```

5. Confirm Prometheus can reach the exporter endpoint:

   ```bash
   kubectl get podmonitor -n <namespace> csi-volume-device-exporter
   ```

## Mitigation

- If pods are crashlooping, review the logs from step 2 above for startup
  errors (missing `NODE_NAME`, inaccessible host paths, permission errors).

- If pods are in `Pending` state, a node may lack resources or have a taint
  that prevents scheduling. Review `kubectl describe node <node>`.

- If the DaemonSet was deleted, redeploy it:

  ```bash
  kubectl apply -f deploy/daemonset.yaml
  ```

- On OpenShift, also verify the SecurityContextConstraint is still bound:

  ```bash
  oc get scc csi-volume-device-exporter
  oc adm policy who-can use scc csi-volume-device-exporter
  ```

The alert will clear automatically once Prometheus successfully scrapes at least
one exporter target.

---

# CSIAddonsVolumeDeviceExporterNodeDown

## Meaning

A single `csi-volume-device-exporter` pod on a specific node has not been
scraped by Prometheus for at least 10 minutes. Unlike `CSIAddonsVolumeDeviceExporterDown`
(which fires when *all* targets are missing), this alert fires for individual
node failures while other nodes remain healthy.

## Impact

Storage path health for CSI volumes **on the affected node** cannot be reported.
Workloads running on that node will have no multipath/NVMe degradation alerts.

## Diagnosis

1. Identify the failing node from the alert labels (`instance`).

2. Check the pod status on that node:

   ```bash
   kubectl get pods -n <namespace> -l app.kubernetes.io/name=csi-volume-device-exporter \
     --field-selector spec.nodeName=<node>
   ```

3. If the pod exists but is not ready, check its logs and events:

   ```bash
   kubectl logs -n <namespace> <pod-name>
   kubectl describe pod -n <namespace> <pod-name>
   ```

4. If the pod does not exist, the DaemonSet may have a scheduling issue on
   that specific node (taints, resource pressure, or node cordoned).

## Mitigation

- If the pod is crashlooping, review logs for startup errors (permission
  denied on `/var/lib/kubelet`, SELinux denials, missing `NODE_NAME`).

- If the node is cordoned or has a NoSchedule taint, uncordon it or add a
  toleration to the DaemonSet.

- If the node is healthy but the pod is missing, delete the DaemonSet pod on
  another node to force a reschedule check, or restart the DaemonSet:

  ```bash
  kubectl rollout restart daemonset/csi-volume-device-exporter -n <namespace>
  ```

The alert will clear automatically once Prometheus successfully scrapes the
exporter target on the affected node.
