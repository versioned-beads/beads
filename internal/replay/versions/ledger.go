package versions

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// Ledger reads what one bd command did to a work clone, from Dolt's own account of the
// clone's history. It never asks bd what it did: a bd that skipped a write or made one
// it did not need to would say the wrong thing, and the point is to catch that.
//
// The unit is one command: a Delta is taken between the clone's head commit before the
// command and its head after it. A real change to an issue is a change in Dolt's diff
// of the issue's durable projection between those two commits. That is the compared
// columns of the issues table, the issue's labels, and the dependency edges the issue
// owns. Dolt's diff is read cell by cell, literally, so a column that goes from NULL to
// the empty string is a change; the stamp columns, the derived is_blocked and the
// recorder's own bookkeeping are not part of the projection. How bd itself decides that
// a write changed nothing is not consulted.
type Ledger struct{}

// batchSize is how many ids go into one IN list, so that the statement stays well
// inside the length of a command-line argument.
const batchSize = 400

// The diff_type values of a Dolt diff table.
const (
	diffAdded    = "added"
	diffRemoved  = "removed"
	diffModified = "modified"
)

// Delta returns what the command that took the clone from the commit from to the
// commit to did, issue by issue. It reads the diff of the clone's tables between the
// two commits, and the tables as of each of them for the issues the diff names. An
// issue's entry is there if the diff mentions it in any of the issues, labels,
// dependencies or issue_versions tables, whether or not it changed in a way that
// counts: a version row for an issue that did not change is something to report.
//
// A ref that does not resolve, and a value in the clone that cannot be read as what the
// schema says it is, are errors, and the Delta returned with one is the zero Delta.
func (Ledger) Delta(ctx context.Context, dir, from, to string) (Delta, error) {
	for _, ref := range []string{from, to} {
		if err := issueops.ValidateRef(ref); err != nil {
			return Delta{}, fmt.Errorf("versions: invalid ref %q: %w", ref, err)
		}
	}

	d := Delta{From: from, To: to}
	var err error
	if d.EpochBefore, err = epochAt(ctx, dir, from); err != nil {
		return Delta{}, err
	}
	if d.Epoch, err = epochAt(ctx, dir, to); err != nil {
		return Delta{}, err
	}

	touched := touches{}
	for _, diff := range []func(context.Context, string, string, string, touches) error{
		diffIssues, diffLabels, diffDependencies, diffVersions,
	} {
		if err := diff(ctx, dir, from, to, touched); err != nil {
			return Delta{}, err
		}
	}
	ids := touched.ids()

	before, fence, err := readHead(ctx, dir, from, ids)
	if err != nil {
		return Delta{}, err
	}
	after, _, err := readHead(ctx, dir, to, ids)
	if err != nil {
		return Delta{}, err
	}
	d.Fence = fence

	for _, id := range ids {
		t, was, is := touched[id], before[id], after[id]
		wisp := is.wisp
		if !is.exists {
			wisp = was.wisp
		}
		rows := slices.Clone(t.rows)
		sort.Slice(rows, func(i, j int) bool { return rows[i].Revision < rows[j].Revision })
		d.Issues = append(d.Issues, IssueDelta{
			ID:                id,
			Existed:           was.exists,
			Exists:            is.exists,
			Wisp:              wisp,
			GenerationBefore:  was.generation,
			GenerationAfter:   is.generation,
			RevisionBefore:    was.revision,
			RevisionAfter:     is.revision,
			Real:              t.real,
			Sections:          t.sectionList(),
			AddedEdges:        sortedKeys(t.added),
			RemovedEdges:      sortedKeys(t.removed),
			MaxRevisionBefore: was.highest,
			MaxRevisionAfter:  is.highest,
			PreviousChangeAt:  was.changedAt,
			Rows:              rows,
			Lost:              t.lost,
			Altered:           t.altered,
			Marked:            t.marked,
		})
	}
	return d, nil
}

// touch is what the diffs found out about one issue.
type touch struct {
	real           bool
	sections       map[string]bool
	added, removed map[string]bool
	rows           []Version
	lost, altered  int
	marked         bool
}

// touches are the issues the diffs of one Delta name, by id.
type touches map[string]*touch

// of returns the entry of an issue, making it when the diffs name the issue for the
// first time.
func (m touches) of(id string) *touch {
	t, ok := m[id]
	if !ok {
		t = &touch{sections: map[string]bool{}, added: map[string]bool{}, removed: map[string]bool{}}
		m[id] = t
	}
	return t
}

func (m touches) ids() []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// change records a real change in one section of the issue's projection.
func (t *touch) change(section string) {
	t.real = true
	t.sections[section] = true
}

