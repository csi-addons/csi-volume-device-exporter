# Contribution Guidelines

Thank you for your interest in contributing to the CSI Volume Device Exporter!

## Contributing changes

This project uses a [GitHub workflow][github_pr] for contributions. Changes are
sent as Pull Requests from a forked repository.

## Reviewing Pull Requests

All Pull Requests require at least two approvals from project maintainers before
they can be merged.

## Merging Pull Requests

After a Pull Request has been reviewed and approved, it will be merged
automatically by the Mergify bot (@mergifyio).

## Development

```bash
# Build
make build

# Run unit tests
make test

# Run alert rule tests (requires podman or docker)
make test-alerts

# Run end-to-end tests (requires a running cluster with exporter deployed)
make test-e2e

# Regenerate alert YAML from Go definitions (run after editing pkg/monitoring/rules/alerts/)
make generate

# Lint
make lint

# Build container image
make image
```

### Metrics and alert changes

When modifying metrics or alert rules, always run `make generate` and commit
the updated generated files (`pkg/monitoring/rules/alerts.yaml` and
`docs/metrics.md`). CI will fail if generated files drift from the Go source.

## Commit messages

Please use clear, concise commit messages. If the change addresses a specific
issue, reference it in the commit body (e.g., `Fixes #123`).

[github_pr]: https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/proposing-changes-to-your-work-with-pull-requests/creating-a-pull-request
