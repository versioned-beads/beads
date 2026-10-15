//go:build cgo

package graphstore

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

func TestGraphIssueListAssigneeAdmission(t *testing.T) {
	for _, request := range []publicops.ListRequest{
		{Assignee: " Alice "}, {NoAssignee: true}, {Assignee: "Alice", NoAssignee: true},
		{Assignee: ""}, {NoAssignee: false}, {Assignee: strings.Repeat("a", PreviewIssueListRequestByteLimit)},
	} {
		got, err := prepareIssueListRequest(request)
		if err != nil || !reflect.DeepEqual(got, request) {
			t.Fatalf("assignee request altered/refused: %+v %v", got, err)
		}
	}
	var store *Store
	for _, tc := range []struct {
		name    string
		request publicops.ListRequest
		want    error
	}{
		{"invalid UTF8", publicops.ListRequest{Assignee: string([]byte{255})}, storage.ErrValidation},
		{"oversized", publicops.ListRequest{Assignee: strings.Repeat("a", PreviewIssueListRequestByteLimit+1)}, storage.ErrValidation},
		{"aggregate title", publicops.ListRequest{Assignee: strings.Repeat("a", PreviewIssueListRequestByteLimit), TitleContains: "x"}, storage.ErrValidation},
		{"aggregate labels", publicops.ListRequest{Assignee: strings.Repeat("a", PreviewIssueListRequestByteLimit), Labels: []string{"x"}}, storage.ErrValidation},
		{"unsupported field", publicops.ListRequest{Assignee: "Alice", Offset: 1}, ErrCapabilityUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.ListIssues(context.Background(), tc.request)
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(got, IssueListPage{}) {
				t.Fatalf("pre-storage refusal: %+v %v", got, err)
			}
		})
	}
}

