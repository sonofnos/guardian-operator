GO ?= /usr/local/go/bin/go
IMG ?= guardian-operator:dev
CONTROLLER_GEN_VERSION ?= v0.19.0

.PHONY: build
build:
	$(GO) build -o bin/manager ./cmd

.PHONY: run
run:
	$(GO) run ./cmd

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test: vet
	$(GO) test ./... -count=1

.PHONY: test-e2e
test-e2e:
	./hack/e2e.sh

.PHONY: manifests
manifests:
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		crd paths="./api/..." output:crd:artifacts:config=config/crd/bases
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		rbac:roleName=guardian-operator-manager-role paths="./internal/..." output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate:
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		object:headerFile="" paths="./api/..."

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: fmt
fmt:
	$(GO) fmt ./...
