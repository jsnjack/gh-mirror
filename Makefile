BINARY := gh-mirror
export PATH := $(PATH):$(shell go env GOPATH)/bin
.NOTPARALLEL:

.PHONY: build check agents-size fmt vet test duplicates lint standards release
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
# Real duplicate pairs, anonymized; the race detector makes this fixture take minutes.
# TestSearchGolden keeps full search results over the same fixture byte-identical.
# Separate processes avoid lock contention inside the SQLite driver, which makes
# concurrent fixture tests in one process about twice as slow.
duplicates:
	@go test -count=1 -run '^TestDuplicateRegression$$' -v ./internal/store & duplicates=$$!; \
	go test -count=1 -run '^TestSearchGolden$$/^cache=true$$' ./internal/store & cached=$$!; \
	go test -count=1 -run '^TestSearchGolden$$/^cache=false$$' ./internal/store & direct=$$!; \
	status=0; for job in $$duplicates $$cached $$direct; do wait $$job || status=1; done; exit $$status
lint:
	@command -v golangci-lint >/dev/null || { echo 'Install golangci-lint: grm install golangci/golangci-lint'; exit 1; }
	golangci-lint run
# Every agent session loads AGENTS.md whole, and Codex reads only its first
# 32 KiB: keep it under the limit (AGENTS.universal.md, "AGENTS.md size").
AGENTS_MAX_BYTES ?= 30000
agents-size:
	@size=$$(wc -c < AGENTS.md); \
	if [ "$$size" -gt $(AGENTS_MAX_BYTES) ]; then \
	  echo "AGENTS.md is $$size bytes, over the $(AGENTS_MAX_BYTES)-byte limit: move detail to docs/ (AGENTS.universal.md, \"AGENTS.md size\")"; \
	  exit 1; \
	fi

check: fmt vet build test duplicates lint agents-size
# grm creates the v<monova> tag on GitHub, so HEAD must already be pushed.
release: build
	@command -v grm >/dev/null || { echo 'Install grm: https://github.com/jsnjack/grm'; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo 'Commit changes before releasing'; exit 1; }
	@git fetch --quiet origin && test "$$(git rev-parse HEAD)" = "$$(git rev-parse @{u})" || { echo 'Push HEAD before releasing'; exit 1; }
	./scripts/package.sh "$$(monova)"
	grm release jsnjack/$(BINARY) \
		-f dist/$(BINARY)_linux_amd64.tar.gz \
		-f dist/$(BINARY)_linux_arm64.tar.gz \
		-f dist/$(BINARY)_darwin_amd64.tar.gz \
		-f dist/$(BINARY)_darwin_arm64.tar.gz \
		-f dist/checksums.txt \
		-t "v$$(monova)"
standards:
	@for name in AGENTS.universal.md AGENTS.go.md; do \
	  curl --fail --silent --show-error --location "https://raw.githubusercontent.com/jsnjack/standards/master/$$name" -o "$$name.tmp" && mv "$$name.tmp" "$$name" || exit $$?; \
	done
