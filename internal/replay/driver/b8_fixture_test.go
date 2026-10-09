package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
	"github.com/steveyegge/beads/internal/storage/schema"
	"github.com/steveyegge/beads/internal/testutil/bazeltest"
)

// The seeding tests need an oracle whose store is behind the integration's schema,
// and one that carries everything a clone of a real store carries: a base that
// holds issues, a clone-local plane with rows in every table, a remote, a backup,
// an identity. A hand-written one would encode a guess about all of that, so this
// one is built by a real, older bd, with the verbs a user would run, and by dolt
// itself where bd has no verb. It is synthetic from the first byte: nothing in it
// is derived from any corpus.

// seedVariant is one shape of the synthetic oracle.
type seedVariant struct {
	name string
	// bigNumber makes one base issue hold a number in its metadata that the older bd
	// stores rounded, and that the integration refuses to turn versioned history on
	// over.
	bigNumber bool
	// noIdentity leaves the base's store without a project identity.
	noIdentity bool
}

var (
	defaultSeedVariant = seedVariant{name: "plain"}
	bigNumberVariant   = seedVariant{name: "bignum", bigNumber: true}
	noIdentityVariant  = seedVariant{name: "noid", noIdentity: true}
)

// seedOracle is a synthetic oracle and the facts about it a test needs.
type seedOracle struct {
	// root holds the oracle project and the remote and the backup it points at.
	root    string
	project string
	data    string
	remote  string
	backup  string
	// base is the commit the seeded store starts from. ids are the issues that
	// exist there, in creation order.
	base string
	ids  []string
	// tailID is the issue the oracle creates after the base.
	tailID string
}

var seedShared struct {
	mu    sync.Mutex
	built map[string]*seedOracle
}

// sharedSeedOracle builds the variant once per test binary and hands the same one
// to every test that asks. A test only ever reads it: the seeding copies from it,
// and a test that changes a store changes its own copy.
func sharedSeedOracle(t testing.TB, v seedVariant) *seedOracle {
	t.Helper()
	replaytest.Require(t, replaytest.NeedDolt|replaytest.NeedBd)
	seedShared.mu.Lock()
	defer seedShared.mu.Unlock()
	if o, ok := seedShared.built[v.name]; ok {
		return o
	}
	shared.mu.Lock()
	root := fixtureRoot(t)
	shared.mu.Unlock()
	o := buildSeedOracle(t, filepath.Join(root, "seed-"+v.name), v)
	if seedShared.built == nil {
		seedShared.built = map[string]*seedOracle{}
	}
	seedShared.built[v.name] = o
	return o
}

