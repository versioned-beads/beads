package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/replaytest"
	"github.com/steveyegge/beads/internal/replay/translate"
	"github.com/steveyegge/beads/internal/storage/schema"
)

// The controls: each one makes a precondition of the recipe fail on purpose and
// shows that the seeding refuses, says which precondition it was, and leaves the
// store as it found it. A control that could pass with the recipe missing proves
// nothing, so each runs with the three override variables set in its own
// environment: if one reached a child, the product would let the write through.

// seedOverrideNames are the variables that tell the product to skip a check the
// seeding exists to satisfy: the remote-migrate gate, the workspace identity
// check and the schema-skew refusal. The tests set them to prove none of them
// reaches a child, and this is one of the few files that names them.
var seedOverrideNames = []string{
	"BD_ALLOW_REMOTE_MIGRATE", "BEADS_SKIP_IDENTITY_CHECK", "BD_IGNORE_SCHEMA_SKEW",
}

// seedCanary is a variable nothing reads. If it shows up in a child's environment,
// the child was handed the caller's environment and not the allow-list.
const seedCanary = "B8_SEED_CANARY"

// seedSetOverrides puts the three override variables in the calling test's
// environment. Call it after replaytest.Isolate, which does not clear them.
func seedSetOverrides(t testing.TB) {
	t.Helper()
	for _, name := range seedOverrideNames {
		t.Setenv(name, "1")
	}
}

// seedEnvHas reports whether an environment in KEY=value form sets name.
func seedEnvHas(env []string, name string) bool {
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == name {
			return true
		}
	}
	return false
}

// seedRewriteProjectID changes the identity in the copy's metadata.json, so that
// the copy's store and its project file name different workspaces.
func seedRewriteProjectID(t testing.TB, workDir, id string) {
	t.Helper()
	path := filepath.Join(workDir, ".beads", "metadata.json")
	data, err := os.ReadFile(path) // #nosec G304 -- the test's own work directory
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("%s is not a JSON object: %v", path, err)
	}
	if _, ok := meta["project_id"]; !ok {
		t.Fatalf("%s has no project_id to rewrite", path)
	}
	meta["project_id"] = id
	out, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// seedFirstWrite is the first write of a replay on a seeded copy, issued the way the
// step loop issues it: through the run's own environment, to the bd the run is
// configured with. That bd is a stand-in that logs the call and runs the real one,
// and the run's seed record is set, as it is for a run that follows a seeding. It
// returns the calls the stand-in saw and what the write returned.
func seedFirstWrite(t *testing.T, cfg SeedConfig) (calls []string, err error) {
	t.Helper()
	bin, log := bdStandIn(t, cfg.BdBin, "")
	env := realEnv{cfg: RunConfig{
		WorkDir: cfg.WorkDir,
		Tools:   Tools{IntegrationBin: bin},
		Seed:    &SeedRecord{},
	}}
	err = env.Execute(context.Background(), translate.Action{
		Kind:  translate.KindCreate,
		Issue: "control-1",
		Argv:  []string{"create", "control write", "-p", "2", "-t", "task", "--json"},
	})
	return standInCalls(t, log), err
}

// requireSeedRefusal fails the test unless err is a refusal of the class, caused by
// a bd that exited 1 with the text. It checks the three things a reader of a failed
// run relies on: the class, whether it is a fault of the harness, and that the
// refusal is not also an *translate.ExecError, which the step loop records as a
// rejected write and goes on from.
func requireSeedRefusal(t *testing.T, err error, class, text string) {
	t.Helper()
	var sr *SeedRefused
	if !errors.As(err, &sr) {
		t.Fatalf("the first write returned %v, want a *SeedRefused of class %q", err, class)
	}
	if sr.Class != class {
		t.Errorf("refusal class = %q, want %q (%v)", sr.Class, class, sr)
	}
	if !sr.HarnessFault() {
		t.Errorf("a %q refusal is not reported as a harness fault", class)
	}
	var ee *translate.ExecError
	if errors.As(err, &ee) {
		t.Errorf("the refusal unwraps to an *translate.ExecError, so the step loop would record a rejected write and carry on")
	}
	switch {
	case sr.Exit == nil:
		t.Errorf("the refusal carries no report of the command that refused")
	default:
		if sr.Exit.ExitCode != 1 {
			t.Errorf("the refusing command exited %d, want 1", sr.Exit.ExitCode)
		}
		if !strings.Contains(sr.Exit.Output, text) {
			t.Errorf("the refusing command's output does not contain %q:\n%s", text, sr.Exit.Output)
		}
	}
}

