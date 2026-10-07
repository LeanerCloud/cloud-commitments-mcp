package mcp_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGosecPinHasSingleSource guards the invariant from #16 and #22: the gosec
// version and the rule set are declared exactly once, in
// scripts/run-gosec.sh, and every call site delegates to that script.
//
// The failure these two issues describe was a functional one. The pre-commit
// hook excluded 15 gosec rules, the Makefile excluded 8, and CI excluded none,
// so a clean local pass predicted nothing about what CI would flag. Pinning
// the version in four files is the same class of drift waiting to happen, and
// neither that nor the rule-set divergence is caught by any compiler or Go
// test, so it needs a test of its own.
func TestGosecPinHasSingleSource(t *testing.T) {
	t.Parallel()

	repoRoot := repoRoot(t)
	script := filepath.Join(repoRoot, "scripts", "run-gosec.sh")

	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}

	versionRe := regexp.MustCompile(`GOSEC_VERSION="([^"]+)"`)
	m := versionRe.FindSubmatch(body)
	if m == nil {
		t.Fatalf("no GOSEC_VERSION declaration in scripts/run-gosec.sh; it is the single source of truth")
	}
	version := "v" + string(m[1])

	// Every file that runs or installs gosec. Each must reach run-gosec.sh
	// rather than carry its own pin, so that is what the test asserts: no
	// gosec version literal, and no gosec module path outside the script.
	callSites := []string{
		filepath.Join("scripts", "gosec-hook.sh"),
		filepath.Join("Makefile"),
		filepath.Join(".pre-commit-config.yaml"),
		filepath.Join(".github", "workflows", "ci.yml"),
		filepath.Join(".github", "workflows", "pre-commit.yml"),
	}

	gosecModuleRe := regexp.MustCompile(`securego/gosec`)

	for _, rel := range callSites {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(repoRoot, rel)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			text := string(content)

			for i, line := range strings.Split(text, "\n") {
				trimmed := strings.TrimSpace(line)
				// Comments describe the pin; they cannot change behavior.
				if strings.HasPrefix(trimmed, "#") {
					continue
				}
				if strings.Contains(line, version) {
					t.Errorf("%s:%d hardcodes the gosec version %q; call scripts/run-gosec.sh instead "+
						"(the version is declared once, in that script)", rel, i+1, version)
				}
				if gosecModuleRe.MatchString(line) {
					t.Errorf("%s:%d references the gosec module directly; call scripts/run-gosec.sh, "+
						"which owns the pinned version", rel, i+1)
				}
			}
		})
	}
}

// TestGosecRuleSetsAgree asserts the three gates all invoke the same script,
// which is what makes their rule sets identical rather than merely similar.
// A future edit that gives one call site its own flags would otherwise pass
// review silently, since nothing else compares the three.
func TestGosecRuleSetsAgree(t *testing.T) {
	t.Parallel()

	repoRoot := repoRoot(t)

	required := map[string]string{
		filepath.Join("scripts", "gosec-hook.sh"):       "scripts/run-gosec.sh",
		filepath.Join("Makefile"):                       "scripts/run-gosec.sh",
		filepath.Join(".github", "workflows", "ci.yml"): "scripts/run-gosec.sh",
	}

	for rel, want := range required {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(repoRoot, rel)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if !strings.Contains(string(content), want) {
				t.Errorf("%s does not invoke %s; a local run and the CI job "+
					"would enforce different gosec rules", rel, want)
			}
		})
	}

	// No gate may reintroduce a rule exclusion. gosec's defaults pass clean on
	// this tree (0 issues), so every -exclude here is drift by construction.
	excludeRe := regexp.MustCompile(`-exclude=G|-exclude[= ]`)
	for rel := range required {
		t.Run("no-exclude/"+filepath.Base(rel), func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(repoRoot, rel)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			for i, line := range strings.Split(string(content), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if excludeRe.MatchString(line) {
					t.Errorf("%s:%d passes a gosec -exclude list; the three gates must "+
						"run gosec's full default rule set", rel, i+1)
				}
			}
		})
	}
}

// repoRoot returns the repository root, walking up from the test's working
// directory (the package directory for a root-package test).
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root (no go.mod above the test directory)")
		}
		dir = parent
	}
}