// sectionList lists the sections that changed, in the order issue, labels,
// dependencies.
func (t *touch) sectionList() []string {
	var out []string
	for _, section := range []string{sectionIssue, sectionLabels, sectionDependencies} {
		if t.sections[section] {
			out = append(out, section)
		}
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	var out []string
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// diffAll is the diff of a whole table between two commits, with the columns of both
// sides.
func diffAll(ctx context.Context, dir, table, from, to string) ([]oracle.Row, error) {
	header, rows, err := doltcli.Query(ctx, dir, "SELECT * FROM dolt_commit_diff_"+table+
		" WHERE to_commit = "+doltcli.SQLQuote(to)+" AND from_commit = "+doltcli.SQLQuote(from))
	if err != nil {
		return nil, fmt.Errorf("versions: diffing %s between %s and %s: %w", table, from, to, err)
	}
	out := make([]oracle.Row, 0, len(rows))
	for _, cells := range rows {
		out = append(out, oracle.Row{Columns: header, Cells: cells})
	}
	return out, nil
}

// diffIssues finds the issues whose row was added, removed, or changed in a column of
// the projection. A row that changed only in the stamp columns, in is_blocked or in the
// recorder's bookkeeping still puts its issue in the Delta, because what the fence did
// to a legacy record shows there, but it is not a real change.
func diffIssues(ctx context.Context, dir, from, to string, touched touches) error {
	rows, err := diffAll(ctx, dir, "issues", from, to)
	if err != nil {
		return err
	}
	columns := comparedColumns(issueFields)
	for _, row := range rows {
		id := ownerOf(row, "id")
		if id == "" {
			return fmt.Errorf("versions: the diff of issues between %s and %s has a row with no id", from, to)
		}
		t := touched.of(id)
		switch kind := cellOf(row, "diff_type").Text; kind {
		case diffAdded, diffRemoved:
			t.change(sectionIssue)
		case diffModified:
			if differs(row, columns) {
				t.change(sectionIssue)
			}
		default:
			return fmt.Errorf("versions: the diff of issues between %s and %s has the diff_type %q", from, to, kind)
		}
	}
	return nil
}

// diffLabels finds the issues that gained or lost a label. A label is a row of its
// own, keyed by both its columns, so it is added or removed and never modified.
func diffLabels(ctx context.Context, dir, from, to string, touched touches) error {
	rows, err := diffAll(ctx, dir, "labels", from, to)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id := ownerOf(row, "issue_id")
		if id == "" {
			return fmt.Errorf("versions: the diff of labels between %s and %s has a row with no issue_id", from, to)
		}
		touched.of(id).change(sectionLabels)
	}
	return nil
}

// diffDependencies finds the issues whose edges changed. An edge belongs to the issue
// that owns it, the one in its issue_id column, and not to the issue it points at; the
// targets of the edges an owner gained and lost are kept, because a delete removes the
// edges that point at the deleted issue from their owners and that has to be told from
// every other edge change.
func diffDependencies(ctx context.Context, dir, from, to string, touched touches) error {
	rows, err := diffAll(ctx, dir, "dependencies", from, to)
	if err != nil {
		return err
	}
	columns := comparedColumns(dependencyFields)
	for _, row := range rows {
		was, is := side(row, "from_"), side(row, "to_")
		oldOwner, newOwner := was.text("issue_id"), is.text("issue_id")
		oldTarget, newTarget := was.target(), is.target()
		switch kind := cellOf(row, "diff_type").Text; kind {
		case diffAdded:
			t := touched.of(newOwner)
			t.change(sectionDependencies)
			t.added[newTarget] = true
		case diffRemoved:
			t := touched.of(oldOwner)
			t.change(sectionDependencies)
			t.removed[oldTarget] = true
		case diffModified:
			if !differs(row, columns) {
				continue
			}
			touched.of(newOwner).change(sectionDependencies)
			if oldOwner != newOwner || oldTarget != newTarget {
				old := touched.of(oldOwner)
				old.change(sectionDependencies)
				old.removed[oldTarget] = true
				touched.of(newOwner).added[newTarget] = true
			}
		default:
			return fmt.Errorf("versions: the diff of dependencies between %s and %s has the diff_type %q", from, to, kind)
		}
		if oldOwner == "" && newOwner == "" {
			return fmt.Errorf("versions: the diff of dependencies between %s and %s has a row with no issue_id", from, to)
		}
	}
	return nil
}

// versionDiffColumns is the select list over the diff of issue_versions. The columns
// that are compared are compared in the statement, because durable_state is a LONGBLOB
// and a CSV of one is not something to carry through the harness for the sake of a
// comparison: altered is 1 for a row that was there at both ends and differs in anything
// but its removal mark, marked is 1 for one whose removal mark differs.
const versionDiffColumns = "diff_type, COALESCE(to_issue_id, from_issue_id), COALESCE(to_revision, from_revision), " +
	"to_epoch, to_change_actor, to_attribution_status, to_change_at, " +
	"CASE WHEN diff_type = 'modified' AND NOT (to_epoch <=> from_epoch AND to_change_actor <=> from_change_actor " +
	"AND to_change_agent <=> from_change_agent AND to_change_message <=> from_change_message " +
	"AND to_change_at <=> from_change_at AND to_attribution_status <=> from_attribution_status " +
	"AND to_durable_state <=> from_durable_state) THEN 1 ELSE 0 END, " +
	"CASE WHEN diff_type = 'modified' AND NOT (to_removed_at <=> from_removed_at " +
	"AND to_removed_reason <=> from_removed_reason AND to_removed_restriction <=> from_removed_restriction) THEN 1 ELSE 0 END"

// diffVersions reads the version rows the action added and what it did to the ones
// that were there.
func diffVersions(ctx context.Context, dir, from, to string, touched touches) error {
	_, rows, err := doltcli.Query(ctx, dir, "SELECT "+versionDiffColumns+" FROM dolt_commit_diff_issue_versions"+
		" WHERE to_commit = "+doltcli.SQLQuote(to)+" AND from_commit = "+doltcli.SQLQuote(from))
	if err != nil {
		return fmt.Errorf("versions: diffing issue_versions between %s and %s: %w", from, to, err)
	}
	for _, row := range rows {
		if len(row) != 9 {
			return fmt.Errorf("versions: a row of the issue_versions diff has %d columns, want 9", len(row))
		}
		id := row[1].Text
		if row[1].Null || id == "" {
			return fmt.Errorf("versions: the diff of issue_versions between %s and %s has a row with no issue_id", from, to)
		}
		t := touched.of(id)
		switch kind := row[0].Text; kind {
		case diffAdded:
			version, err := scanVersion(id, row)
			if err != nil {
				return err
			}
			t.rows = append(t.rows, version)
		case diffRemoved:
			t.lost++
		case diffModified:
			if row[7].Text == "1" {
				t.altered++
			}
			if row[8].Text == "1" {
				t.marked = true
			}
		default:
			return fmt.Errorf("versions: the diff of issue_versions between %s and %s has the diff_type %q", from, to, kind)
		}
	}
	return nil
}

func scanVersion(id string, row []doltcli.Cell) (Version, error) {
	revision, err := strconv.ParseInt(row[2].Text, 10, 64)
	if row[2].Null || err != nil {
		return Version{}, fmt.Errorf("versions: a version of %s has the revision %q, which is not a number", id, row[2].Text)
	}
	epoch, err := strconv.Atoi(row[3].Text)
	if row[3].Null || err != nil {
		return Version{}, fmt.Errorf("versions: revision %d of %s has the epoch %q, which is not a number", revision, id, row[3].Text)
	}
	at, err := parseTime(row[6].Text)
	if row[6].Null || err != nil {
		return Version{}, fmt.Errorf("versions: revision %d of %s has the change_at %q, which is not a time", revision, id, row[6].Text)
	}
	return Version{Revision: revision, Epoch: epoch, Actor: row[4].Text, Status: row[5].Text, At: at}, nil
}

// comparedColumns are the columns of a table that supply a compared key of the
// durable projection.
func comparedColumns(fields []Field) []string {
	var columns []string
	for _, f := range fields {
		if f.Class == ClassCompared {
			columns = append(columns, f.Columns...)
		}
	}
	return columns
}

// differs reports whether a modified row of a diff table differs between its two sides
// in any of columns. The cells are compared as the CSV gives them, so NULL and the
// empty string are different values, and a column the diff does not have is NULL on
// both sides.
func differs(row oracle.Row, columns []string) bool {
	for _, column := range columns {
		was, is := cellOf(row, "from_"+column), cellOf(row, "to_"+column)
		if was.Null != is.Null || was.Text != is.Text {
			return true
		}
	}
	return false
}

// cellOf is a cell of the row, NULL when the row was not read with that column.
func cellOf(row oracle.Row, name string) doltcli.Cell {
	if cell, ok := row.Cell(name); ok {
		return cell
	}
	return doltcli.Cell{Null: true}
}

// ownerOf is the value of a key column of a diff row: the new one, or the old one for
// a row that was removed.
func ownerOf(row oracle.Row, column string) string {
	if cell := cellOf(row, "to_"+column); !cell.Null {
		return cell.Text
	}
	return cellOf(row, "from_"+column).Text
}

// side is the columns of a diff row that belong to one end of the diff, named as the
// table names them, so that a row of the diff can be read as a row of the table.
func side(row oracle.Row, prefix string) *cells {
	out := oracle.Row{}
	for i, column := range row.Columns {
		if name, ok := strings.CutPrefix(column, prefix); ok && i < len(row.Cells) {
			out.Columns = append(out.Columns, name)
			out.Cells = append(out.Cells, row.Cells[i])
		}
	}
	return &cells{table: "dependencies", row: out}
}

// head is what the work clone held about one issue as of one commit.
type head struct {
	exists     bool
	generation *int64
	revision   int64
	wisp       bool
	// highest is the highest revision the issue has in issue_versions, 0 when it has
	// none, and changedAt the change_at of that row.
	highest   int64
	changedAt time.Time
}

// readHead reads the issues and the versions of ids as of ref. It reports whether the
// issues table has the write fence column there. An id with no row is a head that does
// not exist.
func readHead(ctx context.Context, dir, ref string, ids []string) (map[string]*head, bool, error) {
	heads := make(map[string]*head, len(ids))
	for _, id := range ids {
		heads[id] = &head{}
	}

	fence := false
	for n, batch := range inBatches(ids) {
		header, rows, err := oracle.Select(ctx, dir, ref, "issues", "*", inList("id", batch))
		if err != nil {
			return nil, false, fmt.Errorf("versions: reading the issues as of %s: %w", ref, err)
		}
		if n == 0 {
			fence = slices.Contains(header, writeFenceColumn)
		}
		for _, cells := range rows {
			if err := scanHead(heads, oracle.Row{Columns: header, Cells: cells}); err != nil {
				return nil, false, err
			}
		}
	}

	if len(ids) == 0 {
		return heads, fence, nil
	}
	for _, batch := range inBatches(ids) {
		_, rows, err := oracle.Select(ctx, dir, ref, "issue_versions", "issue_id, revision, change_at",
			inList("issue_id", batch)+" ORDER BY issue_id, revision DESC")
		if err != nil {
			return nil, false, fmt.Errorf("versions: reading the versions as of %s: %w", ref, err)
		}
		for _, row := range rows {
			h := heads[row[0].Text]
			if h == nil || h.highest != 0 {
				continue
			}
			revision, err := strconv.ParseInt(row[1].Text, 10, 64)
			if row[1].Null || err != nil {
				return nil, false, fmt.Errorf("versions: a version of %s as of %s has the revision %q, which is not a number", row[0].Text, ref, row[1].Text)
			}
			at, err := parseTime(row[2].Text)
			if row[2].Null || err != nil {
				return nil, false, fmt.Errorf("versions: revision %d of %s as of %s has the change_at %q, which is not a time", revision, row[0].Text, ref, row[2].Text)
			}
			h.highest, h.changedAt = revision, at
		}
	}
	return heads, fence, nil
}

// scanHead reads the fence marker, the revision counter and the wisp flags from a row
// of issues.
func scanHead(heads map[string]*head, row oracle.Row) error {
	c := cells{table: "issues", row: row}
	id := c.text("id")
	h := heads[id]
	if h == nil {
		return nil
	}
	h.exists = true
	if _, ok := c.value(writeFenceColumn); ok {
		generation := c.integer64(writeFenceColumn)
		h.generation = &generation
	}
	h.revision = c.integer64("current_revision")
	h.wisp = c.flag("ephemeral") || c.flag("no_history")
	if c.err != nil {
		return c.err
	}
	return nil
}

// epochAt is the store's epoch as of ref. A store that has not minted yet has no epoch
// row, and is at epoch 1.
func epochAt(ctx context.Context, dir, ref string) (int, error) {
	_, rows, err := oracle.Select(ctx, dir, ref, "store_epoch", "epoch", "ORDER BY id LIMIT 1")
	if err != nil {
		return 0, fmt.Errorf("versions: reading the epoch as of %s: %w", ref, err)
	}
	if len(rows) == 0 || rows[0][0].Null {
		return 1, nil
	}
	epoch, err := strconv.Atoi(rows[0][0].Text)
	if err != nil {
		return 0, fmt.Errorf("versions: the epoch as of %s is %q, which is not a number", ref, rows[0][0].Text)
	}
	return epoch, nil
}

// inBatches splits ids into runs of at most batchSize. There is always one run, empty
// when there are no ids, so that a query that is wanted for its header is still made.
func inBatches(ids []string) [][]string {
	if len(ids) == 0 {
		return [][]string{nil}
	}
	var out [][]string
	for len(ids) > batchSize {
		out = append(out, ids[:batchSize])
		ids = ids[batchSize:]
	}
	return append(out, ids)
}

// inList is the WHERE clause that picks the rows of a column that are in ids, quoted
// the way the harness quotes every value it puts in a statement, and picks nothing for
// an empty list.
func inList(column string, ids []string) string {
	if len(ids) == 0 {
		return "WHERE 1 = 0"
	}
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, doltcli.SQLQuote(id))
	}
	return "WHERE " + column + " IN (" + strings.Join(quoted, ", ") + ")"
}
