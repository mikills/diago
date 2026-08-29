package diago

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// FixSafety describes whether Diago may apply a rule's suggested fix.
type FixSafety string

const (
	FixSafetyNone           FixSafety = "none"
	FixSafetyMechanical     FixSafety = "mechanical"
	FixSafetyReviewRequired FixSafety = "review-required"
)

// RuleDescriptor is stable metadata for a diagnostic rule. Findings retain
// their compact shape; reports expose this catalog separately for agents and
// CI consumers that need version and fix-policy context.
type RuleDescriptor struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Source         string    `json:"source"`
	SinceGo        string    `json:"since_go,omitempty"`
	DefaultEnabled bool      `json:"default_enabled"`
	Severity       string    `json:"severity"`
	FixSafety      FixSafety `json:"fix_safety"`
	Summary        string    `json:"summary"`
}

var ruleCatalog = map[string]RuleDescriptor{
	"sql-rows-err": {
		ID: "sql-rows-err", Kind: "correctness", Source: "native", SinceGo: "1.0", DefaultEnabled: true,
		Severity: "high", FixSafety: FixSafetyNone, Summary: "Check sql.Rows.Err after iteration.",
	},
	"scanner-err": {
		ID: "scanner-err", Kind: "correctness", Source: "native", SinceGo: "1.0", DefaultEnabled: true,
		Severity: "high", FixSafety: FixSafetyNone, Summary: "Check bufio.Scanner.Err after scanning.",
	},
	"modernize": {
		ID: "modernize", Kind: "modernization", Source: "gopls", DefaultEnabled: false,
		Severity: "low", FixSafety: FixSafetyReviewRequired, Summary: "Apply a gopls modernization when supported by the target Go version.",
	},
	"u1000": {
		ID: "u1000", Kind: "maintainability", Source: "staticcheck", DefaultEnabled: false,
		Severity: "low", FixSafety: FixSafetyReviewRequired, Summary: "Review unused code reported by Staticcheck.",
	},
	"infertypeargs": {
		ID: "infertypeargs", Kind: "modernization", Source: "gopls", SinceGo: "1.18", DefaultEnabled: false,
		Severity: "medium", FixSafety: FixSafetyNone, Summary: "Remove explicit type arguments that the compiler can infer.",
	},
}

var modernizeSinceGo = map[string]string{
	"efaceany":       "1.18",
	"fmtappendf":     "1.19",
	"minmax":         "1.21",
	"slicesclone":    "1.21",
	"slicescontains": "1.21",
	"slicesdelete":   "1.21",
	"sortslice":      "1.21",
	"rangeint":       "1.22",
	"mapsloop":       "1.23",
	"bloop":          "1.24",
	"omitzero":       "1.24",
	"splitseq":       "1.24",
	"testingcontext": "1.24",
}

func descriptorForRule(id string) (RuleDescriptor, bool) {
	if descriptor, ok := ruleCatalog[id]; ok {
		return descriptor, true
	}
	if rule, ok := curatedStaticcheckRule(id); ok {
		return RuleDescriptor{
			ID: id, Kind: "correctness", Source: "staticcheck", DefaultEnabled: false,
			Severity: rule.Severity, FixSafety: FixSafetyNone, Summary: "Review this curated Staticcheck correctness diagnostic.",
		}, true
	}
	category, ok := strings.CutPrefix(id, "modernize/")
	if !ok || category == "" {
		return RuleDescriptor{}, false
	}
	return RuleDescriptor{
		ID: id, Kind: "modernization", Source: "gopls", SinceGo: modernizeSinceGo[category], DefaultEnabled: false,
		Severity: "low", FixSafety: FixSafetyReviewRequired, Summary: "Apply this gopls modernization when supported by the target Go version.",
	}, true
}

// supportedRuleDescriptors returns the catalog entries that can apply to the
// module's declared language version. Unknown versions retain all entries so
// callers do not silently lose metadata for legacy go.mod files.
func supportedRuleDescriptors(goVersion string) []RuleDescriptor {
	descriptors := make([]RuleDescriptor, 0, len(ruleCatalog)+len(curatedStaticcheckProfile.Rules))
	for _, descriptor := range ruleCatalog {
		if descriptor.SinceGo != "" && goVersion != "" && !goVersionAtLeast(goVersion, descriptor.SinceGo) {
			continue
		}
		descriptors = append(descriptors, descriptor)
	}
	for _, rule := range curatedStaticcheckProfile.Rules {
		descriptor, _ := descriptorForRule(rule.Rule)
		descriptors = append(descriptors, descriptor)
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].ID < descriptors[j].ID })
	return descriptors
}

func ruleSupportedByGoVersion(id, goVersion string) bool {
	descriptor, ok := descriptorForRule(id)
	if !ok || descriptor.SinceGo == "" || goVersion == "" {
		return true
	}
	return goVersionAtLeast(goVersion, descriptor.SinceGo)
}

// moduleGoVersion reads the go and toolchain directives from the module that
// resolveTarget selected. A missing go directive is valid and returns blanks.
func moduleGoVersion(workDir string) (goVersion, toolchain string, err error) {
	file, err := os.Open(filepath.Join(workDir, "go.mod"))
	if err != nil {
		return "", "", fmt.Errorf("reading go.mod: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSpace(strings.Split(scanner.Text(), "//")[0]))
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "go":
			if validGoVersion(fields[1]) {
				goVersion = fields[1]
			}
		case "toolchain":
			if validGoVersion(strings.TrimPrefix(fields[1], "go")) {
				toolchain = fields[1]
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("reading go.mod: %w", err)
	}
	return goVersion, toolchain, nil
}

func goVersionAtLeast(version, minimum string) bool {
	v, ok := parseGoVersion(version)
	if !ok {
		return false
	}
	m, ok := parseGoVersion(minimum)
	if !ok {
		return false
	}
	for i := range v {
		if v[i] != m[i] {
			return v[i] > m[i]
		}
	}
	return true
}

func validGoVersion(version string) bool {
	_, ok := parseGoVersion(version)
	return ok
}

func parseGoVersion(version string) ([3]int, bool) {
	var parts [3]int
	version = strings.TrimPrefix(version, "go")
	fields := strings.Split(version, ".")
	if len(fields) < 2 || len(fields) > 3 {
		return parts, false
	}
	for i, field := range fields {
		value, err := strconv.Atoi(field)
		if err != nil || value < 0 {
			return parts, false
		}
		parts[i] = value
	}
	return parts, true
}
