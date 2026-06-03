package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// skillsFS holds the agent docs bundled with diago. skills/diago/SKILL.md is
// the single source of truth: the Claude Code target installs it verbatim, and
// the AGENTS.md target reuses its body with the frontmatter stripped.
//
//go:embed skills
var skillsFS embed.FS

const (
	skillsRoot = "skills"
	skillDoc   = "skills/diago/SKILL.md"

	blockBegin = "<!-- BEGIN diago (managed by `diago skills`) -->"
	blockEnd   = "<!-- END diago (managed by `diago skills`) -->"
)

func runSkills(args []string) {
	fs := flag.NewFlagSet("skills", flag.ExitOnError)
	agent := fs.String("agent", "claude", "target coding agent: claude or agents")
	project := fs.Bool(
		"project",
		false,
		"claude target only: install into ./.claude/skills instead of ~/.claude/skills",
	)
	dir := fs.String("dir", "", "destination base directory (overrides the default location)")
	force := fs.Bool("force", false, "overwrite existing files")
	list := fs.Bool("list", false, "list the bundled skills without installing")
	fs.Parse(args)

	if *list {
		names, err := bundledSkillNames()
		if err != nil {
			fmt.Fprintf(os.Stderr, "skills: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("bundled skills:")
		for _, name := range names {
			fmt.Printf("  - %s\n", name)
		}
		return
	}

	var err error
	switch *agent {
	case "claude":
		err = installClaude(*dir, *project, *force)
	case "agents":
		err = installAgents(*dir, *force)
	default:
		fmt.Fprintf(os.Stderr, "skills: unknown -agent %q (want claude or agents)\n", *agent)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "skills: %v\n", err)
		os.Exit(1)
	}
}

// installClaude copies the bundled SKILL.md tree into a Claude Code skills
// directory, preserving the per-skill layout.
func installClaude(dir string, project, force bool) error {
	dest, err := claudeDestination(dir, project)
	if err != nil {
		return err
	}
	written := 0
	err = fs.WalkDir(skillsFS, skillsRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == skillsRoot {
			return nil
		}
		rel, err := filepath.Rel(skillsRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !force {
			if _, statErr := os.Stat(target); statErr == nil {
				return fmt.Errorf("%s already exists (use -force to overwrite)", target)
			}
		}
		data, err := skillsFS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
		written++
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("installed %d file(s) into %s\n", written, dest)
	fmt.Println("restart Claude Code (or reload skills) to pick up the new skill.")
	return nil
}

// installAgents writes the diago doc to <base>/.agents/diago.md and adds a
// managed reference block to <base>/AGENTS.md, the cross-agent convention.
func installAgents(dir string, force bool) error {
	base := dir
	if base == "" {
		base = "."
	}
	body, err := diagoDocBody()
	if err != nil {
		return err
	}

	docPath := filepath.Join(base, ".agents", "diago.md")
	if !force {
		if _, statErr := os.Stat(docPath); statErr == nil {
			return fmt.Errorf("%s already exists (use -force to overwrite)", docPath)
		}
	}
	if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(docPath, []byte(body), 0o644); err != nil {
		return err
	}

	rootPath := filepath.Join(base, "AGENTS.md")
	if err := upsertManagedBlock(rootPath); err != nil {
		return err
	}

	fmt.Printf("wrote %s\n", docPath)
	fmt.Printf("updated %s (diago reference block)\n", rootPath)
	return nil
}

// claudeDestination resolves where the Claude skill should be written. An
// explicit -dir wins; otherwise -project selects ./.claude/skills and the
// default is the user-level ~/.claude/skills.
func claudeDestination(dir string, project bool) (string, error) {
	if dir != "" {
		return dir, nil
	}
	if project {
		return filepath.Join(".claude", "skills"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "skills"), nil
}

// bundledSkillNames lists the top-level skill directories embedded in the binary.
func bundledSkillNames() ([]string, error) {
	entries, err := skillsFS.ReadDir(skillsRoot)
	if err != nil {
		return nil, fmt.Errorf("reading bundled skills: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// diagoDocBody returns the SKILL.md content with its YAML frontmatter removed,
// suitable for agents that read plain markdown rather than Claude skills.
func diagoDocBody() (string, error) {
	data, err := skillsFS.ReadFile(skillDoc)
	if err != nil {
		return "", fmt.Errorf("reading bundled doc: %w", err)
	}
	return stripFrontmatter(string(data)), nil
}

func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	rest := s[len("---\n"):]
	idx := strings.Index(rest, "\n---\n")
	if idx == -1 {
		return s
	}
	return strings.TrimLeft(rest[idx+len("\n---\n"):], "\n")
}

func managedBlock() string {
	return blockBegin + "\n" +
		"## diago\n\n" +
		"See [`.agents/diago.md`](.agents/diago.md) for how and when to run the diago Go " +
		"diagnostics CLI (audit, format, perf profiling, compare).\n" +
		blockEnd + "\n"
}

// upsertManagedBlock creates AGENTS.md if absent, replaces an existing diago
// block in place, or appends one — leaving any surrounding content untouched.
func upsertManagedBlock(path string) error {
	block := managedBlock()
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		header := "# AGENTS.md\n\nGuidance for AI coding agents working in this repository.\n\n"
		return os.WriteFile(path, []byte(header+block), 0o644)
	}
	if err != nil {
		return err
	}

	content := string(existing)
	if bi := strings.Index(content, blockBegin); bi != -1 {
		if ei := strings.Index(content, blockEnd); ei != -1 && ei > bi {
			ei += len(blockEnd)
			updated := content[:bi] + strings.TrimSuffix(block, "\n") + content[ei:]
			return os.WriteFile(path, []byte(updated), 0o644)
		}
	}

	sep := "\n\n"
	if strings.HasSuffix(content, "\n") {
		sep = "\n"
	}
	return os.WriteFile(path, []byte(content+sep+block), 0o644)
}
