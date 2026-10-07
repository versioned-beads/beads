package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/translate"
)

// DefaultSchemaFloor is the lowest main-track schema version a seeded base may be
// at. It is the lowest base any probe has covered, not a limit of the product: a
// base below it is refused rather than seeded on a hope.
const DefaultSchemaFloor = 64

// The classes of a refusal. A seeding refuses when something its recipe depends on
// does not hold, and says which, so that a reader can tell a fault of the harness
// from a finding about the product.
const (
	// RefusedBaseSchema: the base is below the schema floor.
	RefusedBaseSchema = "base-schema"
	// RefusedIgnoredPlane: the copy has an ignored table the recipe has no policy for.
	RefusedIgnoredPlane = "ignored-plane"
	// RefusedRemotes and RefusedBackups: an outbound target is still on the copy
	// after the recipe took every one away.
	RefusedRemotes = "remotes"
	RefusedBackups = "backups"
	// RefusedIdentity: a write on the copy was refused for the workspace identity.
	RefusedIdentity = "identity"
	// RefusedRemoteMigrateGate: a write on the copy was refused because it looks
	// remote-backed.
	RefusedRemoteMigrateGate = "remote-migrate-gate"
	// RefusedMigration: the migration verb itself failed. This is the one class that
	// is a finding about the product.
	RefusedMigration = "migration"
)

// SeedRefused is a seeding, or the first write on a seeded copy, that stopped
// because a precondition of the recipe did not hold. Detail is counts and names of
// tables only: never a row, an id or the product's own text.
type SeedRefused struct {
	Class  string
	Detail string
	// Exit is the refused command's own report, when a child refused. It is a field
	// and not an unwrapped cause: a step loop records a rejected write for any error
	// that unwraps to an *translate.ExecError, and a refusal of the seeding must end
	// the run instead.
	Exit *translate.ExecError
}

func (e *SeedRefused) Error() string {
	return "seed-refused(" + e.Class + "): " + e.Detail
}

// HarnessFault reports whether the refusal is a fault of the harness's own recipe
// or fixture. Every class but the migration's is: with the recipe applied the
// product has nothing to refuse.
func (e *SeedRefused) HarnessFault() bool { return e.Class != RefusedMigration }

// SeedConfig is everything one seeding needs. Every tool is named by path and
// none is looked up, so the dolt and the bd a seeding ran are the ones the caller
// chose.
type SeedConfig struct {
	// OracleDataDir is the oracle clone's dolt data directory. It is copied from
	// and never opened or written.
	OracleDataDir string
	// Base is the oracle commit the seeded store starts from.
	Base string
	// WorkDir is the bd project directory the seed is written into. Its .beads
	// directory must not exist yet.
	WorkDir string
	// DoltBin is the dolt CLI every dolt child runs, and BdBin the bd under test.
	DoltBin string
	BdBin   string
	// EnvRoot is a directory the seeding owns, outside WorkDir, for the home, the
	// configuration, the dolt root and the temporary files its children run with.
	EnvRoot string
	// LatestSchema and LatestIgnored are the integration's latest versions of the
	// main and the ignored migration tracks: what a seeded store must reach.
	LatestSchema  int
	LatestIgnored int
	// LinkedEngine names the dolt engine BdBin links, as a go.mod line does. It is
	// recorded, not checked: the caller reads it from the binary it built.
	LinkedEngine string
	// SchemaFloor is the lowest base schema accepted; zero means DefaultSchemaFloor.
	SchemaFloor int
	// Observer hears SeedMigrating and SeedCompleted. It may be nil.
	Observer Observer
	// Log, when set, records every dolt and bd child the seeding starts.
	Log *doltcli.ChildLog
	// Note, when set, hears what the seeding found that no field of the record
	// holds. Today that is one line: identity: db-has-none.
	Note func(string)
}

// childRunner is the one Runner the seeding's children go through.
func (c SeedConfig) childRunner() *doltcli.Runner {
	return &doltcli.Runner{
		DoltBin: c.DoltBin,
		BdBin:   c.BdBin,
		Env:     doltcli.ChildEnv(c.EnvRoot, c.WorkDir),
		Log:     c.Log,
	}
}

// ignoredPlan is what the seeding does to each table the copy's dolt_ignore
// matches: rows deleted, a counter put back to zero, or left as they are.
type ignoredPlan struct {
	Clear   []string
	Counter []string
	Keep    []string
}

// The clone-local plane's policy. A fresh clone holds none of another clone's
// events, leases, wisps or counters, so a seeded copy is given the state a fresh
// clone has: the tables below are emptied, the counter is put back to its first
// value, and the migration cursor is left alone because the migrations go on from
// it. Nothing is dropped or truncated: the tables stay, so the migrations that
// follow find the shape they expect.
const (
	// wispTablePrefix starts the name of every table a wisp's rows are kept in.
	wispTablePrefix = "wisp_"
	// counterTable holds the one counter row a fresh clone starts at zero.
	counterTable = "bd_events_seq"
	// cursorTable is the ignored track's migration cursor.
	cursorTable = "ignored_schema_migrations"
)

