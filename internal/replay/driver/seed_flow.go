package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/replay/doltcli"
)

// baseHash is what a dolt commit hash looks like: the base is handed to the dolt
// CLI as an argument, so it must not be able to pass for an option.
var baseHash = regexp.MustCompile(`^[0-9a-v]{32}$`)

// The statements the seeding reads the copy with. The one change it makes, to the
// copy's own clone-local plane, is written where it is made, in clearLocalPlane.
const (
	querySchemaVersion  = "SELECT MAX(version) FROM schema_migrations"
	queryIgnoredVersion = "SELECT MAX(version) FROM ignored_schema_migrations"
	queryCursorRows     = "SELECT COUNT(*) FROM ignored_schema_migrations"
	queryIssues         = "SELECT COUNT(*) FROM issues"
	queryLogLength      = "SELECT COUNT(*) FROM dolt_log"
	queryChanges        = "SELECT COUNT(*) FROM dolt_status"
	queryHead           = "SELECT hashof('HEAD')"
	queryProjectID      = "SELECT value FROM metadata WHERE `key` = '_project_id'"
	queryCounterRows    = "SELECT id, next_seq FROM " + counterTable + " ORDER BY id"
	queryHistoryOn      = "SELECT COUNT(*) FROM config WHERE `key` = 'versioned-history.enabled' AND value IN ('true', '1')"

	// queryIgnoredTables is the product's own test for a clone-local table: a
	// pattern of dolt_ignore whose ignored flag is set, matched with LIKE.
	queryIgnoredTables = "SELECT t.TABLE_NAME FROM INFORMATION_SCHEMA.TABLES t " +
		"WHERE t.TABLE_SCHEMA = DATABASE() AND t.TABLE_TYPE = 'BASE TABLE' " +
		"AND EXISTS (SELECT 1 FROM dolt_ignore di WHERE di.ignored = 1 AND t.TABLE_NAME LIKE di.pattern) " +
		"ORDER BY t.TABLE_NAME"
)

// seeding is one run of the recipe: the configuration, the one Runner every child
// goes through, and where the copy is.
type seeding struct {
	cfg    SeedConfig
	runner *doltcli.Runner
	// db is the name of the copy's database directory, which is the oracle's, and
	// dbDir is where it is: the directory the dolt CLI is run in.
	db    string
	dbDir string
	// issues is how many issues the copy held at the base.
	issues int
}

// Seed gives cfg.WorkDir the oracle's state at cfg.Base and migrates it to the
// integration's schema, and says what it did.
//
// The oracle's dolt directory is copied, never opened. On the copy the head is
// moved to the base, the schema is read and checked against the floor, and the
// clone-local plane is given the state a fresh clone has. Every remote and backup is removed, the project file is
// written with the copy's own identity, and only then does the first bd command run:
// the migration, which is the open of an older store that the product would do on
// any first command, made on its own so that it can be measured and refused. What it
// reached is checked, and the record of all of it is returned.
//
// A refusal is a *SeedRefused. Nothing the seeding did is undone on failure: the
// caller owns the directories and removes them.
func Seed(ctx context.Context, cfg SeedConfig) (SeedRecord, error) {
	if err := cfg.check(); err != nil {
		return SeedRecord{}, err
	}
	s := &seeding{cfg: cfg, runner: cfg.childRunner(), db: filepath.Base(cfg.OracleDataDir)}
	s.dbDir = filepath.Join(cfg.WorkDir, ".beads", "embeddeddolt", s.db)
	rec := SeedRecord{LinkedEngine: cfg.LinkedEngine}

	var err error
	if rec.DoltCLIVersion, err = s.prepare(ctx); err != nil {
		return rec, err
	}
	if err := copyTree(filepath.Join(cfg.OracleDataDir, ".dolt"), filepath.Join(s.dbDir, ".dolt")); err != nil {
		return rec, fmt.Errorf("copying the oracle: %w", err)
	}
	if err := s.moveToBase(ctx, &rec); err != nil {
		return rec, err
	}
	if err := s.clearLocalPlane(ctx, &rec); err != nil {
		return rec, err
	}
	if rec.RemotesStripped, err = s.strip(ctx, "remote", RefusedRemotes); err != nil {
		return rec, err
	}
	if rec.BackupsStripped, err = s.strip(ctx, "backup", RefusedBackups); err != nil {
		return rec, err
	}
	if err := s.writeProjectFile(ctx); err != nil {
		return rec, err
	}
	if err := s.migrate(ctx, &rec); err != nil {
		return rec, err
	}
	if err := s.verify(ctx, rec); err != nil {
		return rec, err
	}
	if cfg.Observer != nil {
		if err := cfg.Observer.Observe(ctx, SeedCompleted{SeedRecord: rec}); err != nil {
			return rec, fmt.Errorf("the observer stopped the run after the seeding: %w", err)
		}
	}
	return rec, nil
}

