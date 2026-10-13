package versions

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// b11aPatch returns row with the given columns set: a column the row already has
// is replaced, any other is added.
func b11aPatch(row oracle.Row, extra ...any) oracle.Row {
	patch := b11aRow(extra...)
	out := oracle.Row{
		Columns: append([]string(nil), row.Columns...),
		Cells:   append(row.Cells[:0:0], row.Cells...),
	}
	for i, name := range patch.Columns {
		replaced := false
		for j, have := range out.Columns {
			if have == name {
				out.Cells[j] = patch.Cells[i]
				replaced = true
			}
		}
		if !replaced {
			out.Columns = append(out.Columns, name)
			out.Cells = append(out.Cells, patch.Cells[i])
		}
	}
	return out
}

// b11aIssueRow is an issues row with only the columns the store always has,
// patched with the given ones.
func b11aIssueRow(extra ...any) oracle.Row {
	return b11aPatch(b11aRow(
		"id", "x-1", "title", "T", "priority", "2",
		"created_at", "2026-10-12 10:00:00", "updated_at", "2026-10-12 10:00:00",
	), extra...)
}

// b11aEdgeRow is a dependencies row for an edge from x-1 with every optional
// column NULL, patched with the given ones.
func b11aEdgeRow(extra ...any) oracle.Row {
	return b11aPatch(b11aRow(
		"id", "00000000-0000-5000-8000-000000000001", "issue_id", "x-1", "type", "blocks",
		"created_at", "2026-10-12 10:00:00", "created_by", nil, "metadata", nil, "thread_id", nil,
		"depends_on_issue_id", nil, "depends_on_wisp_id", nil, "depends_on_external", nil,
	), extra...)
}

// b11aDoc is the document the base row projects to, plus extra members. The
// tests compare documents in canonical form, so the order here is free.
func b11aDoc(members ...string) string {
	all := append([]string{
		`"id":"x-1"`, `"title":"T"`, `"priority":2`,
		`"created_at":"2026-10-12T10:00:00Z"`, `"updated_at":"2026-10-12T10:00:00Z"`,
	}, members...)
	return "{" + strings.Join(all, ",") + "}"
}

// b11aDepDoc is one projected dependency of x-1 as it renders in a document.
func b11aDepDoc(members ...string) string {
	all := append([]string{`"issue_id":"x-1"`, `"created_at":"2026-10-12T10:00:00Z"`}, members...)
	return "{" + strings.Join(all, ",") + "}"
}

// b11aProjectBytes projects a view and marshals it the way the mint does,
// before canonicalization.
func b11aProjectBytes(view *oracle.View) ([]byte, error) {
	issue, err := cellsToIssue(view)
	if err != nil {
		return nil, err
	}
	return json.Marshal(issue)
}

// b11aProjectCanonical is b11aProjectBytes in canonical form.
func b11aProjectCanonical(view *oracle.View) ([]byte, error) {
	doc, err := b11aProjectBytes(view)
	if err != nil {
		return nil, err
	}
	return compare.Canonicalize(doc)
}

func b11aCanonicalText(t *testing.T, doc string) string {
	t.Helper()
	canon, err := compare.Canonicalize([]byte(doc))
	if err != nil {
		t.Fatalf("the expected document is not JSON: %v\n%s", err, doc)
	}
	return string(canon)
}

