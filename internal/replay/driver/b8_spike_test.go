package driver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/replaytest"
	"github.com/steveyegge/beads/internal/storage/schema"
)

// seedCloneLocalTables is every table the fixture's older bd keeps outside version
// control. The fixture writes each of them, so that a test of the clearing cannot
// pass on a plane that was empty to begin with. No table is exempt: each of the
// thirteen has rows at the fixture's head, the migration cursor included.
var seedCloneLocalTables = []string{
	"bd_events_journal", "bd_events_seq", "events", "ignored_schema_migrations",
	"leases", "local_metadata", "repo_mtimes", "wisp_child_counters", "wisp_comments",
	"wisp_dependencies", "wisp_events", "wisp_labels", "wisps",
}

// B8.FixtureIsGateEligible: the synthetic oracle is one the seeding's guards can
// be proved against. It is behind the integration's schema, at or above the floor,
// holds three issues at its base and a tail after it, carries an identity, one
// remote and one backup, and writes every clone-local table. Pending migrations
// are counted from the store and the schema package, never by opening it with bd,
// because a bd open is a migration.
func TestB8FixtureIsGateEligible(t *testing.T) {
	o := sharedSeedOracle(t, defaultSeedVariant)
	ctx := context.Background()

	version := seedQueryInt(t, o.data, "SELECT MAX(version) FROM schema_migrations")
	if version <= 0 {
		t.Errorf("the oracle's schema version is %d, want above 0", version)
	}
	if pending := schema.LatestVersion() - version; pending <= 0 {
		t.Errorf("the oracle has %d pending main migrations (at %d, latest %d), want above 0", pending, version, schema.LatestVersion())
	}
	if version < DefaultSchemaFloor {
		t.Errorf("the oracle's schema version %d is below the floor %d", version, DefaultSchemaFloor)
	}
	ignored := seedQueryInt(t, o.data, "SELECT MAX(version) FROM ignored_schema_migrations")
	if pending := schema.LatestIgnoredVersion() - ignored; pending <= 0 {
		t.Errorf("the oracle has %d pending ignored migrations (at %d, latest %d), want above 0", pending, ignored, schema.LatestIgnoredVersion())
	}

	if remotes := seedRemotes(t, o.data); len(remotes) != 1 || !strings.Contains(remotes[0], "file://") {
		t.Errorf("the oracle's remotes = %q, want exactly one file:// remote", remotes)
	}
	if backups := seedBackups(t, o.data); len(backups) != 1 || !strings.Contains(backups[0], "file://") {
		t.Errorf("the oracle's backups = %q, want exactly one file:// backup", backups)
	}
	if id := seedProjectID(t, o.data); id == "" {
		t.Errorf("the oracle has no _project_id")
	}

	w, err := ReadWalk(ctx, o.data)
	if err != nil {
		t.Fatalf("reading the oracle's walk: %v", err)
	}
	if got := w.Base().Hash; got != o.base {
		t.Errorf("the walk starts at %s, want the fixture's base %s: the empty schema commit must be the last bootstrap commit", got, o.base)
	}
	n, err := issuesAt(ctx, o.data, o.base)
	if err != nil {
		t.Fatalf("counting the issues at the base: %v", err)
	}
	if n != 3 {
		t.Errorf("the oracle holds %d issues at its base, want 3", n)
	}
	if steps := w.Steps(0); len(steps) != 3 {
		t.Errorf("the oracle has %d commits after its base, want the 3 of its tail", len(steps))
	}

	got := ignoredTables(t, o.data)
	if !slices.Equal(got, seedCloneLocalTables) {
		t.Errorf("the oracle's clone-local tables are\n  %v\nwant\n  %v", got, seedCloneLocalTables)
	}
	for _, name := range got {
		if rows := seedCount(t, o.data, name); rows == 0 {
			t.Errorf("clone-local table %s has no rows, so a test of the clearing would pass on it without clearing anything", name)
		}
	}
	if status := replaytest.RunDolt(t, o.data, "status"); !strings.Contains(status, "nothing to commit") {
		t.Errorf("the oracle's working set is not clean:\n%s", status)
	}
}

