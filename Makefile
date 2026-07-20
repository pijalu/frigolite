# Frigolite Makefile
# Quality gates: go vet, staticcheck, gocognit, gocyclo
# CLI is a separate Go module at cmd/frigolite/

.PHONY: all build build-cli test test-race test-cover vet staticcheck gocognit
.PHONY: gocyclo lint quality ci clean bench shell demo-basic demo-bulk cross-build release vet-cli quality-cli

APP_NAME     := frigolite
CMD_DIR      := ./cmd/frigolite
GO_FLAGS     := -ldflags="-s -w"
OUTPUT_DIR   := ./build

# Default target: quality gates + tests + build CLI binary
all: quality test build-cli

# Build all packages (root library only)
build:
	go build ./...

# Build the frigolite CLI binary (separate module)
build-cli:
	@mkdir -p $(OUTPUT_DIR)
	cd $(CMD_DIR) && go build $(GO_FLAGS) -o ../../$(OUTPUT_DIR)/$(APP_NAME) .
	@echo "Built $(OUTPUT_DIR)/$(APP_NAME)"

# Cross-compile for multiple platforms
cross-build: build-cli
	@mkdir -p $(OUTPUT_DIR)
	cd $(CMD_DIR) && \
	  GOOS=linux   GOARCH=amd64 go build $(GO_FLAGS) -o ../../$(OUTPUT_DIR)/$(APP_NAME)-linux-amd64   . && \
	  GOOS=linux   GOARCH=arm64 go build $(GO_FLAGS) -o ../../$(OUTPUT_DIR)/$(APP_NAME)-linux-arm64   . && \
	  GOOS=darwin  GOARCH=amd64 go build $(GO_FLAGS) -o ../../$(OUTPUT_DIR)/$(APP_NAME)-darwin-amd64  . && \
	  GOOS=darwin  GOARCH=arm64 go build $(GO_FLAGS) -o ../../$(OUTPUT_DIR)/$(APP_NAME)-darwin-arm64  . && \
	  GOOS=windows GOARCH=amd64 go build $(GO_FLAGS) -o ../../$(OUTPUT_DIR)/$(APP_NAME)-windows-amd64.exe .
	@echo "Cross-build complete:"
	@ls -lh $(OUTPUT_DIR)/$(APP_NAME)-*

# Release: quality gate → test → cross-build
release: quality test cross-build
	@echo "Release artifacts ready in $(OUTPUT_DIR)/"

# Run all tests
test:
	go test -timeout 60s -count=1 -run "^Test[^C]" ./...
	go test -timeout 60s -count=1 -run "^TestSQLite" .

# Test with race detection (excludes compat tests using subprocesses)
test-race:
	go test -timeout 60s -race -count=1 -run "^Test[^C]" ./...

# Verbose test with coverage
test-cover:
	go test -timeout 60s -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

# go vet: report likely mistakes
vet:
	go vet ./...

# staticcheck: report style and correctness issues
staticcheck:
	staticcheck ./...

# gocognit: report functions with high cognitive complexity (>15)
# Exclude test files — tests naturally handle many cases.
gocognit:
	gocognit -over 15 $(shell find . -name '*.go' ! -name '*_test.go') || true

# gocyclo: report functions with high cyclomatic complexity (>15)
# Exclude test files — tests naturally handle many cases.
gocyclo:
	gocyclo -over 15 $(shell find . -name '*.go' ! -name '*_test.go') || true

# Lint: run all linters
lint: vet staticcheck gocognit gocyclo

# Quality gate: fail if any quality check fails
quality: vet staticcheck
	@echo "Checking cognitive complexity (threshold 15)..."
	@! gocognit -over 15 $(shell find . -name '*.go' ! -name '*_test.go') 2>&1 | grep -q . || \
		(echo "FAIL: cognitive complexity exceeds 15 in:"; gocognit -over 15 .; exit 1)
	@echo "OK"
	@echo "Checking cyclomatic complexity (threshold 15)..."
	@! gocyclo -over 15 $(shell find . -name '*.go' ! -name '*_test.go') 2>&1 | grep -q . || \
		(echo "FAIL: cyclomatic complexity exceeds 15 in:"; gocyclo -over 15 .; exit 1)
	@echo "OK"
	@echo "Running quality checks on CLI module..."
	cd $(CMD_DIR) && go vet ./... && echo "  CLI vet: OK"

# CI: full pipeline
ci: build quality test build-cli

# Clean build artifacts
clean:
	rm -f coverage.out
	rm -rf $(OUTPUT_DIR)
	rm -rf /tmp/frigolite_*

# Run benchmarks
bench:
	go test -bench=. -benchtime=100x -timeout 60s ./benchmarks/

# Run the CLI shell (separate module with readline)
shell:
	cd $(CMD_DIR) && go run . $(DB)

# Run demo programs
demo-basic:
	go run ./cmd/demo/basic/

demo-bulk:
	go run ./cmd/demo/bulk/

# Run examples
example-native:
	go run ./_examples/native/

example-driver:
	go run ./_examples/driver/