// buildSeedOracle writes the oracle under root, in the isolated environment of the
// test that first needs it. Everything it leaves behind lives under root.
//
// The order matters, and each step is there for a reason:
//   - the claim comes before the base, because a claim sets started_at, a column the
//     translator has no flag for: a claim in the replayed tail would make its issue
//     untranslatable.
//   - the base is an empty commit whose message the walk reads as a bootstrap commit.
//     The walk starts at the last such commit, so this is what makes the base hold
//     three issues; without it the walk would start before they existed.
//   - the wisp verbs write the clone-local plane and make no commit, so they leave
//     the walk alone.
//   - the journal is switched on after the base, so the tail leaves journal rows.
//   - the tail is three verbs the translator can replay.
func buildSeedOracle(t testing.TB, root string, v seedVariant) *seedOracle {
	t.Helper()
	old := replaytest.OldBd(t)
	replaytest.Isolate(t)
	t.Setenv("BD_BACKUP_ENABLED", "0")
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("clearing %s: %v", root, err)
	}
	o := &seedOracle{
		root:    root,
		project: filepath.Join(root, "oracle"),
		remote:  filepath.Join(root, "remote"),
		backup:  filepath.Join(root, "backup"),
	}
	for _, dir := range []string{o.project, o.remote, o.backup} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	bd := func(args ...string) string {
		t.Helper()
		return replaytest.RunBd(t, old, o.project, args...)
	}
	bd("init", "--non-interactive", "--role=maintainer")
	o.data = replaytest.DataDir(t, o.project)

	for _, title := range []string{"seed issue a", "seed issue b", "seed issue c"} {
		o.ids = append(o.ids, replaytest.JSONID(t, bd("create", title, "-p", "2", "-t", "task", "--json")))
	}
	bd("update", o.ids[0], "--claim")
	if v.bigNumber {
		// 2^53+1 is a number a float cannot hold. The older bd keeps 2^53, which a
		// version cannot tell from its neighbour, and that is what the integration
		// refuses to record.
		bd("update", o.ids[2], "--metadata", `{"n":9007199254740993}`)
	}
	replaytest.RunDolt(t, o.data, "commit", "--allow-empty", "-m", "schema: apply migrations")
	o.base = replaytest.HeadCommit(t, o.data)

	one := replaytest.JSONID(t, bd("create", "wisp one", "--ephemeral", "--json"))
	bd("comments", "add", one, "a comment")
	bd("label", "add", one, "lbl")
	two := replaytest.JSONID(t, bd("create", "wisp two", "--ephemeral", "--json"))
	bd("dep", "add", one, two)
	bd("create", "wisp child", "--ephemeral", "--parent", one, "--json")

	bd("config", "set", "events-journal", "true")
	bd("update", o.ids[0], "--description", "tail description")
	bd("update", o.ids[1], "--title", "tail title")
	o.tailID = replaytest.JSONID(t, bd("create", "tail issue", "-p", "2", "-t", "task", "--json"))

	if v.noIdentity {
		// The identity goes last, in a commit of its own that the walk reads as the
		// base, so that nothing the older bd does afterwards has to run on a store
		// without one. A seeding of this variant has a base and no tail.
		replaytest.RunDolt(t, o.data, "sql", "-q", "DELETE FROM metadata WHERE `key` = '_project_id'")
		replaytest.RunDolt(t, o.data, "add", "-A")
		replaytest.RunDolt(t, o.data, "commit", "-m", "schema: drop the project identity")
		o.base = replaytest.HeadCommit(t, o.data)
	}

	// repo_mtimes has one writer, multi-repo hydration, which a synthetic oracle has
	// no use for; a row written by hand makes the table non-empty.
	replaytest.RunDolt(t, o.data, "sql", "-q", "INSERT INTO repo_mtimes (repo_path, jsonl_path, mtime_ns) VALUES ('/synthetic/repo', '/synthetic/repo/issues.jsonl', 1)")

	replaytest.RunDolt(t, o.data, "remote", "add", "origin", "file://"+o.remote)
	replaytest.RunDolt(t, o.data, "backup", "add", "bk", "file://"+o.backup)
	syncFixtureBackup(t, o.data, "bk")
	return o
}

// syncFixtureBackup writes the oracle's state to its backup destination. It is a
// fixture step, the one place a test makes a backup on purpose, so that the
// destination holds real files for the seeding to leave alone.
func syncFixtureBackup(t testing.TB, dataDir, name string) {
	t.Helper()
	replaytest.RunDolt(t, dataDir, "backup", "sync", name)
}

// seedQueryInt runs a statement that returns one integer.
func seedQueryInt(t testing.TB, dir, sql string) int {
	t.Helper()
	text := seedQueryText(t, dir, sql)
	n, err := strconv.Atoi(text)
	if err != nil {
		t.Fatalf("%s: %q is not an integer", sql, text)
	}
	return n
}

// seedQueryText runs a statement that returns one value.
func seedQueryText(t testing.TB, dir, sql string) string {
	t.Helper()
	_, rows, err := doltcli.Query(context.Background(), dir, sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: expected one value, got %v", sql, rows)
	}
	return rows[0][0].Text
}

// seedCount is the number of rows of a table.
func seedCount(t testing.TB, dir, table string) int {
	t.Helper()
	return seedQueryInt(t, dir, "SELECT COUNT(*) FROM "+table)
}

// seedClearedNames is the test's own statement of the tables a seeding empties:
// the clone-local plane a fresh clone does not carry. It is kept apart from the
// seeding's policy so that a mistake there is not copied here.
var seedClearedNames = []string{"events", "bd_events_journal", "leases", "local_metadata", "repo_mtimes", "wisps"}

// seedClears reports whether a seeding empties the table: the named ones and every
// wisp_ table.
func seedClears(name string) bool {
	return slices.Contains(seedClearedNames, name) || strings.HasPrefix(name, "wisp_")
}

// ignoredTables lists the tables of the store at dir that dolt_ignore matches,
// with the predicate the product uses to tell a clone-local table from a tracked
// one: a pattern whose ignored flag is set, matched with LIKE.
func ignoredTables(t testing.TB, dir string) []string {
	t.Helper()
	_, rows, err := doltcli.Query(context.Background(), dir, `
		SELECT t.TABLE_NAME FROM INFORMATION_SCHEMA.TABLES t
		WHERE t.TABLE_SCHEMA = DATABASE() AND t.TABLE_TYPE = 'BASE TABLE'
		  AND EXISTS (SELECT 1 FROM dolt_ignore di WHERE di.ignored = 1 AND t.TABLE_NAME LIKE di.pattern)
		ORDER BY t.TABLE_NAME`)
	if err != nil {
		t.Fatalf("listing the ignored tables of %s: %v", dir, err)
	}
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r[0].Text
	}
	// The store's collation puts "wisps" ahead of "wisp_...", byte order does not:
	// sort here so that a comparison does not depend on either.
	slices.Sort(names)
	return names
}

