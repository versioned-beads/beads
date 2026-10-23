package compatwindow

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
	"github.com/steveyegge/beads/internal/testutil/bazeltest"
)

// oldWriterSHA names the commit the legacy writer is built from: the upstream
// main commit the participation fence branched from. Its migrations stop at
// 0069, so it knows nothing of the fence's column, and its command line has no
// switch that turns versioned history on, so it never records a version.
const oldWriterSHA = "9cb22b790cb1a7cb1a9af0abc3ec87a1ad1fed2b"

// bdBuildTags is the build tag every bd in this repo is built with; replaytest
// builds the current bd with the same one.
const bdBuildTags = "gms_pure_go"

// requireEnv is set to "1" by the lanes that mean to run these tests. It turns
// a missing prerequisite from a skip into a failure, as replaytest.Require does
// for a missing tool.
const requireEnv = "REPLAY_REQUIRE"

// hatch is the schema-skew escape hatch: it downgrades a binary's refusal to
// open a store whose schema is ahead of it into a warning.
var hatch = []string{"BD_IGNORE_SCHEMA_SKEW=1"}

// ambientSwitches are variables that would change what a bd process observes if
// they leaked in from the machine running the scenario. Each one is set, when it
// is wanted at all, by the scenario itself.
var ambientSwitches = map[string]bool{
	"BD_IGNORE_SCHEMA_SKEW":        true,
	"BD_VERSIONED_HISTORY_ENABLED": true,
}

// baseEnv is the process environment as it was before any test changed it, so
// the Go toolchain building the legacy bd never sees an isolated HOME.
var baseEnv = os.Environ()

