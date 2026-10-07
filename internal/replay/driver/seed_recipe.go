package driver

import (
	"context"
	"errors"
)

// DefaultSchemaFloor is the lowest main-track schema version a seeded base may be
// at. It is the lowest base any probe has covered, not a limit of the product: a
// base below it is refused rather than seeded on a hope.
const DefaultSchemaFloor = 64

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
}

var errSeedNotBuilt = errors.New("seeding is not built")

// Seed gives cfg.WorkDir the oracle's state at cfg.Base and migrates it to the
// integration's schema, and says what it did.
func Seed(_ context.Context, _ SeedConfig) (SeedRecord, error) {
	return SeedRecord{}, errSeedNotBuilt
}
