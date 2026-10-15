package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These tests read the files this package ships next to its sources. Under
// `bazel test` the go_test declares the package's own repo_files partitions as
// data, not the scenarios one by one (a scenario must be addable without
// touching BUILD.bazel), so they run under Bazel as they do under go test.
func loadShipped(t *testing.T) map[string]*Scenario {
	t.Helper()
	got, err := loadScenarios("scenarios")
	if err != nil {
		t.Fatalf("the shipped scenarios do not load: %v", err)
	}
	byID := map[string]*Scenario{}
	for _, sc := range got {
		byID[sc.ID] = sc
	}
	return byID
}

func stepNamed(t *testing.T, sc *Scenario, name string) *Step {
	t.Helper()
	for i := range sc.Steps {
		if sc.Steps[i].Name == name {
			return &sc.Steps[i]
		}
	}
	t.Fatalf("scenario %s has no step %q", sc.ID, name)
	return nil
}

func TestShippedScenariosAreR1R1bR2(t *testing.T) {
	byID := loadShipped(t)
	if len(byID) != 3 || byID["R1"] == nil || byID["R1b"] == nil || byID["R2"] == nil {
		t.Fatalf("shipped scenarios = %v, want exactly R1, R1b and R2", keys(byID))
	}
	for id, sc := range byID {
		if len(sc.Spec) == 0 || !strings.Contains(strings.Join(sc.Spec, " "), "#5877") {
			t.Errorf("%s cites no #5877 spec text: %v (expectations are declared from spec text)", id, sc.Spec)
		}
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Amended 2026-09-29: the spec text gives no typed code and no exit number for
// the current read of a forgotten Memory, so neither R1 nor R1b may pin one.
func TestShippedForgottenReadPinsNoCodeAndNoExitNumber(t *testing.T) {
	byID := loadShipped(t)
	for _, id := range []string{"R1", "R1b"} {
		raw, err := os.ReadFile(filepath.Join("scenarios", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(raw))
		for _, banned := range []string{"gone", "exit 3", "not_found"} {
			if strings.Contains(text, banned) {
				t.Errorf("%s.json contains %q: the spec text does not say what a forgotten read reports", id, banned)
			}
		}
		for _, name := range []string{"read-forgotten", "read-never-created"} {
			var step *Step
			for i := range byID[id].Steps {
				if byID[id].Steps[i].Name == name {
					step = &byID[id].Steps[i]
				}
			}
			if step == nil {
				continue // read-never-created exists only in R1b
			}
			var nonzero, stdoutEmpty bool
			for _, a := range step.Expect {
				switch a.Op {
				case "exit":
					if a.ExitEquals == nil && a.Nonzero {
						nonzero = true
					} else {
						t.Errorf("%s/%s pins an exit number; only nonzero is allowed", id, name)
					}
				case "stdout":
					stdoutEmpty = stdoutEmpty || a.Empty
				case "stderr_code":
					t.Errorf("%s/%s asserts a stderr code; it may only capture it", id, name)
				}
			}
			if !nonzero || !stdoutEmpty {
				t.Errorf("%s/%s must assert exit nonzero and empty stdout (nonzero=%v empty=%v)", id, name, nonzero, stdoutEmpty)
			}
		}
	}
}

func TestShippedR1ProvesRetireWithoutLosingTheRecord(t *testing.T) {
	r1 := loadShipped(t)["R1"]
	if r1.State != "pass" {
		t.Errorf("R1 state = %q, want pass", r1.State)
	}
	forgotten := stepNamed(t, r1, "read-forgotten")
	if _, ok := forgotten.Capture["FORGOTTEN_CODE"]; !ok {
		t.Errorf("the forgotten read's stderr code should be captured for the receipts: %v", forgotten.Capture)
	}
	var byteCompares int
	for _, st := range r1.Steps {
		for _, a := range st.Expect {
			if a.Op == "stdout" && a.EqualsInput != "" {
				byteCompares++
			}
		}
	}
	// current read = new bytes, plus --version V1 and --version V2 byte-compared against inputs
	if byteCompares < 3 {
		t.Errorf("R1 byte-compares %d reads against declared inputs, want at least 3", byteCompares)
	}
}

func TestShippedR1bComparesForgottenAgainstNeverCreated(t *testing.T) {
	r1b := loadShipped(t)["R1b"]
	never := stepNamed(t, r1b, "read-never-created")
	var compared bool
	for _, a := range never.Expect {
		if a.Op == "compare" && a.Relation == "differ" &&
			a.Left.Step == "read-forgotten" && a.Left.Field == "stderr_code" &&
			a.Right.Step == "read-never-created" && a.Right.Field == "stderr_code" {
			compared = true
		}
	}
	if !compared {
		t.Errorf("R1b must compare the two reads' stderr codes as differ: %+v", never.Expect)
	}
}

func TestShippedR2NoSilentOverwrite(t *testing.T) {
	r2 := loadShipped(t)["R2"]
	second := stepNamed(t, r2, "update-second")
	var conflict, exit4 bool
	for _, a := range second.Expect {
		if a.Op == "stderr_code" && a.CodeEquals == "revision_conflict" {
			conflict = true
		}
		if a.Op == "exit" && a.ExitEquals != nil && *a.ExitEquals == 4 {
			exit4 = true
		}
	}
	if !conflict || !exit4 {
		t.Errorf("R2's second guarded update must fail revision_conflict with exit 4 (code=%v exit=%v)", conflict, exit4)
	}
}

func TestPackageDoesNotEmbedOrListScenarios(t *testing.T) {
	// Only the package's own sources matter, and only the real directive: this
	// test file and the docs legitimately mention go:embed in prose.
	directive := regexp.MustCompile(`(?m)^\s*//go:embed\b`)
	sources, _ := filepath.Glob("*.go")
	for _, f := range sources {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if directive.Match(data) {
			t.Errorf("%s uses go:embed: gazelle would then list every scenario in BUILD.bazel", f)
		}
	}
	if build, err := os.ReadFile("BUILD.bazel"); err == nil {
		for _, banned := range []string{"embedsrcs", ".json", "scenarios/"} {
			if strings.Contains(string(build), banned) {
				t.Errorf("BUILD.bazel mentions %q: adding a scenario must not require a BUILD.bazel change", banned)
			}
		}
	}
}

func TestREADMEDocumentsTheContract(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("README.md: %v", err)
	}
	text := strings.ToLower(string(raw))
	for _, topic := range []string{
		"adding a scenario", "xfail", "skip", "normaliz", "exit code", "guard",
		"json path", "assertion", "<ws>", "<token#", "--scenarios", "--list", "receipts.json",
	} {
		if !strings.Contains(text, topic) {
			t.Errorf("README.md does not mention %q", topic)
		}
	}
}