// clearedTables are the clone-local tables, besides the wisp tables, whose rows are
// deleted.
var clearedTables = []string{"events", "bd_events_journal", "leases", "local_metadata", "repo_mtimes", "wisps"}

// tableIdentifier is what a table name must look like to be put in a statement.
var tableIdentifier = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// classifyIgnoredPlane sorts the names of the copy's ignored tables into the
// policy. It refuses a name that is not a plain identifier and a name the policy
// does not know, and names it. A table named twice is listed once.
func classifyIgnoredPlane(names []string) (ignoredPlan, error) {
	var plan ignoredPlan
	var unknown []string
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !tableIdentifier.MatchString(name) {
			return ignoredPlan{}, &SeedRefused{
				Class:  RefusedIgnoredPlane,
				Detail: fmt.Sprintf("an ignored table is named %q, which is not a plain identifier", name),
			}
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		switch {
		case slices.Contains(clearedTables, name) || strings.HasPrefix(name, wispTablePrefix):
			plan.Clear = append(plan.Clear, name)
		case name == counterTable:
			plan.Counter = append(plan.Counter, name)
		case name == cursorTable:
			plan.Keep = append(plan.Keep, name)
		default:
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return ignoredPlan{}, &SeedRefused{
			Class:  RefusedIgnoredPlane,
			Detail: "the recipe has no policy for the ignored table " + strings.Join(unknown, ", "),
		}
	}
	slices.Sort(plan.Clear)
	slices.Sort(plan.Counter)
	slices.Sort(plan.Keep)
	return plan, nil
}

// seedMetadata is the project file of a fresh init, in the order its keys are
// written.
type seedMetadata struct {
	Database     string `json:"database"`
	Backend      string `json:"backend"`
	DoltMode     string `json:"dolt_mode"`
	DoltDatabase string `json:"dolt_database"`
	ProjectID    string `json:"project_id,omitempty"`
}

// renderSeedMetadata is the text of the seeded copy's .beads/metadata.json for the
// database directory named database: the fresh-init shape, with projectID as the
// copy's own project id, and without that key when projectID is empty.
func renderSeedMetadata(database, projectID string) ([]byte, error) {
	if database == "" {
		return nil, errors.New("the project file needs the name of the database directory")
	}
	return json.MarshalIndent(seedMetadata{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "embedded",
		DoltDatabase: database,
		ProjectID:    projectID,
	}, "", "  ")
}

// engineModule is the dolt engine's module path.
const engineModule = "github.com/dolthub/dolt/go"

// LinkedEngine names the dolt engine a bd built from the go.mod text links: the
// module path and version of its requirement, as go.mod writes them. The text is
// the caller's, not a binary's, because a binary built by another build system
// may carry no build information to read it from. An engine that go.mod replaces
// is named with its replacement.
func LinkedEngine(goMod []byte) (string, error) {
	var version, replacement string
	note := func(directive string, words []string) {
		switch {
		case directive == "require" && len(words) >= 2 && words[0] == engineModule:
			version = words[1]
		case directive == "replace" && len(words) >= 1 && words[0] == engineModule:
			if i := slices.Index(words, "=>"); i > 0 {
				replacement = strings.Join(words[i+1:], " ")
			}
		}
	}
	block := ""
	for _, line := range strings.Split(string(goMod), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		words := strings.Fields(line)
		switch {
		case len(words) == 0:
		case block != "":
			if words[0] == ")" {
				block = ""
			} else {
				note(block, words)
			}
		case len(words) == 2 && words[1] == "(" && (words[0] == "require" || words[0] == "replace"):
			block = words[0]
		case words[0] == "require" || words[0] == "replace":
			note(words[0], words[1:])
		}
	}
	if version == "" {
		return "", fmt.Errorf("go.mod does not require %s", engineModule)
	}
	if replacement != "" {
		return engineModule + " " + version + " => " + replacement, nil
	}
	return engineModule + " " + version, nil
}

// refusalClassOf names the refusal the product's own words in a command's output
// make, or "" when they make none. It is the one place the harness reads the
// product's text, and it reads two phrases of it: the workspace identity check,
// and the gate that will not migrate a store that looks remote-backed.
func refusalClassOf(output string) string {
	switch {
	case strings.Contains(output, "workspace identity mismatch detected"):
		return RefusedIdentity
	case strings.Contains(output, "refusing to auto-apply") && strings.Contains(output, "remote-backed database"):
		return RefusedRemoteMigrateGate
	}
	return ""
}

// exitRefusal is the refusal of class made by a child that exited above zero. The
// child's report is kept for a reader who wants the product's text, and stays out
// of the detail, which is the harness's own words.
func exitRefusal(class, detail string, exit *doltcli.ExitError) *SeedRefused {
	return &SeedRefused{
		Class:  class,
		Detail: detail,
		Exit: &translate.ExecError{
			Argv:     exit.Args,
			ExitCode: exit.ExitCode,
			Output:   string(exit.Stdout) + string(exit.Stderr),
			Err:      exit,
		},
	}
}