// cleanEnv is the environment for every bd the scenario starts: the harness's
// sanitized environment, without the ambient switches, plus extra.
func cleanEnv(extra ...string) []string {
	base := doltcli.SanitizedEnv(os.Environ())
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		if name, _, _ := strings.Cut(kv, "="); !ambientSwitches[name] {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

// missing ends the test when a prerequisite is absent: a failure in a lane that
// requires the replay tests to run, a skip anywhere else.
func missing(t testing.TB, what string) {
	t.Helper()
	if os.Getenv(requireEnv) == "1" {
		t.Fatalf("%s, and %s=1 says this lane requires it", what, requireEnv)
	}
	t.Skipf("%s; set %s=1 to fail instead of skipping", what, requireEnv)
}

// workspaceAbove returns the nearest strict ancestor of dir that holds a .beads
// directory, or "" when there is none. bd looks upward from its working
// directory for a workspace, and neither a scrubbed environment nor BEADS_DIR
// stops that search, so a project below such an ancestor can reach a workspace
// the test never created.
func workspaceAbove(dir string) string {
	for d := filepath.Dir(filepath.Clean(dir)); ; d = filepath.Dir(d) {
		if info, err := os.Stat(filepath.Join(d, ".beads")); err == nil && info.IsDir() {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// requireNoWorkspaceAbove stops the test before any bd runs when a workspace the
// test did not create sits above dir.
func requireNoWorkspaceAbove(t testing.TB, dir string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving %s: %v", dir, err)
	}
	if ws := workspaceAbove(resolved); ws != "" {
		t.Fatalf("%s holds a .beads workspace above the test's temporary directory %s; bd would use it instead of the fixture. Point TMPDIR at a directory outside any beads workspace", ws, resolved)
	}
}

var oldBuild struct {
	once   sync.Once
	dir    string
	bin    string
	schema int
	prereq string
	err    error
}

// oldWriter returns the legacy bd, built once per test binary from oldWriterSHA,
// and the highest schema version that tree knows. It is built from that commit's
// own source, never taken from PATH. A checkout that lacks the commit (a shallow
// clone) cannot build it, which is a missing prerequisite.
func oldWriter(t testing.TB, root string) (bin string, schema int) {
	t.Helper()
	oldBuild.once.Do(func() { buildOldWriter(root) })
	if oldBuild.prereq != "" {
		missing(t, oldBuild.prereq)
	}
	if oldBuild.err != nil {
		t.Fatalf("%v", oldBuild.err)
	}
	return oldBuild.bin, oldBuild.schema
}

func buildOldWriter(root string) {
	if _, err := exec.LookPath("git"); err != nil {
		oldBuild.prereq = "building the legacy bd needs git on PATH"
		return
	}
	if out, err := exec.Command("git", "-C", root, "cat-file", "-e", oldWriterSHA+"^{commit}").CombinedOutput(); err != nil {
		oldBuild.prereq = fmt.Sprintf("commit %s is not in this checkout (a shallow clone drops it: fetch the full history): %v: %s", oldWriterSHA, err, bytes.TrimSpace(out))
		return
	}
	dir, err := os.MkdirTemp("", "compatwindow-oldwriter-")
	if err != nil {
		oldBuild.err = err
		return
	}
	oldBuild.dir = dir
	src := filepath.Join(dir, "src")
	if err := extractCommit(root, oldWriterSHA, src); err != nil {
		oldBuild.err = fmt.Errorf("extracting %s: %w", oldWriterSHA, err)
		return
	}
	schema, err := highestMigration(src)
	if err != nil {
		oldBuild.err = err
		return
	}
	bin := filepath.Join(dir, "bd")
	goBin, err := replaytest.GoBin()
	if err != nil {
		oldBuild.err = fmt.Errorf("building bd from %s: %w", oldWriterSHA, err)
		return
	}
	cmd := exec.Command(goBin, "build", "-tags", bdBuildTags, "-o", bin, "./cmd/bd")
	cmd.Dir = src
	cmd.Env = baseEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		oldBuild.err = fmt.Errorf("building bd from %s: %w\n%s", oldWriterSHA, err, out)
		return
	}
	oldBuild.bin, oldBuild.schema = bin, schema
}

// extractCommit writes the tree of commit sha, as git archive reports it, into
// dst. Only directories and regular files are written: symlinks and the
// archive's own header carry nothing the build reads.
func extractCommit(root, sha, dst string) error {
	cmd := exec.Command("git", "-C", root, "archive", "--format=tar", sha)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := untar(tar.NewReader(stdout), dst); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func untar(tr *tar.Reader, dst string) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if !filepath.IsLocal(hdr.Name) {
			return fmt.Errorf("archive entry %q is outside the destination", hdr.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			perm := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				perm = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			if err := f.Close(); copyErr == nil {
				copyErr = err
			}
			if copyErr != nil {
				return copyErr
			}
		}
	}
}

// highestMigration is the highest numbered migration in the main series of the
// tree at src, which is the schema version a bd built from it knows.
func highestMigration(src string) (int, error) {
	files, err := filepath.Glob(filepath.Join(src, "internal", "storage", "schema", "migrations", "*.up.sql"))
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, f := range files {
		digits, _, ok := strings.Cut(filepath.Base(f), "_")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(digits); err == nil {
			highest = max(highest, n)
		}
	}
	if highest == 0 {
		return 0, fmt.Errorf("no numbered migrations under %s", src)
	}
	return highest, nil
}

// jsonObject returns the first JSON object in out that carries key. bd prints a
// structured error on one stream or the other and other lines may share it, so
// the object is located rather than assumed to be the whole output.
func jsonObject(out, key string) map[string]any {
	for i, r := range out {
		if r != '{' {
			continue
		}
		var obj map[string]any
		if err := json.NewDecoder(strings.NewReader(out[i:])).Decode(&obj); err != nil {
			continue
		}
		if _, ok := obj[key]; ok {
			return obj
		}
	}
	return nil
}

// recordState is what the scenario observes about one record: its plain row
// and its history, read straight from the store. participation_generation is
// kept as a cell so NULL (a record that takes no part in history) is never
// confused with a value.
type recordState struct {
	Title      string
	Revision   string
	Generation doltcli.Cell
	Versions   int
}

func (s recordState) String() string {
	generation := "NULL"
	if !s.Generation.Null {
		generation = s.Generation.Text
	}
	return fmt.Sprintf("title=%q current_revision=%s participation_generation=%s version_rows=%d", s.Title, s.Revision, generation, s.Versions)
}

// compat is one store shared by two real bd processes: the current binary,
// built from this tree with versioned history switched on in the store, and the
// legacy binary built from oldWriterSHA.
type compat struct {
	t         *testing.T
	newBin    string
	oldBin    string
	oldSchema int
	dbSchema  int
	proj      string
	data      string
	// participating is a record the current writer created with history on, so
	// it carries a participation generation and one version row.
	participating string
}

// newCompat builds the shared store. The current writer initializes it, turns
// history on and creates one record; the fixture then checks that history really
// is recording, because with it off the fence never runs and every later
// assertion would pass without testing anything.
func newCompat(t *testing.T) *compat {
	t.Helper()
	replaytest.Require(t, replaytest.NeedDolt|replaytest.NeedBd)
	root := bazeltest.RepoRoot(t)
	replaytest.Isolate(t)
	requireNoWorkspaceAbove(t, t.TempDir())

	c := &compat{t: t, newBin: replaytest.BdBin(t)}
	c.oldBin, c.oldSchema = oldWriter(t, root)
	c.proj = replaytest.InitBdProject(t, "compat", c.newBin)
	c.data = replaytest.DataDir(t, c.proj)
	c.requireOwnWorkspace(c.newBin)
	c.requireOwnWorkspace(c.oldBin)

	replaytest.RunBd(t, c.newBin, c.proj, "config", "set", "versioned-history.enabled", "true")
	c.participating = c.create(c.newBin, nil, "record the current writer created")
	c.dbSchema = c.schemaVersion()
	if c.dbSchema <= c.oldSchema {
		t.Fatalf("the store is at schema v%d and the legacy binary knows up to v%d: there is no skew to observe", c.dbSchema, c.oldSchema)
	}
	state := c.record(c.participating)
	if state.Versions != 1 || state.Generation.Null {
		t.Fatalf("versioned history is not recording in the fixture, so the fence would never run: %v", state)
	}
	return c
}

// requireOwnWorkspace asks bin which workspace it resolved from the project
// directory and stops unless it is the fixture's own. It runs before the first
// write by either binary.
func (c *compat) requireOwnWorkspace(bin string) {
	c.t.Helper()
	out, code := c.run(bin, nil, "where", "--json")
	if code != 0 {
		c.t.Fatalf("%s where --json exited %d:\n%s", filepath.Base(bin), code, out)
	}
	where := jsonObject(out, "path")
	got, _ := where["path"].(string)
	if got == "" {
		c.t.Fatalf("%s where --json reported no workspace path:\n%s", filepath.Base(bin), out)
	}
	resolvedGot, errGot := filepath.EvalSymlinks(got)
	resolvedWant, errWant := filepath.EvalSymlinks(filepath.Join(c.proj, ".beads"))
	if errGot != nil || errWant != nil || resolvedGot != resolvedWant {
		c.t.Fatalf("bd resolved the workspace %s, not the fixture's %s: stopping before any write", got, filepath.Join(c.proj, ".beads"))
	}
}

// run starts bin in the project directory with extra added to the scenario's
// clean environment, and returns its combined output and exit code.
func (c *compat) run(bin string, extra []string, args ...string) (string, int) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = c.proj
	cmd.Env = cleanEnv(extra...)
	out, err := cmd.CombinedOutput()
	// A process killed at the deadline reports an exit error too, and a refusal
	// is exactly a non-zero exit, so a hang must not be allowed to pass as one.
	if ctx.Err() != nil {
		c.t.Fatalf("%s %s did not finish in time:\n%s", filepath.Base(bin), strings.Join(args, " "), out)
	}
	if err == nil {
		return string(out), 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		c.t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return string(out), exit.ExitCode()
}

// create makes a record with bin and returns its id; a failure fails the test.
func (c *compat) create(bin string, extra []string, title string) string {
	c.t.Helper()
	out, code := c.run(bin, extra, "create", title, "--json")
	if code != 0 {
		c.t.Fatalf("%s create exited %d:\n%s", filepath.Base(bin), code, out)
	}
	return replaytest.JSONID(c.t, out)
}

// update retitles a record with bin and returns the output and exit code.
func (c *compat) update(bin string, extra []string, id, title string) (string, int) {
	c.t.Helper()
	return c.run(bin, extra, "update", id, "--title", title)
}

// legacyRecord has the legacy writer, admitted by the hatch, create a record.
// Nothing stamped it as taking part in history, and the fixture checks that.
func (c *compat) legacyRecord() string {
	c.t.Helper()
	id := c.create(c.oldBin, hatch, "record the legacy writer created")
	state := c.record(id)
	if !state.Generation.Null || state.Versions != 0 {
		c.t.Fatalf("a record the legacy writer created should take no part in history: %v", state)
	}
	return id
}

// schemaVersion is the highest migration the store has applied.
func (c *compat) schemaVersion() int {
	c.t.Helper()
	_, rows, err := doltcli.Query(context.Background(), c.data, "SELECT MAX(version) FROM schema_migrations")
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		c.t.Fatalf("reading the store's schema version: rows=%v err=%v", rows, err)
	}
	n, err := strconv.Atoi(rows[0][0].Text)
	if err != nil {
		c.t.Fatalf("schema version %q: %v", rows[0][0].Text, err)
	}
	return n
}

// record reads one record's plain row and its version rows straight from the
// store, through the harness's NULL-faithful reader. The plain row alone cannot
// show a version that is missing.
func (c *compat) record(id string) recordState {
	c.t.Helper()
	ctx := context.Background()
	_, rows, err := doltcli.Query(ctx, c.data,
		"SELECT title, current_revision, participation_generation FROM issues WHERE id = "+doltcli.SQLQuote(id))
	if err != nil || len(rows) != 1 || len(rows[0]) != 3 {
		c.t.Fatalf("reading record %s: rows=%v err=%v", id, rows, err)
	}
	_, counts, err := doltcli.Query(ctx, c.data,
		"SELECT COUNT(*) FROM issue_versions WHERE issue_id = "+doltcli.SQLQuote(id))
	if err != nil || len(counts) != 1 || len(counts[0]) != 1 {
		c.t.Fatalf("counting versions of %s: rows=%v err=%v", id, counts, err)
	}
	versions, err := strconv.Atoi(counts[0][0].Text)
	if err != nil {
		c.t.Fatalf("version count %q: %v", counts[0][0].Text, err)
	}
	return recordState{
		Title:      rows[0][0].Text,
		Revision:   rows[0][1].Text,
		Generation: rows[0][2],
		Versions:   versions,
	}
}
