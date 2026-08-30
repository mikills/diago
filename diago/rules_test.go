package diago

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModuleGoVersion(t *testing.T) {
	dir := t.TempDir()
	content := "module example.com/sample\n\ngo 1.22.2\ntoolchain go1.24.1\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	goVersion, toolchain, err := moduleGoVersion(dir)
	if err != nil {
		t.Fatal(err)
	}
	if goVersion != "1.22.2" || toolchain != "go1.24.1" {
		t.Fatalf("got go=%q toolchain=%q", goVersion, toolchain)
	}
}

func TestGoVersionAtLeast(t *testing.T) {
	tests := []struct {
		version string
		minimum string
		want    bool
	}{
		{"1.24", "1.22", true},
		{"1.22.2", "1.22", true},
		{"1.22", "1.24", false},
		{"go1.24.1", "1.24", true},
		{"invalid", "1.24", false},
	}
	for _, test := range tests {
		if got := goVersionAtLeast(test.version, test.minimum); got != test.want {
			t.Errorf("goVersionAtLeast(%q, %q) = %t, want %t", test.version, test.minimum, got, test.want)
		}
	}
}

func TestRuleCatalogGatesModernizers(t *testing.T) {
	if ruleSupportedByGoVersion("modernize/rangeint", "1.21") {
		t.Fatal("rangeint should be unavailable before Go 1.22")
	}
	if !ruleSupportedByGoVersion("modernize/rangeint", "1.22") {
		t.Fatal("rangeint should be available in Go 1.22")
	}
	if ruleSupportedByGoVersion("modernize/omitzero", "1.22") {
		t.Fatal("omitzero should be unavailable before Go 1.24")
	}
	findings := []ASTFinding{
		{Rule: "modernize/omitzero"},
		{Rule: "modernize/rangeint"},
	}
	if got := filterUnsupportedFindings(findings, "1.22"); len(got) != 1 || got[0].Rule != "modernize/rangeint" {
		t.Fatalf("unexpected version-filtered findings: %+v", got)
	}
}

func TestStaticcheckMetadata(t *testing.T) {
	descriptor, ok := descriptorForRule("staticcheck/SA5001")
	if !ok {
		t.Fatal("expected Staticcheck descriptor")
	}
	if descriptor.Source != "staticcheck" || descriptor.Severity != "high" || descriptor.FixSafety != FixSafetyNone {
		t.Fatalf("unexpected Staticcheck descriptor: %+v", descriptor)
	}
	if _, ok := descriptorForRule("staticcheck/SA9999"); ok {
		t.Fatal("unknown Staticcheck code must not become a supported rule")
	}
	descriptors := supportedRuleDescriptors("1.22")
	found := map[string]bool{}
	for _, descriptor := range descriptors {
		found[descriptor.ID] = true
	}
	for _, rule := range curatedStaticcheckProfile.Rules {
		if !found[rule.Rule] {
			t.Errorf("clean audit metadata missing %s", rule.Rule)
		}
	}
}

func TestAuditVersionMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/sample\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("package sample\n\nfunc Value() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := RunAudit(AuditConfig{TargetPath: dir, OutputFile: filepath.Join(dir, "audit.json"), Format: FormatJSON})
	if err != nil {
		t.Fatal(err)
	}
	if report.TargetGoVersion != "1.22" || report.GoToolVersion == "" {
		t.Fatalf("unexpected report versions: %#v", report)
	}
	foundScannerRule := false
	for _, rule := range report.Rules {
		if rule.ID == "scanner-err" && rule.Source == "native" && rule.FixSafety == FixSafetyNone {
			foundScannerRule = true
		}
	}
	if !foundScannerRule {
		t.Fatalf("scanner-err metadata missing: %+v", report.Rules)
	}
}
