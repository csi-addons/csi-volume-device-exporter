# csi-volume-device-exporter

[![GitHub release](https://img.shields.io/github/v/release/csi-addons/csi-volume-device-exporter)](https://github.com/csi-addons/csi-volume-device-exporter/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/csi-addons/csi-volume-device-exporter)](https://goreportcard.com/report/github.com/csi-addons/csi-volume-device-exporter)
[![CI](https://github.com/csi-addons/csi-volume-device-exporter/actions/workflows/test-golang.yaml/badge.svg)](https://github.com/csi-addons/csi-volume-device-exporter/actions/workflows/test-golang.yaml)
[![e2e](https://github.com/csi-addons/csi-volume-device-exporter/actions/workflows/test-e2e.yaml/badge.svg)](https://github.com/csi-addons/csi-volume-device-exporter/actions/workflows/test-e2e.yaml)
[![Lint](https://github.com/csi-addons/csi-volume-device-exporter/actions/workflows/golangci-lint.yaml/badge.svg)](https://github.com/csi-addons/csi-volume-device-exporter/actions/workflows/golangci-lint.yaml)

A Prometheus exporter that maps CSI volumes to their underlying node block devices. Enables correlating storage path health metrics (DM-multipath, NVMe-oF) with specific Kubernetes workloads.

## Problem

`node_exporter` metrics expose storage path health at the node level (`node_dmmultipath_path_state`, `node_nvmesubsystem_path_state`), but no metric exists to map a CSI volume (PV/PVC) to its underlying node block device. Without this mapping, operators cannot determine which workloads are impacted when a storage path degrades.

## How It Works

The exporter runs as a DaemonSet on every node, walks the kubelet pod directory tree to discover CSI volume-to-device mappings, and emits a single info metric:

```
csiaddons_volume_node_device_info{node="worker-1", volume_handle="csi-vol-id", driver="csi.trident.netapp.io", device="dm-5"} 1
```

### Discovery Strategy

- **Driver-specific JSON:** Reads Trident tracking files and HPE `deviceInfo.json` for direct volume-to-device mapping.
- **Universal fallback (filesystem volumes):** Walks kubelet pod directories (`pods/*/volumes/kubernetes.io~csi/*/`), reads `vol_data.json`, and uses `stat()` on the `mount/` subdirectory to resolve the block device via sysfs. A mount-propagation sanity check prevents false positives if `HostToContainer` propagation fails. Requires `mountPropagation: HostToContainer` on the kubelet volume mount.
- **Universal fallback (block volumes):** Walks `pods/*/volumeDevices/kubernetes.io~csi/*/`, `stat()`s the device file for `st_rdev`, and reads `vol_data.json` from the plugin staging path (`plugins/kubernetes.io~csi/volumeDevices/<specName>/data/`). Same sysfs resolution as filesystem volumes.

### Supported Drivers

Works with **any** CSI driver that stages volumes via kubelet, including:
- NetApp Trident (iSCSI/FC/NVMe-oF)
- Dell PowerStore, PowerFlex, Unity XT (iSCSI/FC/NVMe-oF)
- HPE CSI (iSCSI/FC)
- IBM Block CSI (FC/iSCSI)
- Any future CSI driver

## Quick Start

```bash
# Build
make build

# Run locally (for testing against a real kubelet root)
NODE_NAME=$(hostname) ./bin/csi-volume-device-exporter \
  --listen-address=:9710 \
  --poll-interval=30s \
  --host-sys=/sys \
  --kubelet-root=/var/lib/kubelet
```

### Kubernetes

```bash
kubectl apply -f deploy/daemonset.yaml
```

### OpenShift

The exporter must be scraped by the **platform Prometheus** (`openshift-monitoring`), not user
workload monitoring. This is required because the PromQL alert joins
`csiaddons_volume_node_device_info` with `node_dmmultipath_path_state` from `node-exporter`, and both
metrics must live in the same Prometheus instance for the join to work.

On OpenShift, the namespace must carry the label `openshift.io/cluster-monitoring=true` so
that platform Prometheus discovers the PodMonitor. The actual namespace is determined by the
deployer (e.g., cluster-storage-operator). For manual testing you can use the default
`csi-volume-device-exporter` namespace:

```bash
make deploy-openshift NAMESPACE=csi-volume-device-exporter
# or manually:
oc create namespace csi-volume-device-exporter
oc label namespace csi-volume-device-exporter openshift.io/cluster-monitoring=true
oc apply -n csi-volume-device-exporter -f deploy/scc.yaml
oc apply -n csi-volume-device-exporter -f deploy/daemonset.yaml
oc apply -n csi-volume-device-exporter -f deploy/podmonitor.yaml
```

> **Why not user workload monitoring?**
> If the exporter is deployed into a user workload namespace, its metrics land in a separate
> Prometheus instance. The PromQL join with `node_dmmultipath_path_state` (which is in the
> platform Prometheus) will always return empty.

## Configuration

| Flag | Default | Description |
|---|---|---|
| `--listen-address` | `:9710` | Address for metrics and healthz endpoints |
| `--poll-interval` | `30s` | Interval between discovery cycles |
| `--log-level` | `info` | Log level (debug, info, warn, error) |
| `--host-sys` | `/host/sys` | Path to host `/sys` mount inside the container |
| `--kubelet-root` | `/var/lib/kubelet` | Path to kubelet root (must have `mountPropagation: HostToContainer`) |
| `--host-trident-tracking` | `/host/trident/tracking` | Path to host Trident tracking dir inside the container |
| `--version` | — | Print version and exit |

| Environment Variable | Required | Description |
|---|---|---|
| `NODE_NAME` | Yes | Kubernetes node name (set via Downward API `spec.nodeName`) |
| `RUNBOOK_URL_TEMPLATE` | No | printf-style template for alert runbook URLs, e.g. `https://example.com/runbooks/%s.md` |

## Metrics and Alerts

The exporter exposes Prometheus metrics on the `/metrics` endpoint and ships
PrometheusRule alert definitions for storage path health correlation.

See [docs/metrics.md](docs/metrics.md) for the full metrics and alerts
reference, and [docs/runbooks/](docs/runbooks/) for alert runbooks.

Deploy the alert rules:

```bash
kubectl apply -n <namespace> -f pkg/monitoring/rules/alerts.yaml
```

## Security

The exporter runs as root (UID 0) with:
- `privileged: false`
- `allowPrivilegeEscalation: false`
- `capabilities.drop: [ALL]`
- `readOnlyRootFilesystem: true`
- `seccompProfile: RuntimeDefault`
- `seLinuxOptions.level: "s0"`
- No Kubernetes API server access (`automountServiceAccountToken: false`)
- All hostPath mounts are read-only
- JSON file reads limited to 1 MiB (prevents memory exhaustion)
- Path traversal protection (all paths must be absolute, no `..` allowed)
- Recursion depth-limited sysfs walks (prevents stack overflow)
- Distroless base image (`gcr.io/distroless/static`) — no shell, minimal attack surface
- Static binary (`CGO_ENABLED=0`)

Root is required because kubelet writes `vol_data.json` with `0600` permissions.

## Development

```bash
make build          # Build binary
make test           # Run unit tests with race detector
make test-e2e       # Run e2e tests (requires built binary)
make test-alerts    # Lint and unit-test Prometheus alert rules via promtool
make generate       # Regenerate alert YAML files from Go definitions
make lint           # Run golangci-lint
make vet            # Run go vet
make image          # Build container image
make push           # Push container image
make clean          # Remove build artifacts
```

### Repository Structure

```
cmd/exporter/           — binary entrypoint
pkg/
  discovery/            — volume-to-block-device mapping (kubelet, Trident, HPE)
  monitoring/
    metrics/            — Prometheus metric definitions
    rules/
      alerts/           — typed alert rule definitions (Go)
      alerts.yaml       — PrometheusRule CRD manifest (generated by make generate)
      rules.go          — PrometheusRule builder
deploy/                 — DaemonSet, PodMonitor, SCC manifests
docs/runbooks/          — alert runbooks
hack/prom-rule-ci/      — CI tooling: promtool lint + unit tests
tools/generate-rules/   — regenerates alert YAML from Go definitions
```

## Contributing

We welcome contributions! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## Community

- [CSI-Addons mailing list](https://groups.google.com/g/csi-addons)
- [GitHub Issues](https://github.com/csi-addons/csi-volume-device-exporter/issues)

## License

Apache License 2.0