func TestGraphIssueListAssigneeFilters(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, _, s, source, target, dependency := reopenFixture(t, backend)
			assigned, err := s.UpdateIssue(ctx, UpdateIssueRequest{Path: "beads/work", Actor: "fixture", ExpectedRevision: source.Revision, Assignee: issueEditString("Alice")})
			if err != nil {
				t.Fatal(err)
			}
			records := map[string]IssueRecord{"beads/work": assigned.Issue, "beads/prereq": target}
			saved := []IssueRecord{source, assigned.Issue}
			for _, entry := range []struct {
				path, title, assignee string
				closed                bool
			}{
				{"beads/lower", "Lowercase owner", "alice", false},
				{"beads/accent", "Accented owner", "Alíce", false},
				{"beads/spaced", "Padded owner", " Alice ", false},
				{"beads/duplicate", "Second Alice", "Alice", false},
				{"beads/unassigned", "Never assigned", "", false},
				{"beads/cleared", "Explicitly cleared", "previous", false},
				{"beads/closed", "Closed assigned", "Alice", true},
				{"beads/closed-empty", "Closed unassigned", "", true},
				{"beads/identity", "Dotted identity", "rig.agent", false},
				{"beads/literal", "Literal punctuation", "wild%,_", false},
			} {
				request := plainIssue(entry.title)
				if entry.path == "beads/duplicate" {
					request.Issue.Priority = 0
					request.Issue.Labels = []string{"selected"}
				}
				record, err := s.CreateIssue(ctx, entry.path, request)
				if err != nil {
					t.Fatal(err)
				}
				if entry.assignee != "" {
					result, err := s.UpdateIssue(ctx, UpdateIssueRequest{Path: entry.path, Actor: "fixture", ExpectedRevision: record.Revision, Assignee: issueEditString(entry.assignee)})
					if err != nil {
						t.Fatal(err)
					}
					record = result.Issue
				}
				if entry.path == "beads/cleared" {
					before := record
					result, err := s.UpdateIssue(ctx, UpdateIssueRequest{Path: entry.path, Actor: "fixture", ExpectedRevision: record.Revision, Assignee: issueEditString("")})
					if err != nil {
						t.Fatal(err)
					}
					record = result.Issue
					assertIssueListRetained(t, ctx, s, entry.path, before)
				}
				if entry.closed {
					// Since #7425 only an assigned issue's assignee may close it; anyone
					// may close an unassigned one.
					closer := entry.assignee
					if closer == "" {
						closer = "fixture"
					}
					result, err := s.CloseIssue(ctx, entry.path, "complete", closer)
					if err != nil {
						t.Fatal(err)
					}
					record = result.Issue
				}
				records[entry.path] = record
			}
			memory, err := s.Create(ctx, CreateRequest{Path: "beads/memory", Body: "Excluded from Issue listing"})
			if err != nil {
				t.Fatal(err)
			}
			var collation string
			if err := s.db.QueryRowContext(ctx, "SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='issues' AND COLUMN_NAME='assignee'").Scan(&collation); err != nil {
				t.Fatal(err)
			}
			t.Logf("Inherited Issue assignee SQL collation: %s", collation)
			before := reopenState(t, ctx, s)
			// Equality remains owned by the installed SQL collation. Read an independent
			// predicate over API-authored rows rather than hard-coding new byte/identity
			// normalization rules in this graph adapter's tests.
			sqlMatches := func(selector string) map[string]bool {
				t.Helper()
				rows, err := s.db.QueryContext(ctx, "SELECT id FROM issues WHERE assignee = ?", selector)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := rows.Close(); err != nil {
						t.Error(err)
					}
				}()
				out := make(map[string]bool)
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					out[id] = true
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				return out
			}
			assertPage := func(request publicops.ListRequest, wanted []IssueRecord) IssueListPage {
				t.Helper()
				got, err := s.ListIssues(ctx, request)
				if err != nil || got.HasMore || got.Items == nil || len(got.Items) != len(wanted) {
					t.Fatalf("list %+v => %+v want%d err%v", request, got, len(wanted), err)
				}
				wantByID := make(map[string]IssueRecord)
				for _, row := range wanted {
					wantByID[row.ID] = row
				}
				for _, row := range got.Items {
					want, ok := wantByID[row.ID]
					if !ok || !reflect.DeepEqual(row, want) {
						t.Fatalf("incomplete/unexpected listed record: %+v", row)
					}
					delete(wantByID, row.ID)
				}
				if len(wantByID) != 0 {
					t.Fatal("listing omitted expected records")
				}
				return got
			}
			wantRows := func(matches map[string]bool, all, unassigned bool) []IssueRecord {
				out := []IssueRecord{}
				for _, row := range records {
					if !all && row.Properties.Status == types.StatusClosed {
						continue
					}
					if matches != nil && !matches[row.Properties.ID] {
						continue
					}
					if unassigned && row.Properties.Assignee != "" {
						continue
					}
					out = append(out, row)
				}
				return out
			}
			for _, selector := range []string{"Alice", "alice", "Alíce", "ALICE", "ALÍCE", " Alice ", "Alice ", " Alice", "rig.agent", "rig_agent", "wild%,_", "wild%", "missing", strings.Repeat("x", types.MaxFieldLen+1)} {
				name := "equality " + selector
				if len(selector) > types.MaxFieldLen {
					name = "equality overlength query"
				}
				t.Run(name, func(t *testing.T) {
					assertPage(publicops.ListRequest{Assignee: selector}, wantRows(sqlMatches(selector), false, false))
				})
			}
			for _, tc := range []struct {
				name            string
				request         publicops.ListRequest
				all, unassigned bool
			}{
				{"empty selector", publicops.ListRequest{Assignee: ""}, false, false},
				{"false no-assignee", publicops.ListRequest{NoAssignee: false}, false, false},
				{"unassigned", publicops.ListRequest{NoAssignee: true}, false, true},
				{"all unassigned", publicops.ListRequest{NoAssignee: true, AllFlag: true}, true, true},
				{"all", publicops.ListRequest{AllFlag: true}, true, false},
			} {
				t.Run(tc.name, func(t *testing.T) { assertPage(tc.request, wantRows(nil, tc.all, tc.unassigned)) })
			}
			t.Run("conflicting intersection", func(t *testing.T) {
				assertPage(publicops.ListRequest{Assignee: "Alice", NoAssignee: true, AllFlag: true}, nil)
			})
			t.Run("all assigned", func(t *testing.T) {
				assertPage(publicops.ListRequest{Assignee: "Alice", AllFlag: true}, wantRows(sqlMatches("Alice"), true, false))
			})
			t.Run("closed assigned", func(t *testing.T) {
				assertPage(publicops.ListRequest{Assignee: "Alice", Status: "closed"}, []IssueRecord{records["beads/closed"]})
			})
			for _, tc := range []struct {
				name    string
				request publicops.ListRequest
			}{
				{"priority", publicops.ListRequest{Assignee: "Alice", Priority: issueListInt(0)}},
				{"label", publicops.ListRequest{Assignee: "Alice", Labels: []string{"selected"}}},
				{"title", publicops.ListRequest{Assignee: "Alice", TitleContains: "Second"}},
			} {
				t.Run(tc.name, func(t *testing.T) { assertPage(tc.request, []IssueRecord{records["beads/duplicate"]}) })
			}
			t.Run("page limits", func(t *testing.T) {
				unlimited := assertPage(publicops.ListRequest{Assignee: "Alice", SortBy: "title", AllFlag: true, Limit: issueListInt(0)}, wantRows(sqlMatches("Alice"), true, false))
				got, err := s.ListIssues(ctx, publicops.ListRequest{Assignee: "Alice", SortBy: "title", AllFlag: true, Limit: issueListInt(1)})
				if err != nil || !got.HasMore || len(got.Items) != 1 || !reflect.DeepEqual(got.Items[0], unlimited.Items[0]) {
					t.Fatalf("limited page: %+v %v", got, err)
				}
			})
			for path, row := range records {
				current, err := s.ShowIssue(ctx, path)
				if err != nil || !reflect.DeepEqual(current, row) {
					t.Fatalf("list changed %s: %+v %v", path, current, err)
				}
				assertIssueListRetained(t, ctx, s, path, row)
			}
			for _, row := range saved {
				assertIssueListRetained(t, ctx, s, "beads/work", row)
			}
			if got, err := s.ShowLink(ctx, "links/block"); err != nil || !reflect.DeepEqual(got, dependency) {
				t.Fatalf("listing changed Dependency: %+v %v", got, err)
			}
			if got, err := s.Show(ctx, "beads/memory"); err != nil || !reflect.DeepEqual(got, memory) {
				t.Fatalf("listing changed Memory: %+v %v", got, err)
			}
			// The selected current record still contains the complete owned Link and is
			// not a legacy Issue projection with private/native identity substituted.
			owned := assertPage(publicops.ListRequest{Assignee: "Alice", TitleContains: "Work"}, []IssueRecord{assigned.Issue})
			if len(owned.Items[0].Owned) != 1 || !slices.Equal(owned.Items[0].Properties.Labels, assigned.Issue.Properties.Labels) {
				t.Fatal("owned/label projection incomplete")
			}
			if !reflect.DeepEqual(before, reopenState(t, ctx, s)) {
				t.Fatal("read-only list/filter changed stored state")
			}
		})
	}
}