// check refuses a configuration the seeding cannot start from, before anything is
// written. The tools must be paths: a name would be looked up on whatever PATH the
// caller has, and the seeding is meant to run exactly the tools it was given.
func (c SeedConfig) check() error {
	for _, f := range []struct{ name, value string }{
		{"OracleDataDir", c.OracleDataDir},
		{"WorkDir", c.WorkDir},
		{"EnvRoot", c.EnvRoot},
		{"DoltBin", c.DoltBin},
		{"BdBin", c.BdBin},
		{"LinkedEngine", c.LinkedEngine},
	} {
		if f.value == "" {
			return fmt.Errorf("seeding: SeedConfig.%s is empty", f.name)
		}
	}
	for _, f := range []struct{ name, value string }{
		{"OracleDataDir", c.OracleDataDir},
		{"WorkDir", c.WorkDir},
		{"EnvRoot", c.EnvRoot},
		{"DoltBin", c.DoltBin},
		{"BdBin", c.BdBin},
	} {
		if !filepath.IsAbs(f.value) {
			return fmt.Errorf("seeding: SeedConfig.%s must be an absolute path", f.name)
		}
	}
	if !baseHash.MatchString(c.Base) {
		return fmt.Errorf("seeding: SeedConfig.Base is not a dolt commit hash")
	}
	if c.LatestSchema <= 0 || c.LatestIgnored <= 0 {
		return errors.New("seeding: SeedConfig names no latest schema version to reach")
	}
	if _, err := os.Stat(filepath.Join(c.OracleDataDir, ".dolt")); err != nil {
		return fmt.Errorf("seeding: the oracle's data directory is not a dolt database: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(c.WorkDir, ".beads")); err == nil {
		return errors.New("seeding: the work directory already has a .beads directory")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("seeding: %w", err)
	}
	return nil
}

// prepare makes the directories the children run in and the dolt identity they
// run under, and returns the version line of the dolt CLI they run. Nothing of the
// caller's home or dolt configuration is read or written: the identity is set in
// the seeding's own.
func (s *seeding) prepare(ctx context.Context) (string, error) {
	root := s.cfg.EnvRoot
	for _, dir := range []string{
		s.cfg.WorkDir,
		filepath.Join(root, "home", ".config"),
		filepath.Join(root, "dolt-root"),
		filepath.Join(root, "tmp"),
		filepath.Join(root, "bin"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("seeding: %w", err)
		}
	}
	// A bd child finds git on its PATH and nothing else. dolt is not there: the dolt
	// that bd talks to is the engine it links, not a CLI it came across.
	if git, err := exec.LookPath("git"); err == nil {
		if err := os.Symlink(git, filepath.Join(root, "bin", "git")); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("seeding: %w", err)
		}
	}
	for _, kv := range [][2]string{
		{"user.name", "replay-seed"},
		{"user.email", "replay-seed@example.invalid"},
		{"versioncheck.disabled", "true"},
	} {
		if _, err := s.runner.Dolt(ctx, root, "config", "--global", "--set", kv[0], kv[1]); err != nil {
			return "", fmt.Errorf("seeding: setting the dolt configuration %s: %w", kv[0], err)
		}
	}
	res, err := s.runner.Dolt(ctx, root, "version")
	if err != nil {
		return "", fmt.Errorf("seeding: asking the dolt CLI its version: %w", err)
	}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if strings.HasPrefix(line, "dolt version ") {
			return strings.TrimSpace(line), nil
		}
	}
	return "", errors.New("seeding: the dolt CLI printed no version line")
}

