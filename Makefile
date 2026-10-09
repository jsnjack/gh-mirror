BINARY := gh-mirror
export PATH := $(PATH):$(shell go env GOPATH)/bin
.NOTPARALLEL:

.PHONY: build check fmt vet test lint standards
build:
	@command -v monova >/dev/null || { echo 'Install monova: grm install jsnjack/monova'; exit 1; }
	@mkdir -p bin
	@version=dev; if git rev-parse --verify HEAD >/dev/null 2>&1; then version=$$(monova) || exit $$?; fi; \
	CGO_ENABLED=0 go build -trimpath -ldflags="-X gh-mirror/cmd.Version=$$version" -o bin/$(BINARY) .
fmt:
	@command -v goimports >/dev/null || { echo 'Install goimports: go install golang.org/x/tools/cmd/goimports@latest'; exit 1; }
	goimports -w cmd internal main.go
vet:
	go vet ./...
test:
	go test -race ./...
lint:
	@command -v golangci-lint >/dev/null || { echo 'Install golangci-lint: grm install golangci/golangci-lint'; exit 1; }
	golangci-lint run
check: fmt vet build test lint
standards:
	@for name in AGENTS.universal.md AGENTS.go.md; do \
	  curl --fail --silent --show-error --location "https://raw.githubusercontent.com/jsnjack/standards/master/$$name" -o "$$name.tmp" && mv "$$name.tmp" "$$name" || exit $$?; \
	done
