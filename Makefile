SHELL := /bin/bash

BIN := gen/diago
TARGET ?= ./...
BENCH ?= .
THRESHOLD ?= 1.0
FORMAT ?= text
AUDIT_OUT ?= gen/diago_audit.$(if $(filter json,$(FORMAT)),json,txt)
PERF_OUT ?= gen/diago_findings.$(if $(filter json,$(FORMAT)),json,txt)

.PHONY: build analytics audit perf clean test release release-major

build:
	@mkdir -p gen
	go build -o $(BIN) ./cmd/diago

# Analytics uses the default audit workflow.
analytics: audit

audit: build
	@mkdir -p gen
	$(BIN) -target "$(TARGET)" -format "$(FORMAT)" -output "$(AUDIT_OUT)"
	@echo "audit written to $(AUDIT_OUT)"

perf: build
	@mkdir -p gen
	$(BIN) --perf -target "$(TARGET)" -bench "$(BENCH)" -threshold "$(THRESHOLD)" -format "$(FORMAT)" -output "$(PERF_OUT)"
	@echo "performance findings written to $(PERF_OUT)"

test:
	go test ./...

# Release tags and pushes a semver tag. Default bumps minor: vX.Y.0 -> vX.(Y+1).0
# Examples:
#   make release                 # bump minor
#   make release MAJOR=1         # bump major
#   make release-major           # bump major
#   make release MINOR=3         # set minor on current major, patch=0
#   make release VERSION=v1.2.3  # explicit version
#   make release DRY_RUN=1       # print without tagging/pushing
release: test
	@set -euo pipefail; \
	latest="$$(git describe --tags --match 'v[0-9]*' --abbrev=0 2>/dev/null || true)"; \
	if [[ -z "$$latest" ]]; then latest="v0.0.0"; fi; \
	version="$(VERSION)"; \
	if [[ -z "$$version" ]]; then \
		base="$${latest#v}"; \
		IFS=. read -r major minor patch <<<"$$base"; \
		major="$${major:-0}"; minor="$${minor:-0}"; patch="$${patch:-0}"; \
		if [[ -n "$(MAJOR)" ]]; then \
			major=$$((major + 1)); minor=0; patch=0; \
		elif [[ -n "$(MINOR)" ]]; then \
			minor="$(MINOR)"; patch=0; \
		else \
			minor=$$((minor + 1)); patch=0; \
		fi; \
		version="v$${major}.$${minor}.$${patch}"; \
	fi; \
	if [[ ! "$$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$$ ]]; then \
		echo "invalid version: $$version (expected vX.Y.Z)" >&2; exit 1; \
	fi; \
	echo "latest tag: $$latest"; \
	echo "release tag: $$version"; \
	if [[ -n "$(DRY_RUN)" ]]; then \
		echo "dry run: git tag $$version && git push origin $$version"; \
		exit 0; \
	fi; \
	git tag "$$version"; \
	git push origin "$$version"

release-major:
	$(MAKE) release MAJOR=1

clean:
	rm -rf gen
