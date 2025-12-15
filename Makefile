# Colors for output
GREEN := $(shell printf '\033[32m')
RESET := $(shell printf '\033[0m')
BOLD := $(shell printf '\033[1m')

# Shell source files - use shfmt to find them (respects .editorconfig)
SHELL_SRC_FILES := $(shell shfmt -f .)

.PHONY: all
all: build

.PHONY: build
build:
	go build ./...

.PHONY: test
test:
	go test ./... -race

.PHONY: test/integration
test/integration:
	go test -tags=integration -v -timeout=8m ./...

.PHONY: lint
lint: lint/go lint/shellcheck

.PHONY: lint/go
lint/go:
	golangci-lint run --timeout=5m

.PHONY: lint/shellcheck
lint/shellcheck: $(SHELL_SRC_FILES)
	echo "--- shellcheck"
	shellcheck --external-sources $(SHELL_SRC_FILES)

.PHONY: fmt
fmt: fmt/go fmt/shfmt

.PHONY: fmt/go
fmt/go:
	go fmt ./...

.PHONY: fmt/shfmt
fmt/shfmt: $(SHELL_SRC_FILES)
ifdef FILE
	# Format single shell script
	if [[ -f "$(FILE)" ]] && [[ "$(FILE)" == *.sh ]]; then \
		echo "$(GREEN)==>$(RESET) $(BOLD)fmt/shfmt$(RESET) $(FILE)"; \
		shfmt -w "$(FILE)"; \
	fi
else
	echo "$(GREEN)==>$(RESET) $(BOLD)fmt/shfmt$(RESET)"
# Only do diff check in CI, errors on diff.
ifdef CI
	shfmt -d $(SHELL_SRC_FILES)
else
	shfmt -w $(SHELL_SRC_FILES)
endif
endif

.PHONY: clean
clean:
	rm -f coder-logstream-kube

.PHONY: kind/create
kind/create:
	./scripts/kind-setup.sh create

.PHONY: kind/delete
kind/delete:
	./scripts/kind-setup.sh delete

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build            - Build the project"
	@echo "  test             - Run unit tests"
	@echo "  test/integration - Run integration tests (requires KinD cluster)"
	@echo "  lint             - Run all linters"
	@echo "  lint/go          - Run golangci-lint"
	@echo "  lint/shellcheck  - Run shellcheck on shell scripts"
	@echo "  fmt              - Format all code"
	@echo "  fmt/go           - Format Go code"
	@echo "  fmt/shfmt        - Format shell scripts"
	@echo "  kind/create      - Create KinD cluster for integration tests"
	@echo "  kind/delete      - Delete KinD cluster"
	@echo "  clean            - Remove build artifacts"
