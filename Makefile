BIN := gen/diago
TARGET ?= ./...
BENCH ?= .
THRESHOLD ?= 1.0
FORMAT ?= text
AUDIT_OUT ?= gen/diago_audit.$(if $(filter json,$(FORMAT)),json,txt)
PERF_OUT ?= gen/diago_findings.$(if $(filter json,$(FORMAT)),json,txt)

.PHONY: build analytics audit perf clean test

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

clean:
	rm -rf gen
