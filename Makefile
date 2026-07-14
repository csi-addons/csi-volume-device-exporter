IMAGE_REPO   ?= quay.io/csiaddons/csi-volume-device-exporter
IMAGE_TAG    ?= latest
BINARY       := bin/csi-volume-device-exporter
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT       ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS      := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
CRI          ?= podman

.PHONY: all build generate test test-e2e test-alerts image push deploy deploy-podmonitor deploy-openshift undeploy undeploy-openshift clean lint vet fmt check-all-committed help

all: build

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/exporter

# Re-generate alert YAML files from the Go definitions in pkg/monitoring/rules/.
# Run this whenever alert rules change; commit the output alongside the code.
# Pass ALERT_NAMESPACE=<ns> to embed a real namespace in alerts.yaml, e.g.:
#   make generate ALERT_NAMESPACE=kubevirt
ALERT_NAMESPACE ?=
generate:
	go run ./tools/generate-rules -namespace=$(ALERT_NAMESPACE)
	go run ./tools/doc-generator

test:
	go test -race -count=1 ./pkg/... ./cmd/... ./tools/...

test-e2e: build
	go test -tags=e2e -v -timeout=10m ./test/e2e/

# Lint Prometheus rules and run unit tests.
# 1. Go tests: validate alert structure (required fields, runbook_url, etc.)
# 2. promtool: lint rule syntax and run scenario-based unit tests.
# Requires podman or docker (set CRI=docker if needed).
test-alerts:
	go test -race -count=1 ./pkg/monitoring/...
	hack/prom-rule-ci/verify-rules.sh

image:
	$(CRI) build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE_REPO):$(IMAGE_TAG) .

push: image
	$(CRI) push $(IMAGE_REPO):$(IMAGE_TAG)

deploy:
	kubectl apply -f deploy/daemonset.yaml

# Deploy the PodMonitor for Prometheus Operator-based clusters.
deploy-podmonitor:
	kubectl apply -f deploy/podmonitor.yaml

# Deploy to OpenShift with SCC.
# On OpenShift the actual namespace is determined by the deployer (e.g.
# cluster-storage-operator). The NAMESPACE variable below is a convenience
# default for manual testing.
NAMESPACE ?= csi-volume-device-exporter
deploy-openshift:
	kubectl create namespace $(NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	kubectl label namespace $(NAMESPACE) openshift.io/cluster-monitoring=true --overwrite
	kubectl apply -n $(NAMESPACE) -f deploy/scc.yaml
	kubectl apply -n $(NAMESPACE) -f deploy/daemonset.yaml
	kubectl apply -n $(NAMESPACE) -f deploy/podmonitor.yaml

undeploy:
	kubectl delete -f deploy/daemonset.yaml --ignore-not-found
	kubectl delete -f deploy/podmonitor.yaml --ignore-not-found

undeploy-openshift:
	kubectl delete -n $(NAMESPACE) -f deploy/podmonitor.yaml --ignore-not-found
	kubectl delete -n $(NAMESPACE) -f deploy/daemonset.yaml --ignore-not-found
	kubectl delete -n $(NAMESPACE) -f deploy/scc.yaml --ignore-not-found
	kubectl delete namespace $(NAMESPACE) --ignore-not-found

clean:
	rm -rf bin/

fmt:
	go fmt ./...

lint:
	golangci-lint run ./...

vet:
	go vet ./...

check-all-committed:
	test -z "$$(git status --short)" || (echo "files were modified:" ; git status --short ; false)

help:
	@echo "Targets:"
	@echo "  all                 Build the exporter binary (default)"
	@echo "  build               Build the exporter binary"
	@echo "  generate            Re-generate alert YAML from Go definitions"
	@echo "  test                Run unit tests"
	@echo "  test-e2e            Run end-to-end tests (requires running cluster + exporter deployed)"
	@echo "  test-alerts         Lint and test Prometheus alert rules"
	@echo "  image               Build container image"
	@echo "  push                Build and push container image"
	@echo "  deploy              Deploy DaemonSet to current cluster"
	@echo "  deploy-podmonitor   Deploy PodMonitor"
	@echo "  deploy-openshift    Deploy to OpenShift with SCC"
	@echo "  undeploy            Remove DaemonSet and PodMonitor"
	@echo "  undeploy-openshift  Remove all OpenShift resources including namespace"
	@echo "  clean               Remove build artifacts"
	@echo "  fmt                 Run go fmt"
	@echo "  lint                Run golangci-lint"
	@echo "  vet                 Run go vet"
	@echo "  check-all-committed Fail if there are uncommitted changes"