// The closes above pass the assignee fence rather than go around it: a close by
// an actor other than the assignee is refused with the inherited ownership error
// and leaves the stored state alone. Only the error class is pinned, not its text.
func TestGraphIssueCloseRefusesAnotherActor(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, _, s, _, _, _ := reopenFixture(t, backend)
			created, err := s.CreateIssue(ctx, "beads/fenced", plainIssue("Fenced"))
			if err != nil {
				t.Fatal(err)
			}
			assigned, err := s.UpdateIssue(ctx, UpdateIssueRequest{Path: "beads/fenced", Actor: "fixture", ExpectedRevision: created.Revision, Assignee: issueEditString("Alice")})
			if err != nil {
				t.Fatal(err)
			}
			before := reopenState(t, ctx, s)
			if _, err := s.CloseIssue(ctx, "beads/fenced", "complete", "fixture"); !errors.Is(err, storage.ErrNotOwner) {
				t.Fatalf("close by another actor: %v, want %v", err, storage.ErrNotOwner)
			}
			if current, err := s.ShowIssue(ctx, "beads/fenced"); err != nil || !reflect.DeepEqual(current, assigned.Issue) {
				t.Fatalf("a refused close changed the Issue: %+v %v", current, err)
			}
			if !reflect.DeepEqual(before, reopenState(t, ctx, s)) {
				t.Fatal("a refused close changed stored state")
			}
			closed, err := s.CloseIssue(ctx, "beads/fenced", "complete", "Alice")
			if err != nil || closed.Issue.Properties.Status != types.StatusClosed {
				t.Fatalf("close by the assignee: %+v %v", closed, err)
			}
		})
	}
}
