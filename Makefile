.PHONY: build test vet clean

build:
	mkdir -p bin
	go build -trimpath -o bin/idleloom ./cmd/idleloom
	go build -trimpath -o bin/idleloom-vulkan-dra ./cmd/dra-node

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin
