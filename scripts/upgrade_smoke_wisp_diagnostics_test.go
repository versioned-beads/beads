package scripts_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Scenario 7 of scripts/upgrade-smoke-test.sh grades whether a note written to a
// migrated wisp is visible to the next process. A red hosted run used to say only
// "Wisp mutation did not persist after upgrade": the exit status and stderr of
// both commands were thrown away, so a show that errored, a read path that missed
// a note that was stored, and a write that never landed looked the same. These
// tests run the real script against stub bd binaries and a stub dolt, which makes
// each class injectable and the evidence kept for it checkable.
//
// The direct readback is the script's own: it must reach dolt as ${DOLT_BIN:-dolt}
// and name the disposable workspace (its working directory or an argument such as
// --data-dir) before that workspace is removed. The verdict inputs are dolt's exit
// status and whether its output holds the expected notes.

// What the stubs print. A failing run keeps the evidence it exists to preserve; a
// passing run prints none of it.
const (
	wispUpdateOut = "STUB_UPDATE_STDOUT"
	wispUpdateErr = "STUB_UPDATE_STDERR"
	wispShowOut   = "STUB_SHOW_STDOUT"
	wispShowErr   = "STUB_SHOW_STDERR"
	wispDoltRows  = "STUB_DOLT_READBACK"

	// The failures scenario 7 already reports, which diagnostics must not replace.
	wispNotPersisted = "Wisp mutation did not persist after upgrade"
	wispUpdateFailed = "bd update on a migrated wisp errors after upgrade"
	wispPersisted    = "Wisp writable after upgrade (mutation persisted)"
)

var wispClasses = []string{"SHOW_ERRORED", "READ_PATH_MISS", "WRITE_NOT_VISIBLE"}

// wispStubBD stands in for the previous release and the candidate alike (it is
// installed as bd-prev and bd-cand). It keeps its state in the workspace it runs
// from, under .beads/stub, so the stub dolt can report what was really stored
// whatever show prints. Faults (STUB_UPDATE, STUB_SHOW) hit wisp ids (fw-*) only,
// which keeps scenarios 1-6 green and the verdict about scenario 7 alone.
const wispStubBD = `#!/usr/bin/env bash
set -u
role=${0##*/}
state=.beads/stub

next_id() {
	local n=0
	[ -f "$state/seq" ] && n=$(cat "$state/seq")
	n=$((n + 1))
	echo "$n" > "$state/seq"
	echo "$1-$n"
}

list_ids() {
	local f
	for f in "$state"/"$1"-*.title; do
		[ -e "$f" ] || continue
		f=${f##*/}
		echo "${f%.title}"
	done
}

# once_mode <mode> <counter>: "<x>_once" acts as <x> on the first call, then as ok.
once_mode() {
	local mode=$1 counter=$2 n=0
	[ -f "$counter" ] && n=$(cat "$counter")
	echo $((n + 1)) > "$counter"
	case "$mode" in
	*_once) if [ "$n" = 0 ]; then echo "${mode%_once}"; else echo ok; fi ;;
	*) echo "$mode" ;;
	esac
}

print_issue() {
	echo "$1: $(cat "$state/$1.title")"
	if [ -s "$state/$1.notes" ]; then
		echo "Notes: $(cat "$state/$1.notes")"
	fi
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case "$cmd" in
version)
	echo "bd version 1.3.1 (stub $role)"
	;;
init)
	mkdir -p .beads/embeddeddolt/beads/.dolt "$state"
	echo '{"backend":"dolt","dolt_mode":"embedded","dolt_database":"beads"}' > .beads/metadata.json
	git config --get beads.role >/dev/null 2>&1 || git config beads.role maintainer
	;;
migrate | doctor) ;;
config)
	[ "$*" = "get storage.mode" ] && echo embedded
	;;
create)
	silent=0 kind=fk title=
	while [ $# -gt 0 ]; do
		case "$1" in
		--silent) silent=1 ;;
		--ephemeral) kind=fw ;;
		--title) title=$2; shift ;;
		--type | --priority) shift ;;
		esac
		shift
	done
	id=$(next_id "$kind")
	echo "$title" > "$state/$id.title"
	echo open > "$state/$id.status"
	if [ "$silent" = 1 ]; then echo "$id"; else echo "Created issue: $id"; fi
	;;
dep)
	echo "$3" >> "$state/$2.blockers"
	;;
close)
	echo closed > "$state/$1.status"
	;;
list)
	if [ "${1:-}" = --json ]; then
		sep=
		printf '['
		for id in $(list_ids fk); do
			printf '%s{"id":"%s","title":"%s"}' "$sep" "$id" "$(cat "$state/$id.title")"
			sep=,
		done
		echo ']'
	else
		for id in $(list_ids fk); do echo "$id $(cat "$state/$id.title")"; done
	fi
	;;
mol)
	for id in $(list_ids fw); do echo "$id $(cat "$state/$id.title")"; done
	;;
ready | blocked)
	for id in $(list_ids fk); do
		[ "$(cat "$state/$id.status")" = open ] || continue
		held=0
		if [ -f "$state/$id.blockers" ]; then
			while read -r b; do
				[ "$(cat "$state/$b.status" 2>/dev/null)" = open ] && held=1
			done < "$state/$id.blockers"
		fi
		if [ "$cmd" = blocked ] && [ "$held" = 1 ]; then echo "$id $(cat "$state/$id.title")"; fi
		if [ "$cmd" = ready ] && [ "$held" = 0 ]; then echo "$id $(cat "$state/$id.title")"; fi
	done
	;;
update | show)
	id= notes=
	while [ $# -gt 0 ]; do
		case "$1" in
		--notes) notes=$2; shift ;;
		-*) ;;
		*) [ -n "$id" ] || id=$1 ;;
		esac
		shift
	done
	case "$cmd:$id" in
	update:fw-*)
		echo STUB_UPDATE_STDOUT
		echo STUB_UPDATE_STDERR >&2
		case "$(once_mode "${STUB_UPDATE:-ok}" "$state/$id.updates")" in
		ok) echo "$notes" > "$state/$id.notes" ;;
		fail) exit "${STUB_UPDATE_RC:-41}" ;;
		esac # noop: exit 0 and write nothing
		;;
	update:*)
		echo "$notes" > "$state/$id.notes"
		;;
	show:fw-*)
		echo STUB_SHOW_STDOUT
		echo STUB_SHOW_STDERR >&2
		case "$(once_mode "${STUB_SHOW:-ok}" "$state/$id.shows")" in
		error) exit "${STUB_SHOW_RC:-37}" ;;
		hide) echo "$id: $(cat "$state/$id.title")" ;;
		fail_with_notes) print_issue "$id"; exit "${STUB_SHOW_RC:-37}" ;;
		*) print_issue "$id" ;;
		esac
		;;
	show:*)
		print_issue "$id"
		;;
	esac
	;;
*)
	echo "stub bd: unsupported command: $cmd $*" >&2
	exit 2
	;;
esac
exit 0
`

// wispStubDolt answers any sql with one CSV row per wisp, built from what the stub
// bd really stored. The query text is not interpreted: every query gets the header
// id,title,notes,source and a row per wisp, so a verdict has to come from the exit
// status and from whether the notes text appears in the output, not from the shape
// or count of the columns. Like the real dolt it needs a repository: the workspace
// must be named by the working directory or an argument, and a workspace the script
// has already removed is not found.
const wispStubDolt = `#!/usr/bin/env bash
set -u
echo "argv=$*" >> "$STUB_DOLT_LOG"
case "${1:-}" in
version | --version)
	echo "dolt version 2.2.0 (stub)"
	exit 0
	;;
esac
fixture=
for arg in "$PWD" "$@"; do
	if [[ $arg =~ (.*/bd-upgrade-[A-Za-z0-9._]+) ]]; then
		fixture=${BASH_REMATCH[1]}
		break
	fi
done
if [ -z "$fixture" ] || [ ! -d "$fixture/.beads/stub" ]; then
	echo "STUB_DOLT_NO_REPO fixture=${fixture:-none} cwd=$PWD" >> "$STUB_DOLT_LOG"
	echo "stub dolt: no repository at ${fixture:-the working directory or in the arguments}" >&2
	exit 1
fi
echo "STUB_DOLT_REPO fixture=$fixture" >> "$STUB_DOLT_LOG"
echo "id,title,notes,source"
if [ "${STUB_DOLT:-ok}" = fail ]; then
	echo "STUB_DOLT_STDERR" >&2
	exit 1
fi
for f in "$fixture"/.beads/stub/fw-*.title; do
	[ -e "$f" ] || continue
	id=${f##*/}
	id=${id%.title}
	echo "$id,$(cat "$f"),$(cat "$fixture/.beads/stub/$id.notes" 2>/dev/null),STUB_DOLT_READBACK"
done
exit 0
`

type wispRun struct {
	out      string
	exit     int
	doltLog  string
	leftover []string
}

func TestUpgradeSmokeWispDiagnostics(t *testing.T) {
	hostPath := pathWithoutDolt(t, scratchDir(t))
	for _, tc := range []struct {
		name       string
		stub       []string
		doltAbsent bool
		check      func(t *testing.T, r wispRun)
	}{
		// A healthy wisp passes and prints no diagnostic block.
		{"healthy", nil, false, func(t *testing.T, r wispRun) {
			r.passed(t)
		}},
		// The original predicate is the update's exit status and the notes in show's
		// stdout; show's own exit status is not graded.
		{"show_exit_status_not_graded", []string{"STUB_SHOW=fail_with_notes"}, false, func(t *testing.T, r wispRun) {
			r.passed(t)
		}},
		{"show_errored", []string{"STUB_SHOW=error", "STUB_SHOW_RC=37"}, false, func(t *testing.T, r wispRun) {
			r.failed(t, wispNotPersisted)
			r.has(t, "SHOW_ERRORED", wispShowOut, wispShowErr)
			r.lacks(t, "WRITE_NOT_VISIBLE")
			r.status(t, "show", 37)
		}},
		{"read_path_miss", []string{"STUB_SHOW=hide"}, false, func(t *testing.T, r wispRun) {
			r.failed(t, wispNotPersisted)
			r.has(t, "READ_PATH_MISS", wispShowOut, wispShowErr, wispDoltRows)
			r.lacks(t, "SHOW_ERRORED", "WRITE_NOT_VISIBLE")
			r.readbackRan(t)
		}},
		{"write_not_visible", []string{"STUB_UPDATE=noop"}, false, func(t *testing.T, r wispRun) {
			r.failed(t, wispNotPersisted)
			r.has(t, "WRITE_NOT_VISIBLE", wispUpdateOut, wispUpdateErr, wispShowOut, wispShowErr, wispDoltRows)
			r.lacks(t, "SHOW_ERRORED", "READ_PATH_MISS")
			r.readbackRan(t)
		}},
		// The write really is missing here, and the stub dolt would prove it; a failed
		// update is still reported as that, never as an exit-0 write loss.
		{"update_failed", []string{"STUB_UPDATE=fail", "STUB_UPDATE_RC=41"}, false, func(t *testing.T, r wispRun) {
			r.failed(t, wispUpdateFailed)
			r.has(t, wispUpdateOut, wispUpdateErr)
			r.lacks(t, "SHOW_ERRORED", "READ_PATH_MISS", "WRITE_NOT_VISIBLE", wispNotPersisted)
			r.status(t, "update", 41)
		}},
		{"readback_unavailable", []string{"STUB_UPDATE=noop"}, true, func(t *testing.T, r wispRun) {
			r.failed(t, wispNotPersisted)
			r.has(t, wispUpdateOut, wispUpdateErr, wispShowOut, wispShowErr)
			r.lacks(t, "SHOW_ERRORED", "READ_PATH_MISS", "WRITE_NOT_VISIBLE")
			r.unresolved(t)
			r.readbackSaid(t)
		}},
		// dolt prints a header and fails; the notes it did not return are not proof
		// that they were never written.
		{"readback_failed", []string{"STUB_SHOW=hide", "STUB_DOLT=fail"}, false, func(t *testing.T, r wispRun) {
			r.failed(t, wispNotPersisted)
			r.has(t, wispUpdateOut, wispUpdateErr, wispShowOut, wispShowErr)
			r.lacks(t, "SHOW_ERRORED", "READ_PATH_MISS", "WRITE_NOT_VISIBLE")
			r.unresolved(t)
			r.readbackSaid(t)
		}},
		// No retry: a command that errs once and then recovers is still a failure.
		{"show_not_retried", []string{"STUB_SHOW=error_once"}, false, func(t *testing.T, r wispRun) {
			r.failed(t, wispNotPersisted)
		}},
		{"update_not_retried", []string{"STUB_UPDATE=fail_once"}, false, func(t *testing.T, r wispRun) {
			r.failed(t, wispUpdateFailed)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runWispSmoke(t, hostPath, tc.doltAbsent, tc.stub...)
			tc.check(t, r)
			if t.Failed() {
				t.Logf("scenario 7 and summary:\n%s", r.scenario7())
				t.Logf("stub dolt calls: %q", r.doltLog)
			}
		})
	}
}

