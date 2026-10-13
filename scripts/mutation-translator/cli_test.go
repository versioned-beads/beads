package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/testutil/bazeltest"
)

// TestB1MainsCharacterised_TranslatorDryRun drives the real command line with
// --dry-run against a stand-in dolt that returns canned row diffs, and checks
// stdout, exit status and every query the command sends to dolt against a
// golden transcript. The transcript was captured before the command's logic
// moved into a library and re-recorded when the translator began planning a whole
// commit step: the queries now name the changed tables first and read only the
// row diffs of the replayed tables among them, and positionals follow a "--".
// The stand-in keeps the test free of external tools, so it runs everywhere the
// command can be built.

var updateGolden = flag.Bool("update", false, "rewrite golden transcripts from the current output")

const (
	cliFromRef = "0123456789abcdefghijklmnopqrstuv"
	cliToRef   = "vutsrqponmlkjihgfedcba9876543210"
)

// buildCLI returns the command under test. Under Bazel the sandbox holds no Go
// toolchain, so the go_test names the go_binary as data and sets
// MUTATION_TRANSLATOR_BIN to its runfile; elsewhere the command is compiled
// into a temporary directory.
func buildCLI(t *testing.T) string {
	t.Helper()
	if bazeltest.IsBazel() {
		bin, err := bazeltest.RunfileEnv("MUTATION_TRANSLATOR_BIN")
		if err != nil {
			t.Fatalf("locating the prebuilt command: %v", err)
		}
		return bin
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go toolchain not found: %v", err)
	}
	out := filepath.Join(t.TempDir(), "mutation-translator")
	if b, err := exec.Command(goBin, "build", "-o", out, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// translatorShim is a stand-in dolt that answers the queries the command issues
// from canned CSV files and records every argument it is given.
type translatorShim struct {
	dir     string
	argvLog string
}

// cannedRows holds what the stand-in answers: the step's changed tables, the
// issue and dependency row diffs, and which edge targets exist before the step.
type cannedRows struct {
	summary, issues, deps, present string
}

func newTranslatorShim(t *testing.T, c cannedRows) translatorShim {
	t.Helper()
	dir := t.TempDir()
	s := translatorShim{dir: dir, argvLog: filepath.Join(dir, "argv.log")}
	files := map[string]string{"summary.csv": c.summary, "issues.csv": c.issues, "deps.csv": c.deps, "present.csv": c.present}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %s\ncase \"$3\" in\n*dolt_diff_summary*) cat %s ;;\n*dolt_commit_diff_issues*) cat %s ;;\n*dolt_commit_diff_dependencies*) cat %s ;;\n*'issues AS OF'*) cat %s ;;\nesac\n",
		shellQuote(s.argvLog), shellQuote(filepath.Join(dir, "summary.csv")), shellQuote(filepath.Join(dir, "issues.csv")),
		shellQuote(filepath.Join(dir, "deps.csv")), shellQuote(filepath.Join(dir, "present.csv")))
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the dolt stand-in: %v", err)
	}
	return s
}

func runTranslator(t *testing.T, bin string, shim translatorShim, args ...string) (exit int, stdout, stderr string) {
	t.Helper()
	return runTranslatorWithPath(t, bin, shim.dir+":/usr/bin:/bin", args...)
}

func runTranslatorWithPath(t *testing.T, bin, path string, args ...string) (exit int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"PATH=" + path, "HOME=" + t.TempDir()}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running %s: %v", bin, err)
		}
		exit = ee.ExitCode()
	}
	return exit, so.String(), se.String()
}

func readShimLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("creating testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s (run with -update to create it): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("output differs from %s\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

const (
	summaryHeader    = "from_table_name,to_table_name,diff_type\n"
	issuesChanged    = summaryHeader + "issues,issues,modified\n"
	depsChanged      = summaryHeader + "dependencies,dependencies,modified\n"
	issuesHeaderOnly = "diff_type,to_id,from_id\n"
	depsHeaderOnly   = "diff_type,to_issue_id,to_depends_on_issue_id,to_type,from_issue_id,from_depends_on_issue_id\n"
)

func TestB1MainsCharacterised_TranslatorDryRun(t *testing.T) {
	bin := buildCLI(t)

	cases := []struct {
		name string
		rows cannedRows
	}{
		{"issue added", cannedRows{
			summary: issuesChanged,
			issues:  "diff_type,to_id,to_title,to_description,to_priority,to_issue_type,to_status,from_id\nadded,x-1,\"Widget, deluxe\",Does things,2,task,open,\n",
		}},
		{"issue modified", cannedRows{
			summary: issuesChanged,
			issues:  "diff_type,to_id,to_title,to_description,to_status,from_id,from_title,from_description,from_status\nmodified,x-1,New title,New desc,open,x-1,Old title,Old desc,open\n",
		}},
		{"issue closed", cannedRows{
			summary: issuesChanged,
			issues:  "diff_type,to_id,to_status,to_close_reason,to_closed_at,from_id,from_status,from_close_reason,from_closed_at\nmodified,x-1,closed,done,2026-01-01 00:00:00,x-1,open,,\n",
		}},
		{"issue removed", cannedRows{
			summary: issuesChanged,
			issues:  "diff_type,to_id,from_id\nremoved,,x-1\n",
		}},
		{"dependency added", cannedRows{
			summary: depsChanged,
			deps:    depsHeaderOnly + "added,x-1,x-2,blocks,,\n",
			present: "id\nx-2\n",
		}},
		{"dependency removed", cannedRows{
			summary: depsChanged,
			deps:    depsHeaderOnly + "removed,,,,x-1,x-2\n",
		}},
		{"nothing changed", cannedRows{summary: summaryHeader}},
	}

	var transcript strings.Builder
	for _, tc := range cases {
		shim := newTranslatorShim(t, tc.rows)
		exit, stdout, stderr := runTranslator(t, bin, shim,
			"--data-dir", t.TempDir(), "--work-dir", t.TempDir(),
			"--from", cliFromRef, "--to", cliToRef, "--issue", "x-1", "--dry-run")
		if exit != 0 {
			t.Fatalf("%s: exited %d\nstderr: %s", tc.name, exit, stderr)
		}
		fmt.Fprintf(&transcript, "== %s\nexit: %d\nstdout:\n%sdolt argv:\n%s\n", tc.name, exit, stdout, readShimLog(t, shim.argvLog))
	}

	shim := newTranslatorShim(t, cannedRows{})
	exit, stdout, stderr := runTranslator(t, bin, shim)
	fmt.Fprintf(&transcript, "== missing flags\nexit: %d\nstdout:\n%sstderr:\n%sdolt argv:\n%s\n", exit, stdout, stderr, readShimLog(t, shim.argvLog))

	checkGolden(t, "cli_dry_run.golden", transcript.String())
}

// stubBd writes a stand-in bd that appends its arguments, one invocation per
// line, to a log, and returns the binary's path and the log's.
func stubBd(t *testing.T) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + shellQuote(log) + "\n"
	bin = filepath.Join(dir, "bd")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the bd stand-in: %v", err)
	}
	return bin, log
}

// B3.PinnedBd on the command line: replaying runs the bd named by --bd, and a bd
// that happens to be first on PATH is never started.
func TestB3PinnedBdOnTheCommandLine(t *testing.T) {
	bin := buildCLI(t)
	shim := newTranslatorShim(t, cannedRows{summary: issuesChanged, issues: "diff_type,to_id,from_id\nremoved,,x-1\n"})
	pinned, pinnedLog := stubBd(t)
	decoy, decoyLog := stubBd(t)

	exit, _, stderr := runTranslatorWithPath(t, bin, shim.dir+":"+filepath.Dir(decoy)+":/usr/bin:/bin",
		"--data-dir", t.TempDir(), "--work-dir", t.TempDir(),
		"--from", cliFromRef, "--to", cliToRef, "--issue", "x-1", "--bd", pinned)
	if exit != 0 {
		t.Fatalf("exited %d\nstderr: %s", exit, stderr)
	}
	if got, want := readShimLog(t, pinnedLog), "delete --force -- x-1\n"; got != want {
		t.Errorf("the pinned bd was run with %q, want %q", got, want)
	}
	if got := readShimLog(t, decoyLog); got != "" {
		t.Errorf("the bd on PATH was started: %q", got)
	}
}

// Replaying without --bd is a usage error, not a search of PATH: nothing is
// queried and no bd runs, even with one on PATH.
func TestB3ExecuteNeedsAnExplicitBd(t *testing.T) {
	bin := buildCLI(t)
	shim := newTranslatorShim(t, cannedRows{summary: issuesChanged, issues: "diff_type,to_id,from_id\nremoved,,x-1\n"})
	decoy, decoyLog := stubBd(t)

	exit, _, stderr := runTranslatorWithPath(t, bin, shim.dir+":"+filepath.Dir(decoy)+":/usr/bin:/bin",
		"--data-dir", t.TempDir(), "--work-dir", t.TempDir(),
		"--from", cliFromRef, "--to", cliToRef, "--issue", "x-1")
	if exit != 2 {
		t.Errorf("exit = %d, want 2 (usage)", exit)
	}
	if !strings.Contains(stderr, "--bd") {
		t.Errorf("stderr does not say --bd is needed: %q", stderr)
	}
	if got := readShimLog(t, decoyLog); got != "" {
		t.Errorf("the bd on PATH was started: %q", got)
	}
	if got := readShimLog(t, shim.argvLog); got != "" {
		t.Errorf("dolt was queried before the usage error: %q", got)
	}
}
