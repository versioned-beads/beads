// Package oracle is the replay harness's read side: it returns what an issue
// looked like at a given commit of a corpus clone, straight from dolt, typed
// exactly as dolt wrote it so that SQL NULL and the empty string stay different.
//
// It issues only read-only statements against the local clone directory, through
// `dolt sql -q ... AS OF <ref> ...` (never `bd sql`, which refuses outright in
// embedded mode): a SELECT per table and, when a read fails, a few fixed probes
// that say why. It never writes and never connects to a shared server
// (NFR1/NFR2).
//
// A read has three outcomes that callers can tell apart. The ref resolves and
// has the row: a value. The ref predates the table, or the row is not there: nil
// and no error. The ref cannot be resolved at all, or anything else goes wrong:
// an error, and only the first of those satisfies errors.Is(err,
// ErrRefUnresolvable).
package oracle

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// ErrRefUnresolvable reports a ref that does not resolve in the database: a
// branch that does not exist, or a commit hash that was never there or that
// history flattening has since removed. A caller that expects exactly this
// failure tests for it with errors.Is, so it cannot pass on an unrelated one.
var ErrRefUnresolvable = errors.New("ref cannot be resolved in the database")

// errTableAbsent marks, inside this package, a table that does not exist at the
// ref. It never escapes: callers turn it into "nothing there".
var errTableAbsent = errors.New("table does not exist at the ref")

// Row is one row of one table as read at a ref. Columns are the table's columns
// as they stood at that ref, in schema order, and Cells[i] is the value of
// Columns[i]. A Row is read-only: its slices are shared with the other rows of
// the same read.
type Row struct {
	Columns []string
	Cells   []doltcli.Cell
}

// Cell returns the value of the named column, and false when the row was not
// read with such a column.
func (r Row) Cell(name string) (doltcli.Cell, bool) {
	for i, c := range r.Columns {
		if c == name && i < len(r.Cells) {
			return r.Cells[i], true
		}
	}
	return doltcli.Cell{}, false
}

// View is what "the same" means for an issue: its row in the issues table and
// the rows of every other replayed table that belong to it, as of one ref.
type View struct {
	Issue Row
	// Dependencies are the edges the issue owns, the rows whose issue_id is this
	// issue. An edge that points at the issue belongs to the view of the issue
	// that owns it. The order is fixed, by target issue then type, so two reads of
	// the same state agree.
	Dependencies []Row
}

// QueryAsOf returns the row for issueID in the issues table of the Dolt
// database at dir, as it stood at commit ref, as column-name to text, or nil if
// no such row existed at that commit (not yet created, since deleted, or the
// table itself not yet there). dir must be a local, unserved clone directory;
// doltcli refuses one that a dolt sql-server is serving.
//
// The text form reads NULL and the empty string alike, as "". It exists for
// callers that predate views; use ReadView where the difference matters.
//
// ref and issueID are spliced directly into the `dolt sql -q` argument (dolt's
// AS OF clause and CLI have no bind-parameter API), so both are
// charset-validated first via issueops.ValidateRef.
func QueryAsOf(ctx context.Context, dir, ref, issueID string) (map[string]string, error) {
	row, err := readIssue(ctx, dir, ref, issueID)
	if err != nil || row == nil {
		return nil, err
	}
	return doltcli.RowMap(row.Columns, row.Cells), nil
}

// ReadView returns the view of issueID at ref, or nil if the issue had no row
// there. A table the ref does not have yet contributes an empty section and is
// not an error. See the package comment for how failures are told apart.
func ReadView(ctx context.Context, dir, ref, issueID string) (*View, error) {
	issue, err := readIssue(ctx, dir, ref, issueID)
	if err != nil || issue == nil {
		return nil, err
	}
	deps, err := readDependencies(ctx, dir, ref, issueID)
	if err != nil {
		return nil, err
	}
	return &View{Issue: *issue, Dependencies: deps}, nil
}

// tableName is what a table name must look like to be spliced into a query.
var tableName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Columns returns the column names of table as they stood at ref, in schema
// order, or nil if the ref predates the table. It is the schema at a ref, read
// the same way as the rows.
func Columns(ctx context.Context, dir, ref, table string) ([]string, error) {
	if err := issueops.ValidateRef(ref); err != nil {
		return nil, fmt.Errorf("oracle: invalid ref %q: %w", ref, err)
	}
	if !tableName.MatchString(table) {
		return nil, fmt.Errorf("oracle: invalid table name %q", table)
	}
	header, _, err := readTable(ctx, dir, ref, table, "1 = 0")
	if errors.Is(err, errTableAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("oracle: reading the columns of %s AS OF %s: %w", table, ref, err)
	}
	return header, nil
}

// Select returns the columns of the rows of table at ref that tail picks, as the
// header dolt printed and the typed cells of each row, for a caller that needs a
// read of its own. A ref that predates the table gives a nil header and no rows
// and is not an error; a table that is there but has no row to pick still gives
// its header, which is how the two are told apart. See the package comment for
// how failures are told apart.
//
// columns is the select list and tail what follows the table: a WHERE, an ORDER
// BY, a LIMIT, or nothing. Both are spliced into the `dolt sql -q` argument, which
// has no bind-parameter API, so the caller writes them from fixed text and values
// quoted with doltcli.SQLQuote only. ref and table are validated here.
func Select(ctx context.Context, dir, ref, table, columns, tail string) ([]string, [][]doltcli.Cell, error) {
	if err := issueops.ValidateRef(ref); err != nil {
		return nil, nil, fmt.Errorf("oracle: invalid ref %q: %w", ref, err)
	}
	if !tableName.MatchString(table) {
		return nil, nil, fmt.Errorf("oracle: invalid table name %q", table)
	}
	header, rows, err := selectFrom(ctx, dir, ref, table, columns, tail)
	if errors.Is(err, errTableAbsent) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("oracle: reading %s AS OF %s: %w", table, ref, err)
	}
	return header, rows, nil
}

