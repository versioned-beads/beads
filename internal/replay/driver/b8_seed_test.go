package driver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// ---- stopping a seeding where the copy is prepared -------------------------------

// errStopSeed is what a test's observer returns at SeedMigrating to end a seeding
// at the one moment the copy is as the recipe prepared it: nothing has opened it
// with bd, so nothing has migrated it.
var errStopSeed = errors.New("stop the seeding before the migration")

// seedUntilMigration runs a seeding that ends at SeedMigrating, after atMigration
// (when it is not nil) has run there, and returns what the seeding returned. A
// seeding that ends this way is the prepared copy and nothing more, and an error
// the observer returns must come back out of Seed for errors.Is to find it.
func seedUntilMigration(t *testing.T, cfg SeedConfig, atMigration func()) error {
	t.Helper()
	next := cfg.Observer
	cfg.Observer = ObserverFunc(func(ctx context.Context, ev Event) error {
		if next != nil {
			if err := next.Observe(ctx, ev); err != nil {
				return err
			}
		}
		if _, ok := ev.(SeedMigrating); ok {
			if atMigration != nil {
				atMigration()
			}
			return errStopSeed
		}
		return nil
	})
	_, err := Seed(context.Background(), cfg)
	return err
}

// ---- what a test reads off a store ----------------------------------------------

// seedStoreFacts is what the tests compare between two moments of one store. It
// is read with the dolt CLI from outside, the way the recipe measures.
type seedStoreFacts struct {
	// Version and IgnoredVersion are the newest migration of each track, and
	// CursorRows the rows of the ignored track's cursor table.
	Version        int
	IgnoredVersion int
	CursorRows     int
	// LogLen is the commits in the history, Head the newest, Issues the rows of the
	// issues table, and Clean whether the working set has nothing to commit.
	LogLen int
	Head   string
	Issues int
	Clean  bool
	// Seq is the rows of the event counter, as id:next_seq, and Cleared the rows left
	// in each table a seeding empties.
	Seq     []string
	Cleared map[string]int
}

func readSeedStoreFacts(t testing.TB, data string) seedStoreFacts {
	t.Helper()
	f := seedStoreFacts{
		Version:        seedQueryInt(t, data, "SELECT MAX(version) FROM schema_migrations"),
		IgnoredVersion: seedQueryInt(t, data, "SELECT MAX(version) FROM ignored_schema_migrations"),
		CursorRows:     seedCount(t, data, "ignored_schema_migrations"),
		LogLen:         seedCount(t, data, "dolt_log"),
		Head:           seedQueryText(t, data, "SELECT hashof('HEAD')"),
		Issues:         seedCount(t, data, "issues"),
		Clean:          strings.Contains(replaytest.RunDolt(t, data, "status"), "nothing to commit"),
		Cleared:        map[string]int{},
	}
	_, rows, err := doltcli.Query(context.Background(), data, "SELECT id, next_seq FROM bd_events_seq ORDER BY id")
	if err != nil {
		t.Fatalf("reading the event counter of %s: %v", data, err)
	}
	for _, r := range rows {
		f.Seq = append(f.Seq, r[0].Text+":"+r[1].Text)
	}
	for _, name := range ignoredTables(t, data) {
		if seedClears(name) {
			f.Cleared[name] = seedCount(t, data, name)
		}
	}
	return f
}

// seedMigratingFacts is what the flow samples at SeedMigrating: how many children
// had run, and the copy's project file, its .beads entries and its store.
type seedMigratingFacts struct {
	children int
	metadata []byte
	entries  []string
	store    seedStoreFacts
}

func seedSnapshotMigrating(t testing.TB, cfg SeedConfig) seedMigratingFacts {
	t.Helper()
	m := seedMigratingFacts{children: len(cfg.Log.Children())}
	dir := filepath.Join(cfg.WorkDir, ".beads")
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json")) // #nosec G304 -- the test's own work directory
	if err != nil {
		t.Fatalf("reading the seeded project file: %v", err)
	}
	m.metadata = data
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	for _, e := range entries {
		m.entries = append(m.entries, e.Name())
	}
	m.store = readSeedStoreFacts(t, replaytest.DataDir(t, cfg.WorkDir))
	return m
}

// ---- stand-ins that record the environment a child received ---------------------