// runWispSmoke runs the real smoke script in a scratch tree against the stubs.
// doltAbsent leaves dolt out of DOLT_BIN and PATH altogether.
func runWispSmoke(t *testing.T, hostPath string, doltAbsent bool, stub ...string) wispRun {
	t.Helper()
	bash := requireHostTool(t, "bash")
	requireHostTool(t, "git")
	root := sourceRepoRoot(t)
	dir := scratchDir(t)

	files := []string{"scripts/upgrade-smoke-test.sh", ".buildflags"}
	script, err := os.ReadFile(filepath.Join(root, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), "smoke-candidate.sh") {
		files = append(files, "scripts/lib/smoke-candidate.sh")
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	home, tmp, stubs := filepath.Join(dir, "home"), filepath.Join(dir, "tmp"), filepath.Join(dir, "stubs")
	for _, d := range []string{home, tmp, stubs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(stubs, "bd-prev"), wispStubBD)
	writeExecutable(t, filepath.Join(stubs, "bd-cand"), wispStubBD)
	writeExecutable(t, filepath.Join(stubs, "dolt"), wispStubDolt)

	path, doltBin := hostPath, filepath.Join(stubs, "dolt")
	if doltAbsent {
		doltBin = filepath.Join(dir, "absent", "dolt")
	} else {
		path = stubs + string(os.PathListSeparator) + path
	}
	doltLog := filepath.Join(dir, "dolt.log")
	env := append([]string{"PATH=" + path, "HOME=" + home, "TMPDIR=" + tmp,
		"LC_ALL=C", "LANG=C", "ENV=", "BASH_ENV=",
		"PREV_BIN=" + filepath.Join(stubs, "bd-prev"),
		"CANDIDATE_BIN=" + filepath.Join(stubs, "bd-cand"),
		"DOLT_BIN=" + doltBin, "STUB_DOLT_LOG=" + doltLog}, stub...)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "scripts/upgrade-smoke-test.sh", "v1.3.1")
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("smoke script timed out: %s", out)
	}
	if err != nil && exitCode(err) < 0 {
		t.Fatalf("run smoke script: %v", err)
	}
	r := wispRun{out: string(out), exit: exitCode(err)}
	if log, err := os.ReadFile(doltLog); err == nil {
		r.doltLog = string(log)
	}
	r.leftover, _ = filepath.Glob(filepath.Join(tmp, "bd-upgrade-*"))
	return r
}