// moveToBase puts the copy's head and working set at the base, then reads the
// schema it is at and the issues it holds there. A base below the floor is refused
// here, before anything on the copy is changed.
func (s *seeding) moveToBase(ctx context.Context, rec *SeedRecord) error {
	if _, err := s.dolt(ctx, "reset", "--hard", s.cfg.Base); err != nil {
		return fmt.Errorf("seeding: moving the copy to the base: %w", err)
	}
	if err := s.expectHeadAtBase(ctx); err != nil {
		return err
	}
	var err error
	if rec.SchemaBefore, err = s.number(ctx, "the base's schema version", querySchemaVersion); err != nil {
		return err
	}
	floor := s.cfg.SchemaFloor
	if floor == 0 {
		floor = DefaultSchemaFloor
	}
	if rec.SchemaBefore < floor {
		return &SeedRefused{
			Class:  RefusedBaseSchema,
			Detail: fmt.Sprintf("the base is at schema version %d, below the floor %d", rec.SchemaBefore, floor),
		}
	}
	if rec.IgnoredSchemaBefore, err = s.number(ctx, "the base's ignored schema version", queryIgnoredVersion); err != nil {
		return err
	}
	if s.issues, err = s.number(ctx, "the base's issue count", queryIssues); err != nil {
		return err
	}
	return nil
}

// expectHeadAtBase fails when the copy's head is not the base.
func (s *seeding) expectHeadAtBase(ctx context.Context) error {
	head, err := s.text(ctx, "the copy's head", queryHead)
	if err != nil {
		return err
	}
	if head != s.cfg.Base {
		return fmt.Errorf("seeding: the copy's head is %s after the move, want the base %s", head, s.cfg.Base)
	}
	return nil
}

// clearLocalPlane gives the copy the clone-local state of a fresh clone, in one
// batch with foreign keys unchecked, and asserts what it did: every cleared table
// is empty, the counter is back at its first value, the migration cursor is as it
// was, and the store's committed state has not moved.
func (s *seeding) clearLocalPlane(ctx context.Context, rec *SeedRecord) error {
	_, rows, err := s.runner.Query(ctx, s.dbDir, queryIgnoredTables)
	if err != nil {
		return fmt.Errorf("seeding: listing the ignored tables: %w", err)
	}
	names := make([]string, len(rows))
	for i, row := range rows {
		names[i] = row[0].Text
	}
	// The store's collation puts "wisps" ahead of "wisp_...", byte order does not:
	// sort here so that the plan does not depend on either.
	slices.Sort(names)
	plan, err := classifyIgnoredPlane(names)
	if err != nil {
		return err
	}

	cursorRows, err := s.number(ctx, "the migration cursor's rows", queryCursorRows)
	if err != nil {
		return err
	}
	cleared := countAll(plan.Clear)
	if rec.IgnoredRowsCleared, err = s.countOr0(ctx, "the rows to clear", cleared); err != nil {
		return err
	}

	var batch strings.Builder
	batch.WriteString("SET foreign_key_checks = 0;\n")
	for _, name := range plan.Clear {
		fmt.Fprintf(&batch, "DELETE FROM `%s`;\n", name)
	}
	for _, name := range plan.Counter {
		fmt.Fprintf(&batch, "UPDATE `%s` SET next_seq = 0 WHERE id = 0;\n", name)
	}
	if _, err := s.dolt(ctx, "sql", "-q", batch.String()); err != nil {
		return fmt.Errorf("seeding: clearing the clone-local plane: %w", err)
	}

	if left, err := s.countOr0(ctx, "the rows left in the cleared tables", cleared); err != nil {
		return err
	} else if left != 0 {
		return &SeedRefused{Class: RefusedIgnoredPlane, Detail: fmt.Sprintf("%d rows remain in the cleared tables", left)}
	}
	if len(plan.Counter) > 0 {
		_, counter, err := s.runner.Query(ctx, s.dbDir, queryCounterRows)
		if err != nil {
			return fmt.Errorf("seeding: reading the counter: %w", err)
		}
		if len(counter) != 1 || counter[0][0].Text != "0" || counter[0][1].Text != "0" {
			return &SeedRefused{Class: RefusedIgnoredPlane, Detail: fmt.Sprintf("the counter table does not hold exactly its first value: it has %d rows", len(counter))}
		}
	}
	cursorAfter, err := s.number(ctx, "the migration cursor's rows", queryCursorRows)
	if err != nil {
		return err
	}
	ignoredAfter, err := s.number(ctx, "the ignored schema version", queryIgnoredVersion)
	if err != nil {
		return err
	}
	if cursorAfter != cursorRows || ignoredAfter != rec.IgnoredSchemaBefore {
		return &SeedRefused{Class: RefusedIgnoredPlane, Detail: "clearing the clone-local plane moved the ignored track's migration cursor"}
	}
	changes, err := s.number(ctx, "the copy's uncommitted changes", queryChanges)
	if err != nil {
		return err
	}
	if changes != 0 {
		return &SeedRefused{Class: RefusedIgnoredPlane, Detail: fmt.Sprintf("clearing the clone-local plane left %d changed tables in the working set", changes)}
	}
	return s.expectHeadAtBase(ctx)
}

// countAll is the statement that adds up the rows of the named tables. The names
// are identifiers the policy has already checked.
func countAll(tables []string) string {
	parts := make([]string, len(tables))
	for i, name := range tables {
		parts[i] = "(SELECT COUNT(*) FROM `" + name + "`)"
	}
	return strings.Join(parts, " + ")
}

// countOr0 runs the statement SELECT <sum>, or answers 0 for no sum at all.
func (s *seeding) countOr0(ctx context.Context, what, sum string) (int, error) {
	if sum == "" {
		return 0, nil
	}
	return s.number(ctx, what, "SELECT "+sum)
}

// strip removes every remote or backup the copy has, which kind is either of the
// dolt CLI's two words for them, and says how many it removed. It asks the CLI
// what is listed afterwards and refuses with class when anything is: the removal's
// own exit says nothing about whether it did.
func (s *seeding) strip(ctx context.Context, kind, class string) (int, error) {
	names, err := s.targets(ctx, kind)
	if err != nil {
		return 0, err
	}
	for _, name := range names {
		if _, err := s.dolt(ctx, kind, "remove", name); err != nil {
			return 0, fmt.Errorf("seeding: removing a %s from the copy: %w", kind, err)
		}
	}
	left, err := s.targets(ctx, kind)
	if err != nil {
		return 0, err
	}
	if len(left) > 0 {
		return 0, &SeedRefused{Class: class, Detail: fmt.Sprintf("%d %s still listed after the removal of %d", len(left), kind, len(names))}
	}
	return len(names), nil
}

// targets lists the names of the copy's remotes or backups, as the dolt CLI does.
func (s *seeding) targets(ctx context.Context, kind string) ([]string, error) {
	res, err := s.dolt(ctx, kind, "-v")
	if err != nil {
		return nil, fmt.Errorf("seeding: listing the copy's %ss: %w", kind, err)
	}
	var names []string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if words := strings.Fields(line); len(words) > 0 && !strings.HasPrefix(line, "Warning:") {
			names = append(names, words[0])
		}
	}
	return names, nil
}

