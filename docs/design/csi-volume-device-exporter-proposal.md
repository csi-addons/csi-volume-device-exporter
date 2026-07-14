# CSI Volume Device Exporter — Design

## Summary

A per-node DaemonSet that maps CSI volumes to their underlying block devices, enabling correlation of storage path health metrics with Kubernetes workloads via Prometheus metric joins.

The exporter is a **standalone binary** — it does not communicate with CSI drivers via gRPC, does not use CRDs, and does not depend on the csi-addons operator. It reads host files and emits Prometheus metrics. Deployment on OpenShift is managed by cluster-storage-operator (CSO); on vanilla Kubernetes, `kubectl apply` is sufficient.

## Motivation

Prometheus `node_exporter` now includes collectors for DM-multipath path state ([#3581](https://github.com/prometheus/node_exporter/pull/3581)) and NVMe-oF subsystem health ([#3579](https://github.com/prometheus/node_exporter/pull/3579)). These expose storage path health at the **node level** — which device has degraded paths, on which node.

However, no metric exists to answer: **which workloads are affected?**

The CSI Volume Device Exporter provides the **join key** between Kubernetes volume identity and host block device identity as a Prometheus metric.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│  Prometheus                                             │
│                                                         │
│  csiaddons_volume_node_device_info{volume_handle="...", │
│    driver="csi.trident.netapp.io", device="dm-0"}       │
│         ┊                                               │
│         ┊ PromQL JOIN on (node, device/sysfs_name)      │
│         ┊                                               │
│  node_dmmultipath_path_state{node="worker-1",           │
│    sysfs_name="dm-0", path_state="faulty"}              │
│                                                         │
│  → Alert: CSIAddonsVolumeMultipathDegraded              │
│    "PV my-pv on worker-1 has degraded paths"            │
└─────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│  Per-Node DaemonSet: csi-volume-device-exporter         │
│                                                         │
│  Discoverers (ordered by precedence):                   │
│  1. Kubelet (authoritative):                            │
│  2. Trident — reads /var/lib/trident/tracking/*.json    │
│  3. HPE — reads deviceInfo.json from plugin dirs        │
│     a. Walk kubelet pod directories                     │
│     b. Read vol_data.json (CSI volume identity)         │
│     c. stat() mount point → device major:minor          │
│     d. Resolve via sysfs → kernel device name           │
│     e. Walk DM slaves if LUKS → find multipath device   │
│                                                         │
│  Kubelet runs first and wins on conflicts; optional     │
│  discoverers fill gaps only. Reconcile runs only when   │
│  kubelet discoverer succeeds.                           │
└─────────────────────────────────────────────────────────┘
```

## Discovery Strategy

### Filesystem Volumes (VolumeMode=Filesystem)

```
/var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~csi/<pv-name>/
├── vol_data.json    ← CSI volume identity (volumeHandle, driverName)
└── mount/           ← stat() this → st_dev gives device major:minor
```

1. Walk `pods/*/volumes/kubernetes.io~csi/*/`
2. Read `vol_data.json` for volume identity
3. `stat()` the `mount/` subdirectory → `st_dev` gives major:minor
4. Verify mount propagation (`st_dev` must differ from parent directory)
5. Resolve via `/sys/dev/block/{major}:{minor}` → kernel device name
6. For DM devices, walk slaves through LUKS to find multipath device

### Block Volumes (VolumeMode=Block)

```
/var/lib/kubelet/pods/<uid>/volumeDevices/kubernetes.io~csi/<pv-name>/
└── <pv-name>        ← device file; stat() → st_rdev gives major:minor

/var/lib/kubelet/plugins/kubernetes.io~csi/volumeDevices/<pv-name>/data/
└── vol_data.json    ← CSI volume identity
```

1. Walk `pods/*/volumeDevices/kubernetes.io~csi/*/`
2. `stat()` the device file → `st_rdev` gives major:minor
3. Read `vol_data.json` from the plugin staging path for volume identity
4. Same sysfs resolution as filesystem volumes

### Driver-Specific Discoverers

- **Trident**: Reads `/var/lib/trident/tracking/*.json` for direct volume-to-device mapping. Disabled automatically if the directory doesn't exist at startup.
- **HPE**: Reads `deviceInfo.json` from kubelet plugin directories.

### Network Filesystem Filtering

NFS, CephFS, and other network filesystems use pseudo-device numbers (major=0) that don't exist in `/sys/dev/block/`. Resolution fails naturally — no fragile filesystem-type list needed.

## Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `csiaddons_volume_node_device_info` | Gauge (1) | `node`, `volume_handle`, `driver`, `device` | Maps a CSI volume to its block device |
| `csiaddons_volume_device_exporter_discovery_errors_total` | Counter | `discoverer` | Discovery cycle errors by discoverer |
| `csiaddons_volume_device_exporter_volumes_discovered` | Gauge | `driver` | Number of volumes discovered per driver |
| `csiaddons_volume_device_exporter_last_successful_discovery_timestamp_seconds` | Gauge | — | Unix timestamp of last successful cycle |

## Alert Rules

| Alert | Severity | For | Condition |
|---|---|---|---|
| `CSIAddonsVolumeMultipathDegraded` | warning | 5m | PV-backed multipath device has non-active paths |
| `CSIAddonsVolumeMultipathLost` | critical | 1m | All multipath paths failed |
| `CSIAddonsVolumeNVMeSubsystemDegraded` | warning | 5m | NVMe-oF subsystem has non-live controllers |
| `CSIAddonsVolumeNVMeSubsystemLost` | critical | 1m | All NVMe-oF controllers dead |
| `CSIAddonsVolumeDeviceExporterDown` | warning | 5m | All exporter targets have disappeared |
| `CSIAddonsVolumeDeviceExporterNodeDown` | warning | 10m | Individual exporter target unreachable |

NVMe alerts join on `(subsystem, node)` to prevent cross-node false positives.

## Security Model

- Non-privileged container (`privileged: false`, no capabilities)
- `runAsUser: 0` (needed to read kubelet's `vol_data.json` with 0600 permissions)
- `readOnlyRootFilesystem: true`
- `seccompProfile: RuntimeDefault`
- `seLinuxOptions.level: "s0"`
- Read-only hostPath mounts: `/var/lib/kubelet` (with `mountPropagation: HostToContainer`), `/sys`
- No Kubernetes API access (`automountServiceAccountToken: false`)
- Distroless base image (no shell)
- Static binary (`CGO_ENABLED=0`)

## Deployment Lifecycle

The exporter is a standalone component hosted under the `csi-addons` organization. Deployment is managed externally:

- **OpenShift**: cluster-storage-operator (CSO) reconciles the DaemonSet, PrometheusRule, and PodMonitor as an atomic unit. The namespace must carry `openshift.io/cluster-monitoring=true` for platform Prometheus to scrape it.
- **Vanilla Kubernetes**: `kubectl apply -f deploy/` is sufficient. A PodMonitor (or ServiceMonitor) enables Prometheus Operator-based scraping.

## Relationship to Existing Work

- **node_exporter#3581** (merged) — `dmmultipath` collector exposing per-path state
- **node_exporter#3579** (merged) — `nvmesubsystem` collector exposing NVMe-oF health
- **csi-addons/kubernetes-csi-addons#1039** (closed) — previous attempt; closed because framing as a gRPC extension was an architectural mismatch
- **csi-addons/kubernetes-csi-addons#1062** — discussion issue proposing the standalone approach
