.PHONY: fmt fmt-check lint vet mod-verify test test-race vuln integration build docker-build docker-build-multiarch verify

ENVOY_IMAGE := envoyproxy/envoy:v1.38.0@sha256:8146b97ee61a42cd216514709e4e3198af75f014974e3d9f310aef9c901fcbdf
GOFUMPT := go run mvdan.cc/gofumpt@v0.11.0
GOVULNCHECK := go run golang.org/x/vuln/cmd/govulncheck@v1.6.0

fmt:
	$(GOFUMPT) -w ./cmd ./deploy ./integration ./internal

fmt-check:
	test -z "$$($(GOFUMPT) -l ./cmd ./deploy ./integration ./internal)"

lint:
	golangci-lint run

vet:
	go vet -all ./...

mod-verify:
	go mod verify

test:
	go test ./...

test-race:
	go test -race ./...

vuln:
	$(GOVULNCHECK) ./...

integration:
	docker pull $(ENVOY_IMAGE)
	RUN_ENVOY_INTEGRATION=1 go test -v -count=1 ./integration

build:
	CGO_ENABLED=0 go build -mod=readonly -trimpath -o bin/sablier-extproc ./cmd/sablier-extproc

docker-build:
	docker build --tag sablier-extproc:dev .

docker-build-multiarch:
	mkdir -p bin
	docker buildx build --platform linux/amd64,linux/arm64 --output type=oci,dest=bin/sablier-extproc.tar .

verify: fmt-check lint vet mod-verify test test-race vuln
