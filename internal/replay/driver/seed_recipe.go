package driver

import (
	"context"
	"errors"

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
func (*SeedRefused) HarnessFault() bool { return false }

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

// classifyIgnoredPlane sorts the names of the copy's ignored tables into the
// policy. It refuses a name that is not a plain identifier and a name the policy
// does not know, and names it.
func classifyIgnoredPlane(_ []string) (ignoredPlan, error) {
	return ignoredPlan{}, errSeedNotBuilt
}

// renderSeedMetadata is the text of the seeded copy's .beads/metadata.json for the
// database directory named database: the fresh-init shape, with projectID as the
// copy's own project id, and without that key when projectID is empty.
func renderSeedMetadata(_, _ string) ([]byte, error) {
	return nil, errSeedNotBuilt
}

// LinkedEngine names the dolt engine a bd built from the go.mod text links: the
// module path and version of its requirement, as go.mod writes them. The text is
// the caller's, not a binary's, because a binary built by another build system
// may carry no build information to read it from.
func LinkedEngine(_ []byte) (string, error) {
	return "", errSeedNotBuilt
}

var errSeedNotBuilt = errors.New("seeding is not built")

// Seed gives cfg.WorkDir the oracle's state at cfg.Base and migrates it to the
// integration's schema, and says what it did.
func Seed(_ context.Context, _ SeedConfig) (SeedRecord, error) {
	return SeedRecord{}, errSeedNotBuilt
}
