# CI Workflows

This document describes the GitHub Actions CI/CD pipelines for the project.

## Pull Request Checks

| Workflow | Job(s) | What it does |
|----------|--------|-------------|
| `golangci-lint.yaml` | `lint` | Runs `golangci-lint` with 15m timeout |
| `test-golang.yaml` | `test`, `test-alerts`, `go_mod_verify` | Unit tests, `go vet`, alert rule tests (promtool), `go mod verify` |
| `test-build.yaml` | `build-image` | Builds the container image (no push) to verify Dockerfile |
| `yamllint.yaml` | `yamllint` | Lints YAML files using `.yamllint.yaml` config |
| `codespell.yml` | `codespell` | Catches spelling mistakes |

## End-to-End Tests

| Workflow | Trigger | What it does |
|----------|---------|-------------|
| `test-e2e.yaml` | Push to `main`, PRs to `main`, manual (`workflow_dispatch`) | Spins up a kind cluster with CSI hostpath driver, deploys the exporter, and runs the full e2e suite |

The e2e suite (`test/e2e/`) validates:
- Exporter DaemonSet runs on all nodes
- Filesystem volume discovery (PVC → metric with correct labels)
- Block volume discovery
- Volume removal (metric disappears after pod deletion)
- Multiple volumes on a single pod
- Network filesystem exclusion (NFS produces no metric)
- Operational metrics always present

Run locally: `make test-e2e` (requires a running cluster with the exporter deployed and CSI volumes).

Environment variables:
- `E2E_STORAGE_CLASS` — StorageClass to use (default: cluster default)
- `E2E_TEST_NAMESPACE` — namespace for test workloads (default: `csi-exporter-e2e-test`)
- `E2E_EXPORTER_NAMESPACE` — namespace where exporter runs (default: `csi-volume-device-exporter`)
- `E2E_SKIP_BLOCK` — set to `true` to skip block volume tests
- `E2E_NFS_SERVER` / `E2E_NFS_PATH` — for NFS exclusion test

## Merge Automation

[Mergify](https://mergify.com) merges PRs automatically when:
- 2 approvals from project maintainers
- All CI checks pass (`lint`, `test`, `test-alerts`, `go_mod_verify`, `build-image`, `codespell`, `e2e`)
- No `DNM` label

## Image Publishing

| Workflow | Trigger | Image tag |
|----------|---------|-----------|
| `build-push.yaml` | Push to `main` | `quay.io/csiaddons/csi-volume-device-exporter:latest` |
| `tag-release.yaml` | Git tag creation | `quay.io/csiaddons/csi-volume-device-exporter:<tag>` |

Both require `QUAY_USERNAME` and `QUAY_PASSWORD` repository secrets.

## Releases

When a git tag is created (e.g., `v0.1.0`):

1. `tag-release.yaml` builds a multi-arch image (`linux/amd64`, `linux/arm64`) and pushes to Quay
2. A GitHub Release is created with generated release notes
3. Deploy manifests (`deploy/*.yaml`) are attached as release artifacts

## Dependency Updates

Dependabot is configured (`.github/dependabot.yml`) to propose weekly PRs for:
- Go module dependencies (grouped by org)
- GitHub Actions version bumps