// seedDumpBlock is one child as its stand-in saw it: the label the stand-in was
// written with and the environment the process was started with.
type seedDumpBlock struct {
	label string
	env   map[string]string
}

var seedEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// seedWriteStandIn writes an executable sh script named label in dir. When it runs
// it appends its label and the environment it was started with to the dump file,
// and then runs the real tool, by the absolute path it was given, with the same
// arguments. The script looks nothing up on PATH, so it runs under any
// environment.
func seedWriteStandIn(t testing.TB, dir, label, real, dump string) string {
	t.Helper()
	envBin, err := exec.LookPath("env")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	path := filepath.Join(dir, label)
	script := "#!/bin/sh\n" +
		"{ printf '### %s\\n' " + shellQuote(label) + "; " + shellQuote(envBin) + "; } >> " + shellQuote(dump) + "\n" +
		"exec " + shellQuote(real) + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { // #nosec G306 -- a test stand-in must be executable
		t.Fatalf("writing the %s stand-in: %v", label, err)
	}
	return path
}

// parseSeedDump reads the dump file the stand-ins wrote, one block per child, in
// the order the children ran. A missing file is no children.
func parseSeedDump(t testing.TB, path string) []seedDumpBlock {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- the flow's own dump file
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("opening %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	var blocks []seedDumpBlock
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for sc.Scan() {
		line := sc.Text()
		if label, ok := strings.CutPrefix(line, "### "); ok {
			blocks = append(blocks, seedDumpBlock{label: label, env: map[string]string{}})
			continue
		}
		if len(blocks) == 0 {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !seedEnvName.MatchString(k) {
			continue // the rest of a value that holds a newline
		}
		blocks[len(blocks)-1].env[k] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return blocks
}

// seedEnvMap turns an environment in KEY=value form into a map.
func seedEnvMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// seedWithoutShellNames drops the variables a shell adds to what it runs, which a
// stand-in's environment dump cannot tell from the child's own.
func seedWithoutShellNames(env map[string]string) map[string]string {
	out := maps.Clone(env)
	for _, k := range []string{"PWD", "OLDPWD", "SHLVL", "_"} {
		delete(out, k)
	}
	return out
}

// seedUnder reports whether path is root or lies inside it.
func seedUnder(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// seedDecoyPortNames are the two variables that would point a child at a shared
// dolt server, which is the thing the seeding and the replay must never reach.
var seedDecoyPortNames = []string{"BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"}

// ---- the shared seeded flow ------------------------------------------------------

// seededFlow is one seeding of a synthetic oracle and one replay on the copy it
// made, run once per test binary and read by every test that needs the result. A
// real seeding migrates a store and a real replay writes through bd, and neither
// is cheap; the tests that need to see them happen read the same one.
//
// It runs with everything a careless harness would pick up from its caller set in
// the process: the three variables that switch the product's checks off, a canary
// that nothing reads, and the two that name a shared dolt server, which is a
// decoy listener that must hear nothing. Its children are wrapped in stand-ins
// that record the environment they were started with.
type seededFlow struct {
	// done is set when the flow was built to the end, and failure says where it
	// stopped when it was not.
	done    bool
	stage   string
	failure string

	o   *seedOracle
	cfg SeedConfig
	rec SeedRecord
	// seedErr and runErr are what the seeding and the replay returned.
	seedErr error
	runErr  error

	// children are the dolt and bd children of the seeding, in order, as its Runner
	// logged them; events are the seed events its observer heard, in order.
	children          []doltcli.Child
	events            []string
	migrating         seedMigratingFacts
	notes             []string
	completedChildren int
	completedRec      SeedRecord

	// dump is the stand-ins' record of every child's environment. ambient names what
	// the flow set in the process, and ambientHome is the process's own HOME.
	dump        string
	ambient     []string
	ambientHome string

	// The replay: its configuration, what it returned and wrote, and what the
	// observer heard.
	runCfg     RunConfig
	run        ReplayRun
	results    []CommitReplayResult
	mismatches []Mismatch
	summary    Summary
	summaryRaw []byte
	summaryErr error
	steps      []StepIssueResult
	walkSteps  []Step

	// The work store after the replay, and the oracle's and the backup's trees
	// around the whole flow.
	workIssues     int
	legacyRows     int
	remotesAfter   []string
	backupsAfter   []string
	oracleBefore   map[string]string
	oracleAfter    map[string]string
	backupBefore   map[string]string
	backupAfter    map[string]string
	decoyAccepted  int
	decoyListening bool
}

var seedFlows struct {
	mu    sync.Mutex
	built map[string]*seededFlow
}

// sharedSeededFlow builds the flow for a variant of the oracle the first time a
// test asks and hands the same one to every later test. The test that builds it
// owns the process environment the flow sets; the tests that read it must isolate
// their own before they run a tool.
func sharedSeededFlow(t *testing.T, v seedVariant) *seededFlow {
	t.Helper()
	replaytest.Require(t, replaytest.NeedDolt|replaytest.NeedBd)
	seedFlows.mu.Lock()
	defer seedFlows.mu.Unlock()
	if f, ok := seedFlows.built[v.name]; ok {
		return f
	}
	f := &seededFlow{}
	if seedFlows.built == nil {
		seedFlows.built = map[string]*seededFlow{}
	}
	seedFlows.built[v.name] = f
	defer func() {
		if !f.done {
			f.failure = "the test that built it stopped while " + f.stage
		}
	}()
	buildSeededFlow(t, f, v)
	f.done = true
	return f
}

// requireSeedFlow is the flow for a test that reads the seeding: it fails the test
// when the flow could not be built or the seeding did not complete.
func requireSeedFlow(t *testing.T, v seedVariant) *seededFlow {
	t.Helper()
	f := sharedSeededFlow(t, v)
	if f.failure != "" {
		t.Fatalf("the shared seeding flow could not be built: %s", f.failure)
	}
	if f.seedErr != nil {
		t.Fatalf("the seeding did not complete: %v", f.seedErr)
	}
	return f
}

// requireSeededRun is requireSeedFlow for a test that reads the replay too.
func requireSeededRun(t *testing.T, v seedVariant) *seededFlow {
	t.Helper()
	f := requireSeedFlow(t, v)
	if f.runErr != nil {
		t.Fatalf("the replay on the seeded copy did not complete: %v", f.runErr)
	}
	if f.summaryErr != nil {
		t.Fatalf("the replay's summary could not be read: %v", f.summaryErr)
	}
	return f
}

func buildSeededFlow(t *testing.T, f *seededFlow, v seedVariant) {
	t.Helper()
	ctx := context.Background()
	f.stage = "building the oracle"
	o := sharedSeedOracle(t, v)
	f.o = o

	f.stage = "preparing the flow's directories"
	shared.mu.Lock()
	base := fixtureRoot(t)
	shared.mu.Unlock()
	root := filepath.Join(base, "flow-"+v.name)
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("clearing %s: %v", root, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("creating %s: %v", root, err)
	}
	f.dump = filepath.Join(root, "children.txt")
	realDolt, err := doltcli.Path()
	if err != nil {
		t.Fatalf("dolt: %v", err)
	}
	realBd := replaytest.BdBin(t)
	standIns := filepath.Join(root, "stand-ins")
	doltIn := seedWriteStandIn(t, standIns, "dolt", realDolt, f.dump)
	bdIn := seedWriteStandIn(t, standIns, "bd", realBd, f.dump)
	runIn := seedWriteStandIn(t, standIns, "run-bd", realBd, f.dump)

	// The replay's configuration comes from the fixture every other replay test
	// uses, so that a field the run needs is set the way those tests set it.
	fr := newFixtureRun(t, &oracleHistory{data: o.data}, runIn)
	cfg := seedConfigAt(t, o, root)
	cfg.DoltBin, cfg.BdBin = doltIn, bdIn
	cfg.Log = &doltcli.ChildLog{}
	cfg.Note = func(s string) { f.notes = append(f.notes, s) }
	cfg.Observer = ObserverFunc(func(_ context.Context, ev Event) error {
		switch e := ev.(type) {
		case SeedMigrating:
			f.events = append(f.events, "migrating")
			f.migrating = seedSnapshotMigrating(t, cfg)
		case SeedCompleted:
			f.events = append(f.events, "completed")
			f.completedChildren = len(cfg.Log.Children())
			f.completedRec = e.SeedRecord
		}
		return nil
	})
	f.cfg = cfg

	f.stage = "starting the decoy server"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the decoy: %v", err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	defer func() {
		_ = ln.Close()
		wg.Wait()
		f.decoyAccepted = int(accepted.Load())
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("the decoy's address: %v", err)
	}
	f.decoyListening = true
	f.ambientHome = os.Getenv("HOME")
	for _, name := range seedDecoyPortNames {
		t.Setenv(name, port)
	}
	seedSetOverrides(t)
	t.Setenv(seedCanary, "1")
	f.ambient = append(slices.Clone(seedOverrideNames), seedCanary)
	f.ambient = append(f.ambient, seedDecoyPortNames...)

	f.stage = "seeding"
	f.oracleBefore = treeSums(t, o.data)
	f.backupBefore = treeTimes(t, o.backup)
	rec, err := Seed(ctx, cfg)
	f.rec, f.seedErr = rec, err
	f.children = cfg.Log.Children()
	if err != nil {
		f.runErr = errors.New("not run: the seeding did not complete")
		return
	}

	f.stage = "replaying"
	runWork := filepath.Join(root, "run", "work")
	if err := os.MkdirAll(filepath.Dir(runWork), 0o755); err != nil {
		t.Fatalf("creating the run's directory: %v", err)
	}
	copyProject(t, cfg.WorkDir, runWork)
	seedForRun := rec
	rcfg := fr.cfg
	rcfg.WorkDir = runWork
	rcfg.WorkDataDir = replaytest.DataDir(t, runWork)
	rcfg.OutDir = filepath.Join(root, "run", "out")
	rcfg.Seed = &seedForRun
	rcfg.Observer = ObserverFunc(func(_ context.Context, ev Event) error {
		if r, ok := ev.(StepIssueResult); ok {
			f.steps = append(f.steps, r)
		}
		return nil
	})
	f.runCfg = rcfg
	f.run, f.runErr = Run(ctx, rcfg)

	f.stage = "reading what the replay left"
	f.results = readCommitReplayResults(t, rcfg.OutDir)
	f.mismatches = readMismatches(t, rcfg.OutDir)
	if data, err := os.ReadFile(filepath.Join(rcfg.OutDir, "summary.json")); err != nil { // #nosec G304 -- the flow's own output
		f.summaryErr = err
	} else {
		f.summaryRaw = data
		f.summaryErr = unmarshalStrict(data, &f.summary)
	}
	if w, err := ReadWalk(ctx, o.data); err == nil {
		f.walkSteps = w.Steps(0)
	}
	f.workIssues = seedCount(t, rcfg.WorkDataDir, "issues")
	f.legacyRows = seedQueryInt(t, rcfg.WorkDataDir, "SELECT COUNT(*) FROM issues WHERE participation_generation IS NULL")
	f.remotesAfter = seedRemotes(t, rcfg.WorkDataDir)
	f.backupsAfter = seedBackups(t, rcfg.WorkDataDir)
	f.oracleAfter = treeSums(t, o.data)
	f.backupAfter = treeTimes(t, o.backup)
}

// ---- B8.MigrateVerbIsFirstBdChild -------------------------------------------------

// B8.MigrateVerbIsFirstBdChild (R6): the first bd command on the copy is the
// migrate verb, and everything before it is the dolt CLI. Any other bd command on
// a copy at an older schema is itself the migration, so a bd child ahead of the
// verb would migrate the copy somewhere the recipe does not measure.
func TestB8MigrateVerbIsFirstBdChild(t *testing.T) {
	f := requireSeedFlow(t, defaultSeedVariant)
	first := -1
	for i, c := range f.children {
		if c.Tool == doltcli.ToolBd {
			first = i
			break
		}
	}
	if first < 0 {
		t.Fatalf("the seeding started no bd child among %d children", len(f.children))
	}
	if got, want := f.children[first].Args, []string{"migrate", "schema", "--json"}; !slices.Equal(got, want) {
		t.Errorf("the first bd child ran %q, want %q", got, want)
	}
	if f.migrating.children != first {
		t.Errorf("SeedMigrating was heard after %d children, and the migrate verb is child %d: it must come immediately before the verb", f.migrating.children, first)
	}
	readSchema := false
	for _, c := range f.children[:first] {
		if c.Tool != doltcli.ToolDolt {
			t.Errorf("a %s child ran before the migrate verb: %q", c.Tool, c.Args)
		}
		for _, a := range c.Args {
			if strings.Contains(a, "schema_migrations") {
				readSchema = true
			}
		}
	}
	if !readSchema {
		t.Errorf("no dolt child before the verb read schema_migrations: the base's schema is read from outside, before bd opens the copy")
	}
	for _, c := range f.children {
		if slices.Contains(c.Args, "--force") {
			t.Errorf("a %s child ran with --force: %q", c.Tool, c.Args)
		}
	}
	if want := []string{"embeddeddolt", "metadata.json"}; !slices.Equal(f.migrating.entries, want) {
		t.Errorf("at SeedMigrating .beads holds %q, want %q: nothing but the store and the project file until the first bd command", f.migrating.entries, want)
	}
}

// ---- B8.IgnoredPlaneIsFreshCloneAfterSeed -----------------------------------------

// B8.IgnoredPlaneIsFreshCloneAfterSeed (R2b, H20): when the copy is prepared, the
// clone-local plane is the state of a fresh clone. A clone of a store carries the
// committed tables only, so the rows an oracle accumulated in its local plane have
// no place in a seed. The migration track's own cursor is left as it is, the
// working set has nothing to commit, and the history is still the base.
func TestB8IgnoredPlaneIsFreshCloneAfterSeed(t *testing.T) {
	f := requireSeedFlow(t, defaultSeedVariant)
	o := f.o
	s := f.migrating.store

	var want []string
	for _, name := range ignoredTables(t, o.data) {
		if seedClears(name) {
			want = append(want, name)
		}
	}
	got := slices.Sorted(maps.Keys(s.Cleared))
	if !slices.Equal(got, want) {
		t.Fatalf("the seeded copy's clone-local tables are %q, the oracle's are %q", got, want)
	}
	for name, n := range s.Cleared {
		if n != 0 {
			t.Errorf("%s holds %d rows in the seeded copy, want none", name, n)
		}
	}
	if !slices.Equal(s.Seq, []string{"0:0"}) {
		t.Errorf("the event counter is %q, want exactly the one row 0:0", s.Seq)
	}
	if want := seedCount(t, o.data, "ignored_schema_migrations"); s.CursorRows != want {
		t.Errorf("the ignored track's cursor has %d rows, the oracle's has %d: the cursor is not the recipe's to change", s.CursorRows, want)
	}
	if want := seedQueryInt(t, o.data, "SELECT MAX(version) FROM ignored_schema_migrations"); s.IgnoredVersion != want {
		t.Errorf("the ignored track is at %d, the oracle's is at %d", s.IgnoredVersion, want)
	}
	if !s.Clean {
		t.Errorf("the seeded copy's working set has changes to commit")
	}
	if s.Head != o.base {
		t.Errorf("the seeded copy's head is %s, want the base %s", s.Head, o.base)
	}
}

// B8.IgnoredPlaneIsFreshCloneAfterSeed, control: the same oracle, copied and reset
// to the base, still carries its local plane. That is what makes the clearing a
// step the recipe has to take, and not something a copy and a reset do alone. It
// passes without the seeding, because it tests the fixture and not the seeding.
func TestB8OracleCopyKeepsItsLocalPlane(t *testing.T) {
	o := sharedSeedOracle(t, defaultSeedVariant)
	replaytest.Isolate(t)
	dst := filepath.Join(t.TempDir(), "copy")
	copyProject(t, o.project, dst)
	data := replaytest.DataDir(t, dst)
	replaytest.RunDolt(t, data, "reset", "--hard", o.base)

	s := readSeedStoreFacts(t, data)
	for _, name := range seedClearedNames {
		if n, ok := s.Cleared[name]; !ok || n == 0 {
			t.Errorf("%s holds %d rows in a copy that was only reset: the fixture must populate it for the clearing to mean anything", name, n)
		}
	}
	wisps := 0
	for name, n := range s.Cleared {
		if strings.HasPrefix(name, "wisp_") {
			wisps += n
		}
	}
	if wisps == 0 {
		t.Errorf("no wisp_ table holds a row in a copy that was only reset")
	}
	if slices.Equal(s.Seq, []string{"0:0"}) {
		t.Errorf("the event counter is at its start in a copy that was only reset: the fixture must have moved it")
	}
}

// ---- B8.SeedEventsEmitted ---------------------------------------------------------

// B8.SeedEventsEmitted: a seeding tells its observer when the copy is prepared and
// when it is done, once each and in that order, and the second carries the record
// the seeding returned. An observer that returns an error ends the seeding there.
func TestB8SeedEventsEmitted(t *testing.T) {
	f := requireSeedFlow(t, defaultSeedVariant)
	if want := []string{"migrating", "completed"}; !slices.Equal(f.events, want) {
		t.Fatalf("the observer heard %q, want %q", f.events, want)
	}
	if f.migrating.children >= len(f.children) {
		t.Errorf("SeedMigrating was heard after %d of %d children: the migrate verb and the checks come after it", f.migrating.children, len(f.children))
	}
	if f.completedChildren != len(f.children) {
		t.Errorf("SeedCompleted was heard after %d of %d children: it comes after the final assertions", f.completedChildren, len(f.children))
	}
	if f.completedRec != f.rec {
		t.Errorf("SeedCompleted carried %+v, the seeding returned %+v", f.completedRec, f.rec)
	}

	cfg := seedConfigAt(t, f.o, t.TempDir())
	cfg.Log = &doltcli.ChildLog{}
	if err := seedUntilMigration(t, cfg, nil); !errors.Is(err, errStopSeed) {
		t.Fatalf("an observer's error at SeedMigrating came back as %v, want it to end the seeding", err)
	}
	for _, c := range cfg.Log.Children() {
		if c.Tool == doltcli.ToolBd {
			t.Errorf("a bd child ran after the observer ended the seeding: %q", c.Args)
		}
	}
}

// ---- B8.SeedChildEnvIsolated ------------------------------------------------------

// seedChildEnvKeys is the whole environment of a seeding child (H19).
var seedChildEnvKeys = []string{
	"HOME", "XDG_CONFIG_HOME", "DOLT_ROOT_PATH", "TMPDIR", "LC_ALL", "PATH",
	"GIT_CONFIG_NOSYSTEM", "GIT_CEILING_DIRECTORIES", "BD_DISABLE_METRICS", "DO_NOT_TRACK",
	"BD_DISABLE_EVENT_FLUSH", "DOLT_DISABLE_EVENT_FLUSH", "BD_BACKUP_ENABLED",
}

// B8.SeedChildEnvIsolated (H19): every dolt and bd child of a seeding ran with an
// environment built from an allow-list, with its own home and dolt root inside the
// directory the seeding owns, and without dolt on its PATH. The evidence is what
// the stand-ins saw the processes receive, not what the harness says it handed
// them; and the children of the replay that follows carried none of the variables
// that would point them at a shared server or switch a check off.
func TestB8SeedChildEnvIsolated(t *testing.T) {
	f := requireSeededRun(t, defaultSeedVariant)
	for _, name := range append(slices.Clone(seedOverrideNames), append([]string{seedCanary}, seedDecoyPortNames...)...) {
		if !seedHas(f.ambient, name) {
			t.Fatalf("the flow did not run with %s set, so its children prove nothing", name)
		}
	}
	var seeding, replaying []seedDumpBlock
	for _, b := range parseSeedDump(t, f.dump) {
		switch b.label {
		case "dolt", "bd":
			seeding = append(seeding, b)
		case "run-bd":
			replaying = append(replaying, b)
		default:
			t.Errorf("a child with the unknown label %q ran", b.label)
		}
	}
	if len(seeding) != len(f.children) {
		t.Fatalf("the stand-ins saw %d seeding children and the Runner logged %d: a child ran outside the Runner", len(seeding), len(f.children))
	}
	allowed := slices.Sorted(slices.Values(seedChildEnvKeys))
	for i, b := range seeding {
		logged := f.children[i]
		if string(logged.Tool) != b.label {
			t.Errorf("child %d was logged as %s and ran as %s", i, logged.Tool, b.label)
		}
		env := seedWithoutShellNames(b.env)
		if got := slices.Sorted(maps.Keys(env)); !slices.Equal(got, allowed) {
			t.Errorf("child %d (%s %q) received the variables %q, want exactly %q", i, b.label, logged.Args, got, allowed)
		}
		if !maps.Equal(env, seedEnvMap(logged.Env)) {
			t.Errorf("child %d (%s) received an environment other than the one the Runner logged", i, b.label)
		}
		for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "DOLT_ROOT_PATH", "TMPDIR"} {
			if !seedUnder(env[k], f.cfg.EnvRoot) {
				t.Errorf("child %d has %s=%q, want it inside the seeding's own directory", i, k, env[k])
			}
		}
		if env["HOME"] == f.ambientHome {
			t.Errorf("child %d ran with the caller's HOME", i)
		}
		if env["BD_BACKUP_ENABLED"] != "0" {
			t.Errorf("child %d has BD_BACKUP_ENABLED=%q, want 0", i, env["BD_BACKUP_ENABLED"])
		}
		for _, dir := range filepath.SplitList(env["PATH"]) {
			if _, err := os.Stat(filepath.Join(dir, "dolt")); err == nil {
				t.Errorf("child %d has dolt on its PATH, in %s", i, dir)
			}
		}
	}
	if len(replaying) < 3 {
		t.Fatalf("the stand-in saw %d bd children of the replay, want at least the switch and the three writes", len(replaying))
	}
	banned := append(slices.Clone(seedOverrideNames), seedDecoyPortNames...)
	banned = append(banned, seedCanary)
	for i, b := range replaying {
		for _, name := range banned {
			if _, ok := b.env[name]; ok && name != seedCanary {
				t.Errorf("the replay's bd child %d received %s", i, name)
			}
		}
	}
}

// ---- B8.IdentityWrittenNotOmitted --------------------------------------------------

// B8.IdentityWrittenNotOmitted (R4, R5): the project file the recipe writes carries
// the identity the copy's store holds. A project file without it would make the
// product treat the copy as a workspace that has no identity, and the first write
// would be refused or, worse, would mint one.
func TestB8IdentityWrittenNotOmitted(t *testing.T) {
	f := requireSeedFlow(t, defaultSeedVariant)
	var meta map[string]any
	if err := json.Unmarshal(f.migrating.metadata, &meta); err != nil {
		t.Fatalf("the seeded project file is not a JSON object: %v\n%s", err, f.migrating.metadata)
	}
	if got, want := meta["project_id"], seedProjectID(t, f.o.data); got != want {
		t.Errorf("project_id = %v, want the store's %q", got, want)
	}
	if got, want := meta["dolt_database"], filepath.Base(f.o.data); got != want {
		t.Errorf("dolt_database = %v, want %q", got, want)
	}
	if len(meta) != 5 {
		t.Errorf("the project file has %d keys, want 5: %s", len(meta), f.migrating.metadata)
	}
	if len(f.notes) != 0 {
		t.Errorf("the seeding noted %q for a store that has an identity", f.notes)
	}
}

// B8.IdentityNoneRecorded (R4): a store with no identity gets a project file with
// none, and the seeding says so. It does not invent one, because the product mints
// the identity at first open, and a made-up value would be a second identity.
func TestB8IdentityNoneRecorded(t *testing.T) {
	o := sharedSeedOracle(t, noIdentityVariant)
	cfg := newSeedConfig(t, o)
	var notes []string
	cfg.Note = func(s string) { notes = append(notes, s) }
	var meta map[string]any
	err := seedUntilMigration(t, cfg, func() {
		data, err := os.ReadFile(filepath.Join(cfg.WorkDir, ".beads", "metadata.json")) // #nosec G304 -- the test's own work directory
		if err != nil {
			t.Fatalf("reading the seeded project file: %v", err)
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("the seeded project file is not a JSON object: %v\n%s", err, data)
		}
	})
	if !errors.Is(err, errStopSeed) {
		t.Fatalf("seeding up to the migration: %v", err)
	}
	if _, ok := meta["project_id"]; ok {
		t.Errorf("the project file carries project_id %v for a store that has none", meta["project_id"])
	}
	if len(meta) != 4 {
		t.Errorf("the project file has %d keys, want the 4 of a fresh init without an identity: %v", len(meta), meta)
	}
	if want := []string{"identity: db-has-none"}; !slices.Equal(notes, want) {
		t.Errorf("the seeding noted %q, want %q", notes, want)
	}
}