// writeProjectFile gives the copy the project file a bd looks for, with the
// identity the store carries. A store with none is given a file without one, and
// the seeding says so; it is never given an identity it did not have.
func (s *seeding) writeProjectFile(ctx context.Context) error {
	_, rows, err := s.runner.Query(ctx, s.dbDir, queryProjectID)
	if err != nil {
		return fmt.Errorf("seeding: reading the copy's project id: %w", err)
	}
	id := ""
	if len(rows) > 0 {
		id = rows[0][0].Text
	}
	if id == "" && s.cfg.Note != nil {
		s.cfg.Note("identity: db-has-none")
	}
	data, err := renderSeedMetadata(s.db, id)
	if err != nil {
		return fmt.Errorf("seeding: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.WorkDir, ".beads", "metadata.json"), data, 0o600); err != nil {
		return fmt.Errorf("seeding: %w", err)
	}
	return nil
}

// migrate runs the migration, the first bd command on the copy, and measures it
// from outside: what the copy was at before, what it is at after, and how long the
// command took. The observer is told immediately before the command starts, and
// nothing runs between.
func (s *seeding) migrate(ctx context.Context, rec *SeedRecord) error {
	logBefore, err := s.number(ctx, "the copy's log length", queryLogLength)
	if err != nil {
		return err
	}
	if s.cfg.Observer != nil {
		if err := s.cfg.Observer.Observe(ctx, SeedMigrating{}); err != nil {
			return fmt.Errorf("the observer stopped the seeding before the migration: %w", err)
		}
	}
	start := time.Now()
	res, err := s.runner.Bd(ctx, s.cfg.WorkDir, "migrate", "schema", "--json")
	rec.MigrationSeconds = time.Since(start).Seconds()
	if err != nil {
		var exit *doltcli.ExitError
		if !errors.As(err, &exit) {
			return fmt.Errorf("seeding: running the migration: %w", err)
		}
		class := refusalClassOf(string(exit.Stdout) + string(exit.Stderr))
		if class == "" {
			class = RefusedMigration
		}
		return exitRefusal(class, fmt.Sprintf("the migration exited with status %d", exit.ExitCode), exit)
	}
	if class := refusalClassOf(string(res.Stdout) + string(res.Stderr)); class != "" {
		return &SeedRefused{Class: class, Detail: "the migration ran and said it was refused"}
	}

	if rec.SchemaAfter, err = s.number(ctx, "the migrated schema version", querySchemaVersion); err != nil {
		return err
	}
	if rec.IgnoredSchemaAfter, err = s.number(ctx, "the migrated ignored schema version", queryIgnoredVersion); err != nil {
		return err
	}
	logAfter, err := s.number(ctx, "the migrated log length", queryLogLength)
	if err != nil {
		return err
	}
	rec.MigrationCommits = logAfter - logBefore
	rec.SeedHead, err = s.text(ctx, "the migrated head", queryHead)
	return err
}

// verify asserts what the recipe promised once the migration is done: nothing of
// the copy can reach another store, the migration got as far as the integration
// does, no issue was lost to it, and versioned history is still off.
func (s *seeding) verify(ctx context.Context, rec SeedRecord) error {
	for _, k := range []struct{ kind, class string }{{"remote", RefusedRemotes}, {"backup", RefusedBackups}} {
		left, err := s.targets(ctx, k.kind)
		if err != nil {
			return err
		}
		if len(left) > 0 {
			return &SeedRefused{Class: k.class, Detail: fmt.Sprintf("%d %s listed after the migration", len(left), k.kind)}
		}
	}
	if rec.SchemaAfter != s.cfg.LatestSchema {
		return &SeedRefused{Class: RefusedMigration, Detail: fmt.Sprintf("the migration left the main track at %d, the integration's latest is %d", rec.SchemaAfter, s.cfg.LatestSchema)}
	}
	if rec.IgnoredSchemaAfter != s.cfg.LatestIgnored {
		return &SeedRefused{Class: RefusedMigration, Detail: fmt.Sprintf("the migration left the ignored track at %d, the integration's latest is %d", rec.IgnoredSchemaAfter, s.cfg.LatestIgnored)}
	}
	issues, err := s.number(ctx, "the migrated issue count", queryIssues)
	if err != nil {
		return err
	}
	if issues != s.issues {
		return &SeedRefused{Class: RefusedMigration, Detail: fmt.Sprintf("the copy held %d issues before the migration and holds %d after", s.issues, issues)}
	}
	on, err := s.number(ctx, "the versioned history setting", queryHistoryOn)
	if err != nil {
		return err
	}
	if on != 0 {
		return errors.New("seeding: versioned history is on in the seeded copy")
	}
	return nil
}

// dolt runs the dolt CLI in the copy's database directory.
func (s *seeding) dolt(ctx context.Context, args ...string) (doltcli.Result, error) {
	return s.runner.Dolt(ctx, s.dbDir, args...)
}

// number runs a statement that answers one number.
func (s *seeding) number(ctx context.Context, what, sql string) (int, error) {
	text, err := s.text(ctx, what, sql)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("seeding: %s is %q, not a number", what, text)
	}
	return n, nil
}

// text runs a statement that answers one value.
func (s *seeding) text(ctx context.Context, what, sql string) (string, error) {
	_, rows, err := s.runner.Query(ctx, s.dbDir, sql)
	if err != nil {
		return "", fmt.Errorf("seeding: reading %s: %w", what, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].Null {
		return "", fmt.Errorf("seeding: %s is not a single value", what)
	}
	return rows[0][0].Text, nil
}

// copyTree copies the tree at src to dst: directories and regular files, with the
// modes and the modification times they had, the way cp -a copies them. It is made
// in process because a copy is not a dolt or a bd command and nothing but the
// seeding's runner starts one. Anything else in the tree is refused.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return copyFile(path, target, info)
		default:
			return fmt.Errorf("%s is neither a file nor a directory", rel)
		}
	})
}

func copyFile(src, dst string, info fs.FileInfo) error {
	in, err := os.Open(src) // #nosec G304 -- a file of the oracle's own tree, found by walking it
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm()|0o600) // #nosec G304 -- a path inside the copy the seeding is making
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}