// B8.SeedRecipe: the whole recipe, R1 to R7, on the synthetic oracle. It is the
// spike: until the seeding exists this fails because Seed does not seed, and it
// goes green only if a copy of an older store can be made to accept a first write.
// Every expectation is read back from the store with the dolt CLI or the schema
// package, never from the seeding's own record.
func TestB8SeedRecipe(t *testing.T) {
	o := sharedSeedOracle(t, defaultSeedVariant)
	cfg := newSeedConfig(t, o)
	ctx := context.Background()

	oracleBefore := treeSums(t, o.data)
	oracleSchema := seedQueryInt(t, o.data, "SELECT MAX(version) FROM schema_migrations")
	oracleIgnored := seedQueryInt(t, o.data, "SELECT MAX(version) FROM ignored_schema_migrations")
	wantCleared := 0
	for _, name := range ignoredTables(t, o.data) {
		if seedClears(name) {
			wantCleared += seedCount(t, o.data, name)
		}
	}
	logAtBase := seedQueryInt(t, o.data, "SELECT COUNT(*) FROM dolt_log('"+o.base+"')")
	projectID := seedProjectID(t, o.data)

	var events []string
	var metadataAtMigrating []byte
	cfg.Observer = ObserverFunc(func(_ context.Context, ev Event) error {
		switch ev.(type) {
		case SeedMigrating:
			events = append(events, "migrating")
			data, err := os.ReadFile(filepath.Join(cfg.WorkDir, ".beads", "metadata.json")) // #nosec G304 -- the test's own work directory
			if err != nil {
				t.Errorf("reading metadata.json when the migration was about to start: %v", err)
			}
			metadataAtMigrating = data
		case SeedCompleted:
			events = append(events, "completed")
		}
		return nil
	})

	rec, err := Seed(ctx, cfg)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	work := replaytest.DataDir(t, cfg.WorkDir)
	if got, want := filepath.Base(work), filepath.Base(o.data); got != want {
		t.Errorf("the seed's database directory is %q, want the oracle's %q", got, want)
	}

	// R7 (ii): both tracks reached the integration's latest, measured with the CLI.
	if got := seedQueryInt(t, work, "SELECT MAX(version) FROM schema_migrations"); got != schema.LatestVersion() {
		t.Errorf("the seed's schema version is %d, want the integration's %d", got, schema.LatestVersion())
	}
	if got := seedQueryInt(t, work, "SELECT MAX(version) FROM ignored_schema_migrations"); got != schema.LatestIgnoredVersion() {
		t.Errorf("the seed's ignored schema version is %d, want the integration's %d", got, schema.LatestIgnoredVersion())
	}
	// R7 (iii): the oracle's three issues, by id, and nothing else.
	if got := seedCount(t, work, "issues"); got != len(o.ids) {
		t.Errorf("the seed holds %d issues, want the oracle's %d at its base", got, len(o.ids))
	}
	for _, id := range o.ids {
		if got := seedQueryInt(t, work, "SELECT COUNT(*) FROM issues WHERE id = '"+id+"'"); got != 1 {
			t.Errorf("the seed does not hold issue %s exactly once (%d rows)", id, got)
		}
	}
	// The legacy-row finding: every seeded row predates versioned history, so the
	// write fence of schema 72 records nothing for it. The count is the number of
	// seeded issues, not zero.
	if got := seedCount(t, work, "issues WHERE participation_generation IS NULL"); got != len(o.ids) {
		t.Errorf("%d seeded rows have no participation generation, want all %d", got, len(o.ids))
	}
	// R7 (v): versioned history is still off.
	if got := seedCount(t, work, "config WHERE `key` = 'versioned-history.enabled' AND value IN ('true', '1')"); got != 0 {
		t.Errorf("versioned history is on in the seed")
	}

	// R7 (i) and R3: nothing in the seed points anywhere.
	if remotes := seedRemotes(t, work); len(remotes) != 0 {
		t.Errorf("the seed still lists remotes %q", remotes)
	}
	if backups := seedBackups(t, work); len(backups) != 0 {
		t.Errorf("the seed still lists backups %q", backups)
	}
	if rec.RemotesStripped != 1 || rec.BackupsStripped != 1 {
		t.Errorf("stripped %d remotes and %d backups, want 1 and 1", rec.RemotesStripped, rec.BackupsStripped)
	}

	// The record, against the store.
	if rec.SchemaBefore != oracleSchema || rec.SchemaAfter != schema.LatestVersion() {
		t.Errorf("schema %d to %d, want %d to %d", rec.SchemaBefore, rec.SchemaAfter, oracleSchema, schema.LatestVersion())
	}
	if rec.IgnoredSchemaBefore != oracleIgnored || rec.IgnoredSchemaAfter != schema.LatestIgnoredVersion() {
		t.Errorf("ignored schema %d to %d, want %d to %d", rec.IgnoredSchemaBefore, rec.IgnoredSchemaAfter, oracleIgnored, schema.LatestIgnoredVersion())
	}
	if wantCleared == 0 {
		t.Fatalf("the oracle's clone-local plane is empty, so the clearing cannot be measured")
	}
	if rec.IgnoredRowsCleared != wantCleared {
		t.Errorf("cleared %d clone-local rows, want %d", rec.IgnoredRowsCleared, wantCleared)
	}
	head := seedQueryText(t, work, "SELECT hashof('HEAD')")
	if rec.SeedHead != head {
		t.Errorf("seed_head = %q, want the seed's head %q", rec.SeedHead, head)
	}
	wantCommits := seedQueryInt(t, work, "SELECT COUNT(*) FROM dolt_log") - logAtBase
	if wantCommits <= 0 || rec.MigrationCommits != wantCommits {
		t.Errorf("migration_commits = %d, want %d (log of the seed minus the log at the base, which must be above 0)", rec.MigrationCommits, wantCommits)
	}
	if rec.MigrationSeconds <= 0 {
		t.Errorf("migration_seconds = %v, want a measured wall time", rec.MigrationSeconds)
	}
	if !strings.HasPrefix(rec.DoltCLIVersion, "dolt version ") {
		t.Errorf("dolt_cli_version = %q, want dolt's own version line", rec.DoltCLIVersion)
	}
	if rec.LinkedEngine != cfg.LinkedEngine || rec.LinkedEngine == "" {
		t.Errorf("linked_engine = %q, want %q", rec.LinkedEngine, cfg.LinkedEngine)
	}
	// A feasibility result is only as good as the tools it ran on, so the log names them.
	t.Logf("dolt_cli_version = %s", rec.DoltCLIVersion)
	t.Logf("linked_engine = %s", rec.LinkedEngine)

	// R4 and R5: the identity travels, in the fresh-init shape.
	var meta map[string]string
	if err := json.Unmarshal(metadataAtMigrating, &meta); err != nil {
		t.Fatalf("metadata.json when the migration was about to start is not a flat JSON object: %v\n%s", err, metadataAtMigrating)
	}
	wantMeta := map[string]string{
		"database": "dolt", "backend": "dolt", "dolt_mode": "embedded",
		"dolt_database": filepath.Base(o.data), "project_id": projectID,
	}
	if !equalStringMaps(meta, wantMeta) {
		t.Errorf("metadata.json was\n  %v\nwant exactly\n  %v", meta, wantMeta)
	}
	if info, err := os.Stat(filepath.Join(cfg.WorkDir, ".beads")); err != nil {
		t.Errorf("stat .beads: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf(".beads has mode %o, want 700", perm)
	}

	// The events: the migration is announced once, and the seed is complete once,
	// in that order.
	if !slices.Equal(events, []string{"migrating", "completed"}) {
		t.Errorf("seed events = %v, want one migrating then one completed", events)
	}

	// The first write is accepted: the identity check passes, the remote-migrate
	// gate has nothing to refuse, and the cleared plane does not trip the store.
	replaytest.RunBd(t, cfg.BdBin, cfg.WorkDir, "create", "first write", "-p", "2", "-t", "task", "--json")
	if got := seedCount(t, work, "issues"); got != len(o.ids)+1 {
		t.Errorf("after the first write the seed holds %d issues, want %d", got, len(o.ids)+1)
	}

	// H16: the oracle was copied from and never written.
	sameTree(t, "the oracle", oracleBefore, treeSums(t, o.data))
	if len(seedRemotes(t, o.data)) != 1 || len(seedBackups(t, o.data)) != 1 {
		t.Errorf("the oracle lost its remote or its backup: the strip must act on the copy only")
	}
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