func readIssue(ctx context.Context, dir, ref, issueID string) (*Row, error) {
	if err := issueops.ValidateRef(ref); err != nil {
		return nil, fmt.Errorf("oracle: invalid ref %q: %w", ref, err)
	}
	if err := issueops.ValidateRef(issueID); err != nil {
		return nil, fmt.Errorf("oracle: invalid issue id %q: %w", issueID, err)
	}
	header, rows, err := readTable(ctx, dir, ref, "issues", "id = "+doltcli.SQLQuote(issueID))
	if errors.Is(err, errTableAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("oracle: querying %s AS OF %s: %w", issueID, ref, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &Row{Columns: header, Cells: rows[0]}, nil
}

func readDependencies(ctx context.Context, dir, ref, issueID string) ([]Row, error) {
	header, rows, err := readTable(ctx, dir, ref, "dependencies", "issue_id = "+doltcli.SQLQuote(issueID))
	if errors.Is(err, errTableAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("oracle: reading the dependencies of %s AS OF %s: %w", issueID, ref, err)
	}
	edges := make([]Row, len(rows))
	for i, cells := range rows {
		edges[i] = Row{Columns: header, Cells: cells}
	}
	sortEdges(edges)
	return edges, nil
}

// edgeOrder is how an issue's edges are ordered: by target issue, then type,
// then the other target columns. Sorting here, on the typed cells, keeps the
// order the same on both sides of a comparison whatever columns either schema
// has; a column a schema lacks reads as NULL, which sorts first.
var edgeOrder = []string{"depends_on_issue_id", "type", "depends_on_wisp_id", "depends_on_external", "thread_id"}

func sortEdges(edges []Row) {
	cell := func(r Row, col string) doltcli.Cell {
		if c, ok := r.Cell(col); ok {
			return c
		}
		return doltcli.Cell{Null: true}
	}
	sort.SliceStable(edges, func(i, j int) bool {
		for _, col := range edgeOrder {
			a, b := cell(edges[i], col), cell(edges[j], col)
			switch {
			case a.Null && b.Null:
				continue
			case a.Null:
				return true
			case b.Null:
				return false
			}
			if c := strings.Compare(a.Text, b.Text); c != 0 {
				return c < 0
			}
		}
		return false
	})
}

// readTable reads the rows of table at ref that satisfy where, which the caller
// builds from quoted values only. A failure is classified before it is returned:
// errTableAbsent if the ref has no such table, an error wrapping
// ErrRefUnresolvable if the ref does not resolve, otherwise the original error.
func readTable(ctx context.Context, dir, ref, table, where string) ([]string, [][]doltcli.Cell, error) {
	return selectFrom(ctx, dir, ref, table, "*", "WHERE "+where)
}

// selectFrom reads the columns of table at ref, followed by tail, and classifies
// a failure as readTable describes. columns and tail are built by the caller from
// fixed text and quoted values only.
func selectFrom(ctx context.Context, dir, ref, table, columns, tail string) ([]string, [][]doltcli.Cell, error) {
	query := fmt.Sprintf("SELECT %s FROM %s AS OF %s", columns, table, doltcli.SQLQuote(ref)) //nolint:gosec // G201 -- dolt sql -q has no bind-parameter API; ref is charset-validated and quoted, table is a fixed name, and columns and tail are built by the caller from fixed text and quoted values only
	if tail != "" {
		query += " " + tail
	}
	header, rows, err := doltcli.Query(ctx, dir, query)
	if err != nil {
		return nil, nil, classify(ctx, dir, ref, table, err)
	}
	return header, rows, nil
}

// classify says why a read failed by asking dolt, not by reading its error text,
// which is not a contract. The probes run only on the failure path. A directory
// that is not a readable dolt database tells nothing about the ref, so the
// original error stands; otherwise an unresolvable ref, then a missing table. A
// probe that fails because ctx is done tells nothing either, so an interrupt
// never reads as a ref that is gone.
func classify(ctx context.Context, dir, ref, table string, cause error) error {
	if _, _, err := doltcli.Query(ctx, dir, "SELECT COUNT(*) FROM dolt_log"); err != nil {
		return cause
	}
	if _, _, err := doltcli.Query(ctx, dir, "SELECT hashof("+doltcli.SQLQuote(ref)+")"); err != nil {
		// A canceled or expired context fails this probe whatever the ref is, whether
		// it was done before the probe started or ended it mid-run.
		if ctx.Err() != nil {
			return cause
		}
		return fmt.Errorf("%w: %q: %v", ErrRefUnresolvable, ref, cause)
	}
	_, tables, err := doltcli.Query(ctx, dir, "SHOW TABLES AS OF "+doltcli.SQLQuote(ref))
	if err != nil {
		return cause
	}
	for _, row := range tables {
		if len(row) == 1 && strings.EqualFold(row[0].Text, table) {
			return cause
		}
	}
	return errTableAbsent
}