// scratchDir is a temp directory whose name does not carry the test name: the
// script echoes the stub paths, and a name like readback_unavailable would then
// satisfy the checks on diagnostic wording by itself.
func scratchDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wisp-stub-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// pathWithoutDolt returns the host PATH with no dolt on it. A directory holding
// one is swapped for a farm of symlinks to everything else in it, so the script
// keeps git, mktemp and the rest while the hermetic or system dolt stays out.
func pathWithoutDolt(t *testing.T, farm string) string {
	t.Helper()
	var dirs []string
	for i, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "dolt")); err != nil {
			dirs = append(dirs, dir)
			continue
		}
		// Absolute, so a relative PATH entry cannot leave dangling links.
		abs, err := filepath.Abs(dir)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			t.Fatal(err)
		}
		swapped := filepath.Join(farm, fmt.Sprint(i))
		if err := os.MkdirAll(swapped, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() == "dolt" {
				continue
			}
			if err := os.Symlink(filepath.Join(abs, e.Name()), filepath.Join(swapped, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
		dirs = append(dirs, swapped)
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// sane holds whatever the script under test does: scenarios 1-6 ran green
// against the stubs and scenario 7 got to run.
func (r wispRun) sane(t *testing.T) {
	t.Helper()
	i := strings.Index(r.out, "Wisp plane survives upgrade")
	if i < 0 {
		t.Fatalf("scenario 7 never started:\n%s", r.out)
	}
	if strings.Contains(r.out[:i], "✗") {
		t.Fatalf("a scenario before 7 failed against the stubs:\n%s", r.out)
	}
}

func (r wispRun) scenario7() string {
	if i := strings.Index(r.out, "Wisp plane survives upgrade"); i >= 0 {
		return r.out[i:]
	}
	return r.out
}

// passed: exit 0, the original pass line, and no diagnostic block.
func (r wispRun) passed(t *testing.T) {
	t.Helper()
	r.sane(t)
	if r.exit != 0 {
		t.Errorf("exit = %d, want 0", r.exit)
	}
	r.has(t, wispPersisted, "All 7 scenarios passed")
	r.lacks(t, "SHOW_ERRORED", "READ_PATH_MISS", "WRITE_NOT_VISIBLE",
		wispUpdateOut, wispUpdateErr, wispShowOut, wispShowErr, wispDoltRows)
	if wispUnresolvedRE.MatchString(r.out) {
		t.Errorf("a passing run prints a diagnostic block (it mentions an unresolved classification)")
	}
	r.cleaned(t)
}

// failed: exit 1 with the original failure kept, the run completed, and cleanup done.
func (r wispRun) failed(t *testing.T, original string) {
	t.Helper()
	r.sane(t)
	if r.exit != 1 {
		t.Errorf("exit = %d, want 1", r.exit)
	}
	r.has(t, original, "scenario(s) failed")
	r.lacks(t, wispPersisted)
	r.cleaned(t)
}

func (r wispRun) has(t *testing.T, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(r.out, s) {
			t.Errorf("output lacks %q", s)
		}
	}
}

func (r wispRun) lacks(t *testing.T, unwanted ...string) {
	t.Helper()
	for _, s := range unwanted {
		if strings.Contains(r.out, s) {
			t.Errorf("output holds %q, which the evidence does not establish", s)
		}
	}
}

func (r wispRun) cleaned(t *testing.T) {
	t.Helper()
	if len(r.leftover) > 0 {
		t.Errorf("workspace left behind: %v", r.leftover)
	}
}

// status wants the command's exit status shown next to its name, in whatever
// layout the diagnostic uses.
func (r wispRun) status(t *testing.T, command string, code int) {
	t.Helper()
	re := regexp.MustCompile(fmt.Sprintf(`(?is)(%[1]s.{0,120}\b%[2]d\b|\b%[2]d\b.{0,120}%[1]s)`, regexp.QuoteMeta(command), code))
	if !re.MatchString(r.out) {
		t.Errorf("exit status %d of %s is not shown", code, command)
	}
}

// readbackRan wants a dolt query that found the workspace before it was removed.
func (r wispRun) readbackRan(t *testing.T) {
	t.Helper()
	if !strings.Contains(r.doltLog, "STUB_DOLT_REPO") {
		t.Errorf("no direct readback reached the workspace before cleanup")
	}
}

var (
	wispUnresolvedRE = regexp.MustCompile(`(?i)\bunresolved\b`)
	wispReadbackRE   = regexp.MustCompile(`(?i)\b(direct|readback|dolt)\b`)
	wispWhyRE        = regexp.MustCompile(`(?i)unavailable|not available|not found|not on|missing|cannot|could not|failed|failure|error|exit|status|rc=|skipped`)
)

// unresolved wants the classification left open, in so many words.
func (r wispRun) unresolved(t *testing.T) {
	t.Helper()
	if !wispUnresolvedRE.MatchString(r.out) {
		t.Errorf("output never says the classification is unresolved")
	}
}

// readbackSaid wants a line that says the direct readback gave nothing, and why.
func (r wispRun) readbackSaid(t *testing.T) {
	t.Helper()
	for _, line := range strings.Split(r.out, "\n") {
		if wispReadbackRE.MatchString(line) && wispWhyRE.MatchString(line) {
			return
		}
	}
	t.Errorf("no line says the direct readback was unavailable or failed")
}
