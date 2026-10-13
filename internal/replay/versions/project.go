// Package versions is the replay harness's view of an issue's version history.
// It reads what the version-history mint stored for an issue at a commit, and it
// builds the same document from the issue's rows, so that the two can be held to
// each other.
//
// A version is stored as a types.Issue marshaled to canonical JSON, the
// durable_state of a row of issue_versions. The projection (Project) builds that
// same types.Issue from the cells of an oracle view, and marshals it with the
// mint's own path, not a hand-rendered one. A Go value is all the mint stores, so
// where a value cannot tell a NULL from its zero, the projection reads NULL as the
// zero, the way the store's own reader does; Lossy lists those fields, and the
// harness reports the list once per run.
//
// The reader (Read) answers one question of the stored side: the newest version of
// an issue as of a ref, byte for byte as the store holds it. The drift table
// (IssueFields, DependencyFields) classifies every key of the stored document, and
// is held to the types and to a fresh store's schema by tests, so a key or a
// column added later fails a build until someone decides what the axis does with
// it.
package versions

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/types"
)

// Project builds the issue the rows of view stand for: the document the mint would
// store for the issue if it recorded a version at the commit the view was read at,
// less what rows cannot supply. Marshaling it gives that document.
//
// It supplies the sections the translator replays: the issue's own columns and
// the dependencies it owns. It leaves out what the rows of those tables cannot
// say, which is named in the drift table: the lease overlay, the derived
// is_blocked, and the labels, which are a recorded gap until they are replayed.
//
// A cell that does not read as the type of its column is an error, not a zero:
// the store's own reader turns an unreadable time into the zero time, and a
// projection that did the same would agree with a stored version it should not.
func Project(view *oracle.View) (*types.Issue, error) {
	return cellsToIssue(view)
}

// cellsToIssue is Project. NULL reads as the zero value for a value-typed field
// and as nil for a pointer-typed one, so a pointer to a string keeps what a plain
// string loses: NULL is absent and the empty string is present and empty.
func cellsToIssue(view *oracle.View) (*types.Issue, error) {
	if view == nil {
		return nil, errors.New("versions: no view to project")
	}
	c := &cells{table: "issues", row: view.Issue}
	issue := &types.Issue{
		ID:                 c.text("id"),
		Title:              c.text("title"),
		Description:        c.text("description"),
		Design:             c.text("design"),
		AcceptanceCriteria: c.text("acceptance_criteria"),
		Notes:              c.text("notes"),
		SpecID:             c.text("spec_id"),
		Status:             types.Status(c.text("status")),
		Priority:           c.integer("priority"),
		IssueType:          types.IssueType(c.text("issue_type")),
		Assignee:           c.text("assignee"),
		Owner:              c.text("owner"),
		EstimatedMinutes:   c.integerPointer("estimated_minutes"),
		CreatedAt:          c.time("created_at"),
		CreatedBy:          c.text("created_by"),
		UpdatedAt:          c.time("updated_at"),
		StartedAt:          c.timePointer("started_at"),
		ClosedAt:           c.timePointer("closed_at"),
		CloseReason:        c.text("close_reason"),
		ClosedBySession:    c.text("closed_by_session"),
		DueAt:              c.timePointer("due_at"),
		DeferUntil:         c.timePointer("defer_until"),
		ExternalRef:        c.textPointer("external_ref"),
		SourceSystem:       c.text("source_system"),
		Metadata:           c.metadata("metadata"),
		CompactionLevel:    c.integer("compaction_level"),
		CompactedAt:        c.timePointer("compacted_at"),
		CompactedAtCommit:  c.textPointer("compacted_at_commit"),
		OriginalSize:       c.integer("original_size"),
		Sender:             c.text("sender"),
		Ephemeral:          c.flag("ephemeral"),
		NoHistory:          c.flag("no_history"),
		WispType:           types.WispType(c.text("wisp_type")),
		StorageClass:       types.StorageClass(c.text("storage_class")),
		Pinned:             c.flag("pinned"),
		IsTemplate:         c.flag("is_template"),
		AwaitType:          c.text("await_type"),
		AwaitID:            c.text("await_id"),
		Timeout:            time.Duration(c.integer64("timeout_ns")),
		Waiters:            c.strings("waiters"),
		MolType:            types.MolType(c.text("mol_type")),
		WorkType:           types.WorkType(c.text("work_type")),
		EventKind:          c.text("event_kind"),
		Actor:              c.text("actor"),
		Target:             c.text("target"),
		Payload:            c.text("payload"),
	}
	if c.err != nil {
		return nil, c.err
	}

	edges, err := projectEdges(view.Dependencies)
	if err != nil {
		return nil, err
	}
	issue.Dependencies = edges
	return issue, nil
}

// edge is a projected dependency with the surrogate id of its row, which orders
// edges that agree on everything else and is not carried into the document.
type edge struct {
	dep *types.Dependency
	id  string
}

