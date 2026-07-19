BINARY  := terraform-provider-busbar
VERSION ?= dev

.PHONY: generate docs build fmt vet test testacc

# Regenerate the API client from the vendored OpenAPI schema AND the registry docs.
generate:
	go generate ./...

# Regenerate only the Terraform Registry documentation under docs/.
docs:
	go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-name busbar

# Build the provider binary.
build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) .

fmt:
	gofmt -s -w .

vet:
	go vet ./...

# Unit tests (no live gateway required).
test:
	go test ./...

# Acceptance tests. Require a reachable gateway:
#   BUSBAR_ENDPOINT=... BUSBAR_ADMIN_TOKEN=... make testacc
testacc:
	TF_ACC=1 go test ./internal/provider/... -v -timeout 120m
