CONTROLLER_GEN_VERSION ?= v0.20.1
IDLECTL_VERSION ?= development
IDLECTL_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
IDLECTL_BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IDLECTL_LDFLAGS = -s -w -X main.version=$(IDLECTL_VERSION) -X main.commit=$(IDLECTL_COMMIT) -X main.buildDate=$(IDLECTL_BUILD_DATE)

.PHONY: build build-idlectl build-projection test vet generate-native clean

build: build-idlectl
	mkdir -p bin
	go build -trimpath -o bin/idleloom-vulkan-dra ./cmd/dra-node

build-idlectl:
	mkdir -p bin
	rm -f bin/idleloom bin/idleloom-controller bin/idleloom-agent bin/idleloom-link bin/idleloom-projection bin/idleloom-projection-linux
	go build -trimpath -ldflags "$(IDLECTL_LDFLAGS)" -o bin/idlectl ./cmd/idlectl

build-projection:
	mkdir -p bin
	rm -f bin/idlectl-linux bin/idleloom-projection-linux
	CGO_ENABLED=0 GOOS=linux go build -trimpath -o bin/idlectl-linux ./cmd/idlectl

test:
	go test ./...

vet:
	go vet ./...

generate-native:
	mkdir -p deploy/native/crds
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) object paths=./api/native/v1alpha1
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) crd:maxDescLen=0 paths=./api/native/v1alpha1 output:crd:artifacts:config=deploy/native/crds

clean:
	rm -rf bin
