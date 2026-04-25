# ── Variables ─────────────────────────────────────────────────────────────────
BINARY      := netperf-api
IMAGE       ?= docker.io/loihoangthanh1411/netperf-api
TAG         ?= latest
KUBECONFIG  ?= $(HOME)/.kube/config

GO          := go
GOFLAGS     := -trimpath -ldflags="-s -w"
GOTEST      := $(GO) test -v -race

# ── Default ───────────────────────────────────────────────────────────────────
.DEFAULT_GOAL := help

.PHONY: help build docker-build docker-push test-unit test-e2e deploy clean

## help: List all available make targets with descriptions
help:
	@printf "\nUsage: make <target>\n\n"
	@grep -E '^## ' Makefile | awk 'BEGIN{FS=": "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' | sed 's/## //'
	@printf "\n"

# ── Build ─────────────────────────────────────────────────────────────────────

## build: Compile the Go binary for the current platform into bin/
build:
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -o bin/$(BINARY) ./cmd/
	@echo "✓  built bin/$(BINARY)"

## docker-build: Build the Docker image (IMAGE:TAG)
docker-build:
	docker build -t $(IMAGE):$(TAG) .
	@echo "✓  built $(IMAGE):$(TAG)"

## docker-push: Push the Docker image to the registry
docker-push: docker-build
	docker push $(IMAGE):$(TAG)
	@echo "✓  pushed $(IMAGE):$(TAG)"

# ── Tests ─────────────────────────────────────────────────────────────────────

## test-unit: Run pure unit tests (no cluster required, includes -race)
test-unit:
	$(GOTEST) ./internal/scheduler/... ./pkg/iperf3/...

## test-e2e: Run live Kubernetes integration tests (requires a reachable cluster)
##           Prerequisites: KUBECONFIG set, cluster-admin or equivalent RBAC.
##           The tests create + destroy a temporary namespace automatically.
test-e2e:
	KUBECONFIG=$(KUBECONFIG) \
	$(GO) test -v -tags integration -timeout 15m \
	    ./test/integration/...

# ── Deployment ────────────────────────────────────────────────────────────────

## deploy: Apply all Kubernetes manifests and wait for rollouts to complete
deploy:
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/daemonset.yaml
	kubectl apply -f deploy/deployment.yaml
	kubectl -n netperf-api rollout status daemonset/iperf3-server  --timeout=5m
	kubectl -n netperf-api rollout status deployment/netperf-api   --timeout=5m
	@echo "✓  netperf-api deployed and ready"

## undeploy: Remove all netperf resources from the cluster
undeploy:
	kubectl delete namespace netperf-api --ignore-not-found

# ── Utilities ─────────────────────────────────────────────────────────────────

## clean: Remove build artifacts
clean:
	rm -rf bin/
	@echo "✓  cleaned"

## port-forward: Forward the API service to localhost:8080
port-forward:
	kubectl -n netperf-api port-forward svc/netperf-api 8080:80

## logs: Tail the API server logs
logs:
	kubectl -n netperf-api logs -f deployment/netperf-api
