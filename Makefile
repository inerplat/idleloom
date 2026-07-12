CONTROLLER_GEN_VERSION ?= v0.20.1

.PHONY: build build-idlectl build-projection test vet generate-native clean

build: build-idlectl
	mkdir -p bin
	go build -trimpath -o bin/idleloom ./cmd/idleloom
	go build -trimpath -o bin/idleloom-vulkan-dra ./cmd/dra-node

build-idlectl:
	mkdir -p bin
	go build -trimpath -o bin/idlectl ./cmd/idlectl
	cp bin/idlectl bin/idleloom-controller
	cp bin/idlectl bin/idleloom-agent
	cp bin/idlectl bin/idleloom-link
	cp bin/idlectl bin/idleloom-projection

build-projection:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux go build -trimpath -o bin/idleloom-projection-linux ./cmd/idlectl

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
