.PHONY: tools ui build test test-short lint serve run bench bench-baseline cluster docker clean

TOOLS := $(CURDIR)/.tools/bin

# Pinned tool versions. go install checks every module against the Go
# checksum database, so a pinned version always builds the same tool.
BUF_VERSION             := v1.75.0
PROTOC_GEN_GO_VERSION   := v1.36.12
PROTOC_GEN_GRPC_VERSION := v1.6.2
GOLANGCI_LINT_VERSION   := v2.15.0
STATICCHECK_VERSION     := v0.8.2
GOVULNCHECK_VERSION     := v1.10.0

tools: ## install the pinned toolchain into .tools/bin
	GOBIN=$(TOOLS) go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	GOBIN=$(TOOLS) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(TOOLS) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GRPC_VERSION)
	GOBIN=$(TOOLS) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	GOBIN=$(TOOLS) go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	GOBIN=$(TOOLS) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

ui: ## rebuild the web UI (React + shadcn/ui) into web/static, which the binary embeds
	cd web/ui && npm ci && npm run build

build:
	go build -trimpath -o .tools/fliessband ./cmd/fliessband

test: ## everything, including the line with a fake LLM (needs `make tools`)
	go test -race -timeout 20m ./...

test-short: ## unit tests only
	go test -short ./...

lint:
	go vet ./...
	$(TOOLS)/golangci-lint run ./...

serve: build
	./.tools/fliessband serve

run: build ## make run SPEC=specs/inventory.yaml
	./.tools/fliessband run $(SPEC)

bench: build ## benchmark the factory (template-only, all example specs) against bench/baseline.json
	./.tools/fliessband bench -specs 'specs/*.yaml' -out data/bench/results.json -baseline bench/baseline.json

bench-baseline: build ## rerun the template-only benchmark and make it the new baseline
	./.tools/fliessband bench -specs 'specs/*.yaml' -out bench/baseline.json

cluster: ## a local k3d cluster that fliessband deploys to (needs k3d and Docker)
	k3d cluster create fliessband --agents 1 --k3s-arg "--disable=traefik@server:0" --wait

docker:
	docker build -t fliessband:dev .

clean:
	rm -rf data/runs
