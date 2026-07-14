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

## Merge Automation

[Mergify](https://mergify.com) merges PRs automatically when:
- 2 approvals from project maintainers
- All CI checks pass (`lint`, `test`, `test-alerts`, `go_mod_verify`, `build-image`, `codespell`)
- No `DNM` label

## Image Publishing

| Workflow | Trigger | Image tag |
|----------|---------|-----------|
| `build-push.yaml` | Push to `main` | `quay.io/csiaddons/k8s-volume-device-exporter:latest` |
| `tag-release.yaml` | Git tag creation | `quay.io/csiaddons/k8s-volume-device-exporter:<tag>` |

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