// projectEdges builds the dependencies the issue owns, in the order the mint
// stores them: by target, then type, then the row's own id. The oracle lists
// them by the issue-id target column with the NULL ones first, which puts an edge
// to an external target ahead of an edge to a local issue where the mint puts it
// after, so the rows are re-sorted here. The target is what the mint's
// COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) gives,
// so it is the first of the three that is not NULL, however empty. The order is
// bytewise, as the store's binary collation compares.
func projectEdges(rows []oracle.Row) ([]*types.Dependency, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	edges := make([]edge, 0, len(rows))
	for _, row := range rows {
		c := &cells{table: "dependencies", row: row}
		dep := &types.Dependency{
			IssueID:     c.text("issue_id"),
			DependsOnID: c.target(),
			Type:        types.DependencyType(c.text("type")),
			CreatedAt:   c.time("created_at"),
			CreatedBy:   c.text("created_by"),
			Metadata:    c.text("metadata"),
			ThreadID:    c.text("thread_id"),
		}
		if c.err != nil {
			return nil, c.err
		}
		edges = append(edges, edge{dep: dep, id: c.text("id")})
	}
	sort.SliceStable(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.dep.DependsOnID != b.dep.DependsOnID {
			return a.dep.DependsOnID < b.dep.DependsOnID
		}
		if a.dep.Type != b.dep.Type {
			return a.dep.Type < b.dep.Type
		}
		return a.id < b.id
	})
	out := make([]*types.Dependency, len(edges))
	for i, e := range edges {
		out[i] = e.dep
	}
	return out, nil
}

// cells reads the columns of one row as the types of the fields they fill. The
// first column that does not read as its type is remembered and every later read
// is a zero, so a conversion is a list of reads and one check at the end.
type cells struct {
	table string
	row   oracle.Row
	err   error
}

// value returns the text of a column and whether it holds one: a NULL cell holds
// none, and neither does a column the row was not read with.
func (c *cells) value(name string) (string, bool) {
	cell, ok := c.row.Cell(name)
	if !ok || cell.Null {
		return "", false
	}
	return cell.Text, true
}

func (c *cells) fail(name, text, want string, cause error) {
	if c.err == nil {
		c.err = fmt.Errorf("versions: %s.%s holds %q, which is not %s: %w", c.table, name, text, want, cause)
	}
}

func (c *cells) text(name string) string {
	text, _ := c.value(name)
	return text
}

// textPointer is a text column that keeps the difference between NULL and the
// empty string.
func (c *cells) textPointer(name string) *string {
	text, ok := c.value(name)
	if !ok {
		return nil
	}
	return &text
}

func (c *cells) integer(name string) int {
	return int(c.integer64(name))
}

func (c *cells) integer64(name string) int64 {
	text, ok := c.value(name)
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		c.fail(name, text, "an integer", err)
		return 0
	}
	return n
}

func (c *cells) integerPointer(name string) *int {
	if _, ok := c.value(name); !ok {
		return nil
	}
	n := c.integer(name)
	return &n
}

// flag reads a boolean column the way the store's reader does: any non-zero
// number is true.
func (c *cells) flag(name string) bool {
	return c.integer64(name) != 0
}

// timeLayouts are the spellings of a time the store's own reader accepts, in its
// order. The reader answers the zero time for text it cannot read, which would
// make a corrupt cell agree with a stored version that has no such time, so this
// reads the same spellings and refuses the rest.
var timeLayouts = []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"}

func parseTime(text string) (time.Time, error) {
	var last error
	for _, layout := range timeLayouts {
		t, err := time.Parse(layout, text)
		if err == nil {
			return t.UTC(), nil
		}
		last = err
	}
	return time.Time{}, last
}

func (c *cells) time(name string) time.Time {
	text, ok := c.value(name)
	if !ok {
		return time.Time{}
	}
	t, err := parseTime(text)
	if err != nil {
		c.fail(name, text, "a time", err)
		return time.Time{}
	}
	return t
}

func (c *cells) timePointer(name string) *time.Time {
	if _, ok := c.value(name); !ok {
		return nil
	}
	t := c.time(name)
	if c.err != nil {
		return nil
	}
	return &t
}

// metadata is a JSON document the projection carries as written, so that the
// mint's own marshaling is what renders it and the number gate sees every digit.
// The store's reader leaves out a document that is empty or has no members, and
// so does this.
func (c *cells) metadata(name string) json.RawMessage {
	text, ok := c.value(name)
	if !ok || text == "" || text == "{}" {
		return nil
	}
	if !json.Valid([]byte(text)) {
		c.fail(name, text, "JSON", errors.New("invalid JSON"))
		return nil
	}
	return json.RawMessage(text)
}

// strings reads a JSON array of strings. The store's reader answers no strings for
// a value that does not parse; this refuses it.
func (c *cells) strings(name string) []string {
	text, ok := c.value(name)
	if !ok || text == "" || text == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		c.fail(name, text, "a JSON array of strings", err)
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// target is the id an edge points at, whichever of the three target columns holds it.
func (c *cells) target() string {
	for _, name := range []string{"depends_on_issue_id", "depends_on_wisp_id", "depends_on_external"} {
		if text, ok := c.value(name); ok {
			return text
		}
	}
	return ""
}