// seedProjectID reads the identity the store at dir carries in its metadata table.
func seedProjectID(t testing.TB, dir string) string {
	t.Helper()
	_, rows, err := doltcli.Query(context.Background(), dir, "SELECT value FROM metadata WHERE `key` = '_project_id'")
	if err != nil {
		t.Fatalf("reading the project id of %s: %v", dir, err)
	}
	if len(rows) != 1 || rows[0][0].Text == "" {
		t.Fatalf("%s holds no single _project_id: %v", dir, rows)
	}
	return rows[0][0].Text
}

// repoLinkedEngine is the dolt engine this tree links, read from go.mod the way a
// reader placing a result would read it.
func repoLinkedEngine(t testing.TB) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(bazeltest.RepoRoot(t), "go.mod")) // #nosec G304 -- this tree's own go.mod
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	const engine = "github.com/dolthub/dolt/go"
	for _, line := range splitLines(string(data)) {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == engine {
			return f[0] + " " + f[1]
		}
	}
	t.Fatalf("go.mod has no %s requirement", engine)
	return ""
}

// newSeedConfig is the configuration of a seeding of o into a fresh work directory
// of the calling test, with the dolt on PATH and the bd built from this tree, both
// named by path. The latest versions come from the schema package, which is the
// product's own answer and not the harness's.
func newSeedConfig(t testing.TB, o *seedOracle) SeedConfig {
	t.Helper()
	return seedConfigAt(t, o, t.TempDir())
}

// seedConfigAt is newSeedConfig with the directory the work project and the
// seeding's own environment live under named by the caller.
func seedConfigAt(t testing.TB, o *seedOracle, root string) SeedConfig {
	t.Helper()
	replaytest.Isolate(t)
	doltBin, err := doltcli.Path()
	if err != nil {
		t.Fatalf("dolt: %v", err)
	}
	return SeedConfig{
		OracleDataDir: o.data,
		Base:          o.base,
		WorkDir:       filepath.Join(root, "work"),
		EnvRoot:       filepath.Join(root, "env"),
		DoltBin:       doltBin,
		BdBin:         replaytest.BdBin(t),
		LatestSchema:  schema.LatestVersion(),
		LatestIgnored: schema.LatestIgnoredVersion(),
		LinkedEngine:  repoLinkedEngine(t),
	}
}

// seedRemotes and seedBackups list what the dolt CLI says the store at dir points
// at, one line per target. They go through the CLI the harness itself uses.
func seedRemotes(t testing.TB, dir string) []string { return seedTargets(t, dir, "remote") }
func seedBackups(t testing.TB, dir string) []string { return seedTargets(t, dir, "backup") }

func seedTargets(t testing.TB, dir, kind string) []string {
	t.Helper()
	out := replaytest.RunDolt(t, dir, kind, "-v")
	var lines []string
	for _, line := range splitLines(out) {
		if isDoltNoise(line) {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if line := s[start:i]; line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// isDoltNoise reports a line the dolt CLI prints that is not part of the answer:
// its version-check warning.
func isDoltNoise(line string) bool {
	return len(line) >= 8 && line[:8] == "Warning:"
}

// treeSums lists every regular file under dir with its size and the hash of its
// contents. A read through the dolt CLI touches a few files' modification times
// without changing them, so contents, not times, say whether a tree was written.
func treeSums(t testing.TB, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path) // #nosec G304 -- a path under the fixture's own directory
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, _ := filepath.Rel(dir, path)
		out[rel] = fmt.Sprintf("%d:%s", len(data), hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("summing %s: %v", dir, err)
	}
	return out
}

// treeTimes lists every regular file under dir with its size and modification
// time, which is what a destination nothing reads must keep.
func treeTimes(t testing.TB, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[rel] = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	return out
}

// sameTree fails the test unless the two listings are identical.
func sameTree(t testing.TB, what string, before, after map[string]string) {
	t.Helper()
	for name, was := range before {
		now, ok := after[name]
		switch {
		case !ok:
			t.Errorf("%s: %s is gone", what, name)
		case now != was:
			t.Errorf("%s: %s changed (%s, was %s)", what, name, now, was)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("%s: %s is new", what, name)
		}
	}
}