// requireSeedControl seeds up to the migration, lets the tamper spoil the copy, and
// issues the first write of a replay on it. The write must be refused by the
// product, be reported as the class, and change nothing: the first bd command of the
// run is the refused write itself, and the store is as the tamper left it.
func requireSeedControl(t *testing.T, tamper func(t testing.TB, cfg SeedConfig), class, text string) {
	t.Helper()
	o := sharedSeedOracle(t, defaultSeedVariant)
	cfg := newSeedConfig(t, o)
	seedSetOverrides(t)
	cfg.Log = &doltcli.ChildLog{}
	if err := seedUntilMigration(t, cfg, func() { tamper(t, cfg) }); !errors.Is(err, errStopSeed) {
		t.Fatalf("seeding up to the migration: %v", err)
	}
	data := replaytest.DataDir(t, cfg.WorkDir)
	before := readSeedStoreFacts(t, data)

	calls, err := seedFirstWrite(t, cfg)
	requireSeedRefusal(t, err, class, text)

	if len(calls) != 1 || !strings.HasPrefix(calls[0], "create ") {
		t.Errorf("the bd stand-in saw %q, want exactly the one refused create", calls)
	}
	for _, c := range cfg.Log.Children() {
		if c.Tool == doltcli.ToolBd {
			t.Errorf("the seeding ran bd %q before the first write: the first bd command must be the refused write", c.Args)
		}
	}
	if after := readSeedStoreFacts(t, data); !seedSameCommittedState(before, after) {
		t.Errorf("the refused write changed the store:\n  before %+v\n  after  %+v", before, after)
	}
}

// seedSameCommittedState compares what a refused or repeated command must leave
// alone: the schema, the migration cursor, the history, the issues and the working
// set. The clone-local tables are left out, because a bd command may write to them.
func seedSameCommittedState(a, b seedStoreFacts) bool {
	return a.Version == b.Version && a.IgnoredVersion == b.IgnoredVersion &&
		a.CursorRows == b.CursorRows && a.LogLen == b.LogLen && a.Issues == b.Issues &&
		a.Head == b.Head && a.Clean == b.Clean
}

// B8.SeedRefusesWithoutIdentity: a seeded copy whose project file and store name
// different workspaces is refused by the product's identity check, on the first
// write, before anything is written; the seeding reports it as its own refusal.
func TestB8SeedRefusesWithoutIdentity(t *testing.T) {
	requireSeedControl(t, func(t testing.TB, cfg SeedConfig) {
		seedRewriteProjectID(t, cfg.WorkDir, "another-workspace-identity")
	}, RefusedIdentity, "workspace identity mismatch detected")
}

// B8.SeedRefusesWithRemote: a seeded copy that still has a remote is refused by the
// product's remote-migrate gate, on the first write, before anything is migrated.
func TestB8SeedRefusesWithRemote(t *testing.T) {
	requireSeedControl(t, func(t testing.TB, cfg SeedConfig) {
		data := replaytest.DataDir(t, cfg.WorkDir)
		replaytest.RunDolt(t, data, "remote", "add", "origin", "file://"+t.TempDir())
	}, RefusedRemoteMigrateGate, "remote-backed database")
}

// seedLogMessages reads the messages of the newest n commits of the store at dir.
func seedLogMessages(t testing.TB, dir string, n int) []string {
	t.Helper()
	_, rows, err := doltcli.Query(context.Background(), dir, "SELECT message FROM dolt_log ORDER BY commit_order DESC LIMIT "+strconv.Itoa(n))
	if err != nil {
		t.Fatalf("reading the newest %d commit messages of %s: %v", n, dir, err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r[0].Text
	}
	return out
}

// B8.FirstOpenMigrates: the first bd command on the prepared copy is what migrates
// it. The main track goes from the base's version to the integration's latest, the
// ignored track to its latest, each pending migration is one commit, and the verb
// has nothing left to do afterwards. A bd that did not migrate on first open would
// leave the recipe's migrate verb with work the recipe does not expect.
func TestB8FirstOpenMigrates(t *testing.T) {
	o := sharedSeedOracle(t, defaultSeedVariant)
	cfg := newSeedConfig(t, o)
	cfg.Log = &doltcli.ChildLog{}
	if err := seedUntilMigration(t, cfg, nil); !errors.Is(err, errStopSeed) {
		t.Fatalf("seeding up to the migration: %v", err)
	}
	data := replaytest.DataDir(t, cfg.WorkDir)
	ctx := context.Background()
	before := readSeedStoreFacts(t, data)
	pending := schema.LatestVersion() - before.Version
	if pending <= 0 {
		t.Fatalf("the prepared copy is at schema %d, the integration's latest is %d: nothing is pending", before.Version, schema.LatestVersion())
	}

	runner := cfg.childRunner()
	if _, err := runner.Bd(ctx, cfg.WorkDir, "count", "--json"); err != nil {
		t.Fatalf("the first bd command on the prepared copy: %v", err)
	}
	after := readSeedStoreFacts(t, data)
	if after.Version != schema.LatestVersion() {
		t.Errorf("the main track is at %d after the first open, want %d", after.Version, schema.LatestVersion())
	}
	if after.IgnoredVersion != schema.LatestIgnoredVersion() {
		t.Errorf("the ignored track is at %d after the first open, want %d", after.IgnoredVersion, schema.LatestIgnoredVersion())
	}
	n := after.LogLen - before.LogLen
	if n <= 0 {
		t.Fatalf("the first open added %d commits, want the migration's", n)
	}
	applied, seeds := 0, 0
	for _, m := range seedLogMessages(t, data, n) {
		if !strings.HasPrefix(m, "schema: ") {
			t.Errorf("a commit the first open made has the message %q, want one that starts with %q", m, "schema: ")
		}
		if strings.HasPrefix(m, "schema: apply migration ") {
			applied++
		}
		if m == "schema: seed dolt_ignore patterns" {
			seeds++
		}
	}
	if applied != pending {
		t.Errorf("%d migration commits for %d pending migrations", applied, pending)
	}
	if n != applied+seeds {
		t.Errorf("the first open made %d commits, %d of them migrations and %d seeding the ignore patterns: want no others", n, applied, seeds)
	}

	if _, err := runner.Bd(ctx, cfg.WorkDir, "migrate", "schema", "--json"); err != nil {
		t.Fatalf("the migrate verb on a migrated copy: %v", err)
	}
	if again := readSeedStoreFacts(t, data); !seedSameCommittedState(after, again) {
		t.Errorf("the migrate verb changed a copy the first open had already migrated:\n  before %+v\n  after  %+v", after, again)
	}
}

// B8.BaseBelowFloorRefused (R2a): a base below the schema floor is refused with the
// number it was at, before any bd command and without touching the oracle. The
// fixture's base is at one fixed version, so the floor is raised above it, which is
// what a base below the floor looks like to the seeding; at the base's own version
// the seeding goes on.
func TestB8BaseBelowFloorRefused(t *testing.T) {
	o := sharedSeedOracle(t, defaultSeedVariant)
	oracleSchema := seedQueryInt(t, o.data, "SELECT MAX(version) FROM schema_migrations")
	before := treeSums(t, o.data)

	cfg := newSeedConfig(t, o)
	cfg.SchemaFloor = oracleSchema + 1
	cfg.Log = &doltcli.ChildLog{}
	_, err := Seed(context.Background(), cfg)
	var sr *SeedRefused
	if !errors.As(err, &sr) || sr.Class != RefusedBaseSchema {
		t.Fatalf("Seed with the floor above the base returned %v, want a %q refusal", err, RefusedBaseSchema)
	}
	if !sr.HarnessFault() {
		t.Errorf("a base below the floor is not reported as a harness fault")
	}
	for _, want := range []string{strconv.Itoa(oracleSchema), strconv.Itoa(cfg.SchemaFloor)} {
		if !strings.Contains(sr.Detail, want) {
			t.Errorf("the refusal %q does not record %s", sr.Detail, want)
		}
	}
	for _, c := range cfg.Log.Children() {
		if c.Tool == doltcli.ToolBd {
			t.Errorf("a bd command ran on a base that was refused: %q", c.Args)
		}
	}
	sameTree(t, "the oracle", before, treeSums(t, o.data))

	cfg = seedConfigAt(t, o, t.TempDir())
	cfg.SchemaFloor = oracleSchema
	if err := seedUntilMigration(t, cfg, nil); !errors.Is(err, errStopSeed) {
		t.Fatalf("Seed with the floor at the base's own version: %v, want it to go on", err)
	}
}

// seedNoopRemoveStandIn writes a stand-in for dolt that does nothing for
// `<sub> remove`, so a target the recipe takes away stays, and runs the real dolt
// for everything else.
func seedNoopRemoveStandIn(t testing.TB, realDolt, sub string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dolt-stand-in")
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = " + shellQuote(sub+" remove") + " ]; then exit 0; fi\n" +
		"exec " + shellQuote(realDolt) + " \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { // #nosec G306 -- a test stand-in must be executable
		t.Fatalf("writing the dolt stand-in: %v", err)
	}
	return bin
}