// TestB11aNullVsEmptyRules fixes what the projection does with each kind of cell,
// and with it where the version axis is blind. The mint stores a Go value, whose
// zero a NULL and an empty string and a 0 all collapse to; a row keeps the
// difference. So for the fields that cannot tell them apart the projection reads
// NULL as the zero value, the way the store's own reader does, and the harness
// states, once per run, which fields that covers.
func TestB11aNullVsEmptyRules(t *testing.T) {
	type conversion struct {
		name    string
		row     oracle.Row
		edges   []oracle.Row
		want    string
		wantErr bool
	}
	var cases []conversion

	// A text column of a value-typed omitempty field: NULL and the empty string
	// both leave the key out, and any text is carried.
	for _, key := range []string{
		"description", "design", "acceptance_criteria", "notes", "spec_id", "status",
		"issue_type", "assignee", "owner", "created_by", "close_reason", "closed_by_session",
		"source_system", "sender", "wisp_type", "storage_class", "await_type", "await_id",
		"mol_type", "work_type", "event_kind", "actor", "target", "payload",
	} {
		cases = append(cases,
			conversion{name: key + " NULL is the zero value", row: b11aIssueRow(key, nil), want: b11aDoc()},
			conversion{name: key + " empty is the zero value", row: b11aIssueRow(key, ""), want: b11aDoc()},
			conversion{name: key + " text is carried", row: b11aIssueRow(key, "v-"+key), want: b11aDoc(`"` + key + `":"v-` + key + `"`)},
		)
	}

	cases = append(cases,
		conversion{
			name: "text with a quote, a comma, a newline and markup is carried as written",
			row:  b11aIssueRow("description", "say \"hi\", <b>a & b</b>\nline two é 日本"),
			want: b11aDoc(`"description":"say \"hi\", <b>a & b</b>\nline two é 日本"`),
		},

		// A pointer field keeps what a value field loses.
		conversion{name: "external_ref NULL is absent", row: b11aIssueRow("external_ref", nil), want: b11aDoc()},
		conversion{name: "external_ref empty is present and empty", row: b11aIssueRow("external_ref", ""), want: b11aDoc(`"external_ref":""`)},
		conversion{name: "external_ref text is carried", row: b11aIssueRow("external_ref", "gh-9"), want: b11aDoc(`"external_ref":"gh-9"`)},
		conversion{name: "compacted_at_commit NULL is absent", row: b11aIssueRow("compacted_at_commit", nil), want: b11aDoc()},
		conversion{name: "compacted_at_commit empty is present and empty", row: b11aIssueRow("compacted_at_commit", ""), want: b11aDoc(`"compacted_at_commit":""`)},
		conversion{name: "estimated_minutes NULL is absent", row: b11aIssueRow("estimated_minutes", nil), want: b11aDoc()},
		conversion{name: "estimated_minutes 0 is present", row: b11aIssueRow("estimated_minutes", "0"), want: b11aDoc(`"estimated_minutes":0`)},
		conversion{name: "estimated_minutes 30 is carried", row: b11aIssueRow("estimated_minutes", "30"), want: b11aDoc(`"estimated_minutes":30`)},

		// Priority is not omitempty: 0 is a priority, and so is no priority.
		conversion{
			name: "priority 0 is written",
			row:  b11aIssueRow("priority", "0"),
			want: `{"created_at":"2026-10-12T10:00:00Z","id":"x-1","priority":0,"title":"T","updated_at":"2026-10-12T10:00:00Z"}`,
		},
		conversion{
			name: "priority NULL is written as 0",
			row:  b11aIssueRow("priority", nil),
			want: `{"created_at":"2026-10-12T10:00:00Z","id":"x-1","priority":0,"title":"T","updated_at":"2026-10-12T10:00:00Z"}`,
		},
		conversion{
			name: "created_at NULL is the zero time",
			row:  b11aIssueRow("created_at", nil),
			want: `{"created_at":"0001-01-01T00:00:00Z","id":"x-1","priority":2,"title":"T","updated_at":"2026-10-12T10:00:00Z"}`,
		},

		// Integers and booleans.
		conversion{name: "compaction_level 0 is the zero value", row: b11aIssueRow("compaction_level", "0"), want: b11aDoc()},
		conversion{name: "compaction_level NULL is the zero value", row: b11aIssueRow("compaction_level", nil), want: b11aDoc()},
		conversion{name: "compaction_level 2 is carried", row: b11aIssueRow("compaction_level", "2"), want: b11aDoc(`"compaction_level":2`)},
		conversion{name: "original_size 0 is the zero value", row: b11aIssueRow("original_size", "0"), want: b11aDoc()},
		conversion{name: "original_size 4096 is carried", row: b11aIssueRow("original_size", "4096"), want: b11aDoc(`"original_size":4096`)},
		conversion{name: "timeout_ns 0 is the zero value", row: b11aIssueRow("timeout_ns", "0"), want: b11aDoc()},
		conversion{name: "timeout_ns NULL is the zero value", row: b11aIssueRow("timeout_ns", nil), want: b11aDoc()},
		conversion{name: "timeout_ns is the timeout in nanoseconds", row: b11aIssueRow("timeout_ns", "5000000000"), want: b11aDoc(`"timeout":5000000000`)},
	)
	for _, key := range []string{"ephemeral", "no_history", "pinned", "is_template"} {
		cases = append(cases,
			conversion{name: key + " 1 is true", row: b11aIssueRow(key, "1"), want: b11aDoc(`"` + key + `":true`)},
			conversion{name: key + " 0 is false", row: b11aIssueRow(key, "0"), want: b11aDoc()},
			conversion{name: key + " NULL is false", row: b11aIssueRow(key, nil), want: b11aDoc()},
		)
	}

	// Times: every one is read at its exact text and rendered in UTC.
	for _, key := range []string{"started_at", "closed_at", "compacted_at", "due_at", "defer_until"} {
		cases = append(cases,
			conversion{name: key + " NULL is absent", row: b11aIssueRow(key, nil), want: b11aDoc()},
			conversion{name: key + " is rendered in UTC", row: b11aIssueRow(key, "2026-10-12 11:30:15"), want: b11aDoc(`"` + key + `":"2026-10-12T11:30:15Z"`)},
			conversion{name: key + " keeps its fraction", row: b11aIssueRow(key, "2026-10-12 11:30:15.123456"), want: b11aDoc(`"` + key + `":"2026-10-12T11:30:15.123456Z"`)},
		)
	}

	// The same text a column holds and does not parse is an error, not a zero.
	for _, column := range []string{
		"created_at", "updated_at", "started_at", "closed_at", "compacted_at", "due_at", "defer_until",
		"priority", "estimated_minutes", "compaction_level", "original_size", "timeout_ns",
	} {
		cases = append(cases, conversion{name: column + " that is not a value of its type is refused", row: b11aIssueRow(column, "not a value"), wantErr: true})
	}

	// metadata is JSON the projection carries as it is.
	cases = append(cases,
		conversion{name: "metadata NULL is absent", row: b11aIssueRow("metadata", nil), want: b11aDoc()},
		conversion{name: "metadata empty is absent", row: b11aIssueRow("metadata", ""), want: b11aDoc()},
		conversion{name: "metadata {} is absent", row: b11aIssueRow("metadata", "{}"), want: b11aDoc()},
		conversion{name: "metadata is carried", row: b11aIssueRow("metadata", `{"k":"v"}`), want: b11aDoc(`"metadata":{"k":"v"}`)},
		conversion{name: "metadata keys are sorted by canonicalization", row: b11aIssueRow("metadata", `{"b":2,"a":1}`), want: b11aDoc(`"metadata":{"a":1,"b":2}`)},
		conversion{name: "metadata spacing does not matter", row: b11aIssueRow("metadata", `{"k": "v", "n": [1, 2]}`), want: b11aDoc(`"metadata":{"k":"v","n":[1,2]}`)},

		// waiters is a JSON array of strings.
		conversion{name: "waiters NULL is absent", row: b11aIssueRow("waiters", nil), want: b11aDoc()},
		conversion{name: "waiters empty is absent", row: b11aIssueRow("waiters", ""), want: b11aDoc()},
		conversion{name: "waiters [] is absent", row: b11aIssueRow("waiters", "[]"), want: b11aDoc()},
		conversion{name: "waiters are carried", row: b11aIssueRow("waiters", `["a@x","b@y"]`), want: b11aDoc(`"waiters":["a@x","b@y"]`)},

		// What the document must not take from the row.
		conversion{name: "is_blocked is derived and is not read", row: b11aIssueRow("is_blocked", "1"), want: b11aDoc()},
		conversion{
			name: "columns the document has no key for are ignored",
			row: b11aIssueRow(
				"content_hash", "abc", "source_repo", ".", "hook_bead", "h-1", "role_bead", "r-1",
				"agent_state", "idle", "last_activity", "2026-10-12 10:00:00", "role_type", "x", "rig", "r",
				"row_lock", "99", "current_revision", "7", "participation_generation", "1", "zzz_future_column", "v",
			),
			want: b11aDoc(),
		},
		conversion{
			name: "a column the row lacks reads as the zero value",
			row:  b11aRow("id", "x-1", "created_at", "2026-10-12 10:00:00"),
			want: `{"created_at":"2026-10-12T10:00:00Z","id":"x-1","priority":0,"title":"","updated_at":"0001-01-01T00:00:00Z"}`,
		},
	)

	// Edges. The target is the first of the three target columns that is not NULL.
	edgeCases := []conversion{
		{
			name:  "an edge to an issue",
			row:   b11aIssueRow(),
			edges: []oracle.Row{b11aEdgeRow("depends_on_issue_id", "y-1")},
			want:  b11aDoc(`"dependencies":[` + b11aDepDoc(`"depends_on_id":"y-1"`, `"type":"blocks"`) + `]`),
		},
		{
			name:  "an edge to a wisp",
			row:   b11aIssueRow(),
			edges: []oracle.Row{b11aEdgeRow("depends_on_wisp_id", "w-1")},
			want:  b11aDoc(`"dependencies":[` + b11aDepDoc(`"depends_on_id":"w-1"`, `"type":"blocks"`) + `]`),
		},
		{
			name:  "an edge to an external target",
			row:   b11aIssueRow(),
			edges: []oracle.Row{b11aEdgeRow("depends_on_external", "external:p:c")},
			want:  b11aDoc(`"dependencies":[` + b11aDepDoc(`"depends_on_id":"external:p:c"`, `"type":"blocks"`) + `]`),
		},
		{
			name:  "the issue column is the target before the wisp column",
			row:   b11aIssueRow(),
			edges: []oracle.Row{b11aEdgeRow("depends_on_issue_id", "y-1", "depends_on_wisp_id", "w-1", "depends_on_external", "external:p:c")},
			want:  b11aDoc(`"dependencies":[` + b11aDepDoc(`"depends_on_id":"y-1"`, `"type":"blocks"`) + `]`),
		},
		{
			name:  "the wisp column is the target before the external column",
			row:   b11aIssueRow(),
			edges: []oracle.Row{b11aEdgeRow("depends_on_wisp_id", "w-1", "depends_on_external", "external:p:c")},
			want:  b11aDoc(`"dependencies":[` + b11aDepDoc(`"depends_on_id":"w-1"`, `"type":"blocks"`) + `]`),
		},
		{
			name:  "an edge's optional columns are carried when set",
			row:   b11aIssueRow(),
			edges: []oracle.Row{b11aEdgeRow("depends_on_issue_id", "y-1", "created_by", "alice", "thread_id", "t-1", "metadata", `{"gate": "any-children"}`)},
			want: b11aDoc(`"dependencies":[` + b11aDepDoc(
				`"depends_on_id":"y-1"`, `"type":"blocks"`, `"created_by":"alice"`, `"thread_id":"t-1"`,
				`"metadata":"{\"gate\": \"any-children\"}"`) + `]`),
		},
		{
			name:  "an edge's empty metadata document is a string, not an absence",
			row:   b11aIssueRow(),
			edges: []oracle.Row{b11aEdgeRow("depends_on_issue_id", "y-1", "metadata", "{}")},
			want:  b11aDoc(`"dependencies":[` + b11aDepDoc(`"depends_on_id":"y-1"`, `"type":"blocks"`, `"metadata":"{}"`) + `]`),
		},
		{
			name:  "an edge's empty optional columns are absent",
			row:   b11aIssueRow(),
			edges: []oracle.Row{b11aEdgeRow("depends_on_issue_id", "y-1", "created_by", "", "thread_id", "", "metadata", "")},
			want:  b11aDoc(`"dependencies":[` + b11aDepDoc(`"depends_on_id":"y-1"`, `"type":"blocks"`) + `]`),
		},
		{
			name: "the oracle lists the external edge first and the mint stores it last",
			row:  b11aIssueRow(),
			edges: []oracle.Row{
				b11aEdgeRow("id", "00000000-0000-5000-8000-000000000002", "type", "blocks", "depends_on_external", "external:otherproj:cap"),
				b11aEdgeRow("id", "00000000-0000-5000-8000-000000000003", "type", "related", "depends_on_issue_id", "aaa-1"),
			},
			want: b11aDoc(`"dependencies":[` +
				b11aDepDoc(`"depends_on_id":"aaa-1"`, `"type":"related"`) + `,` +
				b11aDepDoc(`"depends_on_id":"external:otherproj:cap"`, `"type":"blocks"`) + `]`),
		},
		{
			name: "edges to one target are ordered by type",
			row:  b11aIssueRow(),
			edges: []oracle.Row{
				b11aEdgeRow("type", "related", "depends_on_issue_id", "y-1"),
				b11aEdgeRow("type", "blocks", "depends_on_issue_id", "y-1"),
			},
			want: b11aDoc(`"dependencies":[` +
				b11aDepDoc(`"depends_on_id":"y-1"`, `"type":"blocks"`) + `,` +
				b11aDepDoc(`"depends_on_id":"y-1"`, `"type":"related"`) + `]`),
		},
	}
	cases = append(cases, edgeCases...)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := b11aProjectCanonical(&oracle.View{Issue: c.row, Dependencies: c.edges})
			if c.wantErr {
				if err == nil {
					t.Fatalf("projected a row it should refuse: %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("projecting: %v", err)
			}
			if want := b11aCanonicalText(t, c.want); string(got) != want {
				t.Errorf("projection differs\ngot:  %s\nwant: %s", got, want)
			}
		})
	}

	t.Run("the order of edges does not depend on the order the rows list them", func(t *testing.T) {
		a := b11aEdgeRow("type", "blocks", "depends_on_external", "external:otherproj:cap")
		b := b11aEdgeRow("type", "related", "depends_on_issue_id", "aaa-1")
		c := b11aEdgeRow("type", "blocks", "depends_on_issue_id", "bbb-2")
		first, err := b11aProjectBytes(&oracle.View{Issue: b11aIssueRow(), Dependencies: []oracle.Row{a, b, c}})
		if err != nil {
			t.Fatalf("projecting: %v", err)
		}
		second, err := b11aProjectBytes(&oracle.View{Issue: b11aIssueRow(), Dependencies: []oracle.Row{c, b, a}})
		if err != nil {
			t.Fatalf("projecting: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Errorf("the same edges in another order project differently\n%s\n%s", first, second)
		}
	})

	t.Run("an edge's surrogate id is not carried", func(t *testing.T) {
		issue, err := cellsToIssue(&oracle.View{
			Issue:        b11aIssueRow(),
			Dependencies: []oracle.Row{b11aEdgeRow("depends_on_issue_id", "y-1")},
		})
		if err != nil {
			t.Fatalf("projecting: %v", err)
		}
		if len(issue.Dependencies) != 1 || issue.Dependencies[0].ID != "" {
			t.Errorf("dependencies = %+v, want one edge with no id", issue.Dependencies)
		}
	})

	t.Run("an edge's times must parse", func(t *testing.T) {
		_, err := cellsToIssue(&oracle.View{
			Issue:        b11aIssueRow(),
			Dependencies: []oracle.Row{b11aEdgeRow("depends_on_issue_id", "y-1", "created_at", "not a time")},
		})
		if err == nil {
			t.Error("projected an edge whose created_at is not a time")
		}
	})

	t.Run("an integer a float cannot hold in metadata is carried and refused by the gate", func(t *testing.T) {
		const meta = `{"big": 12345678901234567890}`
		issue, err := cellsToIssue(&oracle.View{Issue: b11aIssueRow("metadata", meta)})
		if err != nil {
			t.Fatalf("projecting: %v", err)
		}
		if string(issue.Metadata) != meta {
			t.Errorf("metadata = %q, want it carried as written, %q", issue.Metadata, meta)
		}
		doc, err := json.Marshal(issue)
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		if _, err := issueops.ReplayCanonicalDurableState(doc); err == nil {
			t.Error("the number gate accepted an integer it cannot represent")
		}
	})

	t.Run("a nil view is refused", func(t *testing.T) {
		if _, err := cellsToIssue(nil); err == nil {
			t.Error("projected a view that is not there")
		}
		if _, err := Project(nil); err == nil {
			t.Error("Project accepted a view that is not there")
		}
	})

	issueKeys := b11aTypeKeys(t, reflect.TypeOf(types.Issue{}))
	depKeys := b11aTypeKeys(t, reflect.TypeOf(types.Dependency{}))

	t.Run("LossyListIsTheOmitemptyValueFields", func(t *testing.T) {
		want := map[string]bool{}
		for _, f := range IssueFields() {
			k := b11aKeyNamed(t, issueKeys, f.Key)
			if f.Class == ClassCompared && len(f.Columns) > 0 && k.omitEmpty && k.kind != reflect.Ptr {
				want[f.Key] = true
			}
		}
		for _, f := range DependencyFields() {
			k := b11aKeyNamed(t, depKeys, f.Key)
			if f.Class == ClassCompared && len(f.Columns) > 0 && k.omitEmpty && k.kind != reflect.Ptr {
				want["dependencies."+f.Key] = true
			}
		}
		got := map[string]bool{}
		for _, l := range Lossy() {
			if got[l.Key] {
				t.Errorf("%q is listed twice", l.Key)
			}
			got[l.Key] = true
			if l.Tag != "omitempty" {
				t.Errorf("%q has tag %q, want omitempty", l.Key, l.Tag)
			}
		}
		for _, key := range b11aSortedSet(want) {
			if !got[key] {
				t.Errorf("%q cannot tell NULL from its zero value, and Lossy does not list it", key)
			}
		}
		for _, key := range b11aSortedSet(got) {
			if !want[key] {
				t.Errorf("Lossy lists %q, which can tell NULL from its zero value", key)
			}
		}

		for _, key := range []string{"description", "metadata", "waiters", "timeout", "ephemeral", "dependencies.thread_id", "status", "close_reason"} {
			if !got[key] {
				t.Errorf("%q must be on the lossy list", key)
			}
		}
		for _, key := range []string{"priority", "title", "id", "external_ref", "estimated_minutes", "due_at", "dependencies", "created_at", "updated_at"} {
			if got[key] {
				t.Errorf("%q must not be on the lossy list: its zero value is distinguishable from NULL", key)
			}
		}
	})

	t.Run("LossyFieldsCannotTellNullFromZero", func(t *testing.T) {
		for _, l := range Lossy() {
			t.Run(l.Key, func(t *testing.T) {
				if sub, isDep := strings.CutPrefix(l.Key, "dependencies."); isDep {
					column := b11aOnlyColumn(t, DependencyFields(), sub)
					if k := b11aKeyNamed(t, depKeys, sub); k.kind != reflect.String {
						t.Fatalf("this test knows the zero of a string edge field, not of %s", k.kind)
					}
					project := func(cell any) []byte {
						doc, err := b11aProjectBytes(&oracle.View{
							Issue:        b11aIssueRow(),
							Dependencies: []oracle.Row{b11aEdgeRow("depends_on_issue_id", "y-1", column, cell)},
						})
						if err != nil {
							t.Fatalf("projecting an edge with %s %v: %v", column, cell, err)
						}
						return doc
					}
					null := project(nil)
					if zero := project(""); !bytes.Equal(null, zero) {
						t.Errorf("an edge with %s NULL and one with it empty project differently\n%s\n%s", column, null, zero)
					}
					if set := project("v"); bytes.Equal(null, set) {
						t.Errorf("the projection ignores %s: a set value changed nothing", column)
					}
					return
				}

				column := b11aOnlyColumn(t, IssueFields(), l.Key)
				k := b11aKeyNamed(t, issueKeys, l.Key)
				var zeros []string
				var set string
				switch {
				case l.Key == "metadata":
					zeros, set = []string{"", "{}"}, `{"k":"v"}`
				case l.Key == "waiters":
					zeros, set = []string{"", "[]"}, `["w"]`
				case k.kind == reflect.String:
					zeros, set = []string{""}, "v"
				case k.kind == reflect.Int || k.kind == reflect.Int64:
					zeros, set = []string{"0"}, "1"
				case k.kind == reflect.Bool:
					zeros, set = []string{"0"}, "1"
				default:
					t.Fatalf("this test does not know the zero of a %s field", k.kind)
				}
				project := func(cell any) []byte {
					doc, err := b11aProjectBytes(&oracle.View{Issue: b11aIssueRow(column, cell)})
					if err != nil {
						t.Fatalf("projecting %s %v: %v", column, cell, err)
					}
					return doc
				}
				null := project(nil)
				for _, zero := range zeros {
					if got := project(zero); !bytes.Equal(null, got) {
						t.Errorf("%s NULL and %s %q project differently\n%s\n%s", column, column, zero, null, got)
					}
				}
				if got := project(set); bytes.Equal(null, got) {
					t.Errorf("the projection ignores %s: a set value changed nothing", column)
				}
			})
		}
	})
}

// b11aOnlyColumn returns the one column that backs a key of a drift table.
func b11aOnlyColumn(t *testing.T, fields []Field, key string) string {
	t.Helper()
	for _, f := range fields {
		if f.Key == key {
			if len(f.Columns) != 1 {
				t.Fatalf("%q is backed by columns %v, want exactly one", key, f.Columns)
			}
			return f.Columns[0]
		}
	}
	t.Fatalf("the drift table has no entry for %q", key)
	return ""
}

func b11aSortedSet(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
