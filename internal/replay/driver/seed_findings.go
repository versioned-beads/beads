package driver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/oracle"
)

// BaselineFinding is what comparing the seeded work clone with the oracle at the
// base came to, before the first step is replayed. It is counts only: a corpus's
// issues are never named in anything a run writes.
type BaselineFinding struct {
	// Compared is the number of issues the oracle held at the base.
	Compared int `json:"compared"`
	// Differing is how many of them the work clone does not hold exactly as the
	// oracle did. A pair the comparison could not decide counts as differing, because
	// only a matched pair is known to be the same. Zero says the seed copied the base
	// without changing it.
	Differing int `json:"differing"`
}

// EnableRefusal records that the product refused to turn versioned history on:
// some issue holds a value a version could not record. The run goes on without
// versioning and says so. Only the number is kept, because the product's own
// report names the issues.
type EnableRefusal struct {
	// Count is the number of issues the product named as unversionable.
	Count int `json:"count"`
}

// unversionable is the first line of the product's refusal to turn versioned
// history on, as far as the harness reads it: the number of issues it names.
var unversionable = regexp.MustCompile(`cannot turn versioned history on: ([0-9]+) issues? holds? a value a version could not record`)

// unversionableCount is how many issues the product named when it refused to turn
// versioned history on, and false when its output is not that refusal. The issues
// themselves are named further down the same report and are not read.
func unversionableCount(output string) (int, bool) {
	m := unversionable.FindStringSubmatch(output)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// issueIDsAt lists the ids of the issues table as of ref, in order. A ref that
// predates the table holds none.
func issueIDsAt(ctx context.Context, dir, ref string) ([]string, error) {
	columns, err := oracle.Columns(ctx, dir, ref, "issues")
	if err != nil {
		return nil, err
	}
	if columns == nil {
		return nil, nil
	}
	_, rows, err := doltcli.Query(ctx, dir, "SELECT id FROM issues AS OF "+doltcli.SQLQuote(ref)+" ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list the issues as of %s: %w", ref, err)
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		if len(row) != 1 {
			return nil, fmt.Errorf("list the issues as of %s: row %d has %d columns, not 1", ref, i+1, len(row))
		}
		ids[i] = row[0].Text
	}
	return ids, nil
}

// compareBaseline compares each of ids, the issues the oracle held at base, with
// the work clone's view of it, and keeps the counts. It runs before anything is
// written to the clone, and it is not a step: it writes no result row, takes no
// step number and names no issue, in the summary or in its errors. Columns that
// only one side has join the run's schema skew, as they do in a step, and are not
// differences.
func (r *runner) compareBaseline(ctx context.Context, env stepEnv, base string, ids []string) error {
	head, err := env.WorkHead(ctx)
	if err != nil {
		return fmt.Errorf("baseline: read the work clone's head: %w", err)
	}
	found := &BaselineFinding{}
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		want, err := env.OracleView(ctx, base, id)
		if err != nil {
			return fmt.Errorf("baseline: read the oracle's issue %d of %d at %s: %w", i+1, len(ids), base, err)
		}
		got, err := env.CandidateView(ctx, head, id)
		if err != nil {
			return fmt.Errorf("baseline: read the work clone's issue %d of %d at %s: %w", i+1, len(ids), head, err)
		}
		res, skew, err := compare.CompareViews(want, got)
		if err != nil {
			return fmt.Errorf("baseline: compare issue %d of %d: %w", i+1, len(ids), err)
		}
		mergeSkew(r.skew, skew)
		found.Compared++
		if !res.Matched {
			found.Differing++
		}
	}
	r.baseline = found
	return nil
}

// checkSeedHead refuses a seed record that does not belong to the work clone: its
// head must be a commit of the clone's own history, which is where the check for
// legacy rows reads the seeded issues from. Nothing has been written when it runs,
// so a record for another store is refused at the start and not hours in.
func checkSeedHead(ctx context.Context, cfg RunConfig) error {
	head := cfg.Seed.SeedHead
	if !baseHash.MatchString(head) {
		return fmt.Errorf("the seed record's head %q is not a commit hash", head)
	}
	n, err := queryCount(ctx, cfg.WorkDataDir, "checking the seed record against the work clone",
		"SELECT COUNT(*) FROM dolt_log WHERE commit_hash = "+doltcli.SQLQuote(head))
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("the seed record's head %s is not in the history of the work clone", head)
	}
	return nil
}

// queryCount runs a statement that answers one count in the dolt database at dir.
// what leads the error of a statement that does not.
func queryCount(ctx context.Context, dir, what, query string) (int, error) {
	_, rows, err := doltcli.Query(ctx, dir, query)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, fmt.Errorf("%s: unexpected result %v", what, rows)
	}
	n, err := strconv.Atoi(rows[0][0].Text)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a count", what, rows[0][0].Text)
	}
	return n, nil
}

// outboundTables are the system tables that list where a store can send its state:
// the remotes it can push to and the backups it can sync to. Reading them is not a
// call of anything that sends.
var outboundTables = []struct{ kind, table, class string }{
	{"remotes", "dolt_remotes", RefusedRemotes},
	{"backups", "dolt_backups", RefusedBackups},
}

// checkNoOutbound is the check every run ends with, whether or not its work clone
// was seeded: the clone holds no remote and no backup. A bd under test that left
// one behind has made the clone something a later push could reach, and a run in
// that state has not been run in isolation. Each kind that is left is a
// *SeedRefused of its own class, a fault of the harness and never a result. The
// target is left in place for whoever has to look at it.
func checkNoOutbound(ctx context.Context, dataDir string) error {
	var left []error
	for _, o := range outboundTables {
		n, err := queryCount(ctx, dataDir, "checking the work clone for "+o.kind, "SELECT COUNT(*) FROM "+o.table)
		if err != nil {
			return err
		}
		if n > 0 {
			left = append(left, &SeedRefused{Class: o.class, Detail: fmt.Sprintf("%d of the work clone's %s are left at the end of the run", n, o.kind)})
		}
	}
	return errors.Join(left...)
}