// B8.SeedRefusesWhileTargetsRemain (R3): a remote or a backup that is still listed
// after the recipe took it away stops the seeding before any bd command. The
// check is made on the listing the dolt CLI gives, not on the command's exit.
func TestB8SeedRefusesWhileTargetsRemain(t *testing.T) {
	for _, tc := range []struct{ sub, class string }{
		{"remote", RefusedRemotes},
		{"backup", RefusedBackups},
	} {
		t.Run(tc.sub, func(t *testing.T) {
			o := sharedSeedOracle(t, defaultSeedVariant)
			cfg := newSeedConfig(t, o)
			cfg.DoltBin = seedNoopRemoveStandIn(t, cfg.DoltBin, tc.sub)
			cfg.Log = &doltcli.ChildLog{}
			_, err := Seed(context.Background(), cfg)
			var sr *SeedRefused
			if !errors.As(err, &sr) || sr.Class != tc.class {
				t.Fatalf("Seed with a %s that cannot be removed returned %v, want a %q refusal", tc.sub, err, tc.class)
			}
			if !sr.HarnessFault() {
				t.Errorf("a %q refusal is not reported as a harness fault", tc.class)
			}
			if sr.Detail == "" {
				t.Errorf("the refusal gives no detail")
			}
			for _, c := range cfg.Log.Children() {
				if c.Tool == doltcli.ToolBd {
					t.Errorf("a bd command ran on a copy that still had a %s: %q", tc.sub, c.Args)
				}
			}
		})
	}
}

// B8.OverrideEnvStripped (H14): none of the three override variables reaches a
// child. The unit half sets them and asks the two environment builders; the other
// half reads what the children of a whole seeding and replay actually received,
// recorded by the stand-ins that ran them, in a process that had all three set.
func TestB8OverrideEnvStripped(t *testing.T) {
	seedSetOverrides(t)
	t.Setenv(seedCanary, "1")
	sanitized := doltcli.SanitizedEnv(os.Environ())
	root := t.TempDir()
	child := doltcli.ChildEnv(filepath.Join(root, "env"), filepath.Join(root, "work"))
	for _, name := range seedOverrideNames {
		if seedEnvHas(sanitized, name) {
			t.Errorf("SanitizedEnv keeps %s", name)
		}
		if seedEnvHas(child, name) {
			t.Errorf("ChildEnv sets %s", name)
		}
	}
	if seedEnvHas(child, seedCanary) {
		t.Errorf("ChildEnv passes an ambient variable through: it must be built from an allow-list")
	}

	f := sharedSeededFlow(t, defaultSeedVariant)
	for _, name := range seedOverrideNames {
		if !seedHas(f.ambient, name) {
			t.Fatalf("the shared flow did not run with %s set, so its children prove nothing", name)
		}
	}
	blocks := parseSeedDump(t, f.dump)
	if len(blocks) == 0 {
		t.Fatalf("the stand-ins recorded no child")
	}
	for i, b := range blocks {
		for _, name := range seedOverrideNames {
			if _, ok := b.env[name]; ok {
				t.Errorf("child %d (%s) received %s", i, b.label, name)
			}
		}
	}
}

func seedHas(list []string, name string) bool {
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}

// B8.SeedRefusalEndsTheRun: the step loop records a write the product rejected and
// goes on with the next issue, and ends the run for anything else. A seeding
// refusal must be the second kind, or a copy that cannot take a write would be
// replayed against as a long list of rejected rows.
func TestB8SeedRefusalEndsTheRun(t *testing.T) {
	refusal := &SeedRefused{
		Class:  RefusedIdentity,
		Detail: "the project file and the store name different workspaces",
		Exit:   &translate.ExecError{ExitCode: 1, Output: "refused"},
	}
	env := &fakeEnv{
		plan: &translate.StepPlan{Actions: []translate.Action{
			action(translate.KindCreate, "x-1", "create", "x-1"),
			action(translate.KindCreate, "y-1", "create", "y-1"),
		}},
		oracle:    map[string]*oracle.View{"x-1": viewOf("x-1", txt("title", "T")), "y-1": viewOf("y-1", txt("title", "T"))},
		candidate: map[string]*oracle.View{"x-1": viewOf("x-1", txt("title", "T")), "y-1": viewOf("y-1", txt("title", "T"))},
		head:      "workhead",
		execErr:   func(translate.Action) error { return refusal },
	}
	q := quarantined{}
	_, err := replayStep(context.Background(), env, q, stepFromTo("from", "to"))
	var got *SeedRefused
	if !errors.As(err, &got) || got != refusal {
		t.Fatalf("replayStep returned %v, want the seeding's refusal to end the step", err)
	}
	if len(q) != 0 {
		t.Errorf("the refusal quarantined %v as a rejected write", q)
	}
	if n := len(env.callsWithPrefix("exec ")); n != 1 {
		t.Errorf("%d writes ran after the refusal, want the one that was refused", n)
	}
}
