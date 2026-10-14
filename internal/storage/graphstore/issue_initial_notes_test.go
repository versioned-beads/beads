//go:build cgo

package graphstore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	publicops "github.com/steveyegge/beads/issueops"
)

func initialNotesRequest(title string) publicops.CreateRequest {
	request := createFieldsRequest(title)
	request.Actor = "audit-author"
	request.Issue.Assignee = "worker"
	request.Issue.Notes = "  Initial — 雪\r\n\t-  "
	request.Issue.Owner = "  owner.É@example.test  "
	request.Issue.CreatedBy = "  imported creator — 雪  "
	return request
}

// wantCreator is the creator a create must store. An empty creator is no longer
// a literal since c4e667cb1 (#7471): the request actor becomes the creator. It
// is spelled out here, apart from the production guard, so this expectation does
// not validate that guard with itself.
func wantCreator(request publicops.CreateRequest) string {
	if request.Issue.CreatedBy != "" {
		return request.Issue.CreatedBy
	}
	return request.Actor
}

func assertInitialNotes(t *testing.T, got IssueRecord, request publicops.CreateRequest) {
	t.Helper()
	assertCreateFields(t, got, request)
	if got.Properties.Notes != request.Issue.Notes || got.Properties.Owner != request.Issue.Owner || got.Properties.CreatedBy != wantCreator(request) {
		t.Fatal("initial notes/owner/creator were defaulted, normalized or lost")
	}
}

func TestIssueInitialNotesLifecycle(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, s := createFieldsStore(t, backend)
			request := initialNotesRequest("Initial authored context")
			first, err := s.CreateIssue(ctx, "beads/work", request)
			if err != nil {
				t.Fatal(err)
			}
			assertInitialNotes(t, first, request)
			assertIssueEditCounts(t, ctx, s, first.Properties.ID, 1)
			assertIssueEditVersion(t, ctx, s, "beads/work", first)
			assertAssigneeLeaseCount(t, ctx, s, first.Properties.ID, 0)
			// Labels may emit their ordinary audit events; initialization must not
			// manufacture an update or an extra retained Issue version.
			for _, event := range []struct {
				kind string
				want int
			}{{"created", 1}, {"updated", 0}} {
				var count int
				if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE issue_id=? AND event_type=?", first.Properties.ID, event.kind).Scan(&count); err != nil || count != event.want {
					t.Fatalf("initial %s events=%d want%d: %v", event.kind, count, event.want, err)
				}
			}
			target, err := s.CreateIssue(ctx, "beads/prereq", plainIssue("Prerequisite"))
			if err != nil {
				t.Fatal(err)
			}
			memory, err := s.Create(ctx, CreateRequest{Path: "beads/context", Body: "Unchanged context"})
			if err != nil {
				t.Fatal(err)
			}
			dependency, err := s.AddDependency(ctx, DependencyRequest{Path: "links/block", SourcePath: "beads/work", TargetPath: "beads/prereq", Actor: "linker"})
			if err != nil {
				t.Fatal(err)
			}
			assertDependencyOwned(t, dependency.Source, dependency.Link)
			assertDependencyReadiness(t, ctx, s, dependency.Source, false)
			if !reflect.DeepEqual(dependency.Source.Properties, first.Properties) {
				t.Fatal("Dependency changed initial hydrated properties")
			}
			info, err := s.AddInformationalLink(ctx, LinkCreateRequest{Path: "links/context", SourcePath: "beads/context", TargetPath: "beads/work", Actor: "linker", ExpectedSourceRevision: memory.Revision})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := s.ClaimIssue(ctx, "beads/work", "worker")
			if err != nil || !claimed.Changed {
				t.Fatalf("claim initially assigned Issue: changed=%t err=%v", claimed.Changed, err)
			}
			assertClaimRecord(t, dependency.Source, claimed.Issue, "worker")
			assertClaimLease(t, ctx, s, first.Properties.ID, "worker")
			appended, err := s.UpdateIssue(ctx, UpdateIssueRequest{Path: "beads/work", Actor: "worker", ExpectedRevision: claimed.Issue.Revision, AppendNotes: issueEditString("Progress — café\r\n")})
			if err != nil || !appended.Changed {
				t.Fatalf("append initial notes: changed=%t err=%v", appended.Changed, err)
			}
			assertIssueAppendTransition(t, claimed.Issue, appended.Issue, request.Issue.Notes+"\nProgress — café\r\n", "worker")
			assertIssueEditCounts(t, ctx, s, first.Properties.ID, 4)
			assertClaimLease(t, ctx, s, first.Properties.ID, "worker")
			for _, record := range []IssueRecord{first, dependency.Source, claimed.Issue, appended.Issue} {
				assertIssueEditVersion(t, ctx, s, "beads/work", record)
				if record.Properties.Owner != request.Issue.Owner || record.Properties.CreatedBy != request.Issue.CreatedBy {
					t.Fatal("a later writer rewrote original owner/creator")
				}
			}
			for path, want := range map[string]any{"beads/work": appended.Issue, "beads/prereq": target, "beads/context": info.Source, "links/block": dependency.Link, "links/context": info.Link} {
				if got, err := s.Read(ctx, path); err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("complete current %s changed unexpectedly: %v", path, err)
				}
			}
		})
	}
}

func TestIssueInitialNotesLiteralValues(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, s := createFieldsStore(t, backend)
			for index, tc := range []struct {
				name string
				set  func(*publicops.Issue)
			}{
				{"omitted", func(i *publicops.Issue) {}},
				{"empty", func(i *publicops.Issue) { i.Notes, i.Owner, i.CreatedBy = "", "", "" }},
				{"literal-hyphen", func(i *publicops.Issue) { i.Notes, i.Owner, i.CreatedBy = "-", "  owner  ", "  creator  " }},
				{"ascii-bound", func(i *publicops.Issue) {
					i.Notes, i.Owner, i.CreatedBy = "\r\n  \t", strings.Repeat("o", 255), strings.Repeat("c", 255)
				}},
				{"unicode-bound", func(i *publicops.Issue) {
					i.Notes, i.Owner, i.CreatedBy = "  雪 e\u0301\r\n", strings.Repeat("雪", 255), strings.Repeat("😀", 255)
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					request := plainIssue(tc.name)
					tc.set(request.Issue)
					path := fmt.Sprintf("beads/literal-%d", index)
					got, err := s.CreateIssue(ctx, path, request)
					if err != nil {
						t.Fatal(err)
					}
					assertInitialNotes(t, got, request)
					assertIssueEditCounts(t, ctx, s, got.Properties.ID, 1)
					assertIssueEditVersion(t, ctx, s, path, got)
					if current, err := s.Read(ctx, path); err != nil || !reflect.DeepEqual(current, got) {
						t.Fatalf("literal current record differs: %v", err)
					}
				})
			}
		})
	}
}

func TestIssueInitialNotesCopiesInput(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, s := createFieldsStore(t, backend)
			request := initialNotesRequest("Captured initial intent")
			want := issueops.CloneCreateRequest(request)
			touched := false
			s.afterWrite = func(stage string) error {
				if stage == "coordination" {
					touched = true
					request.Issue.Notes, request.Issue.Owner, request.Issue.CreatedBy = "changed notes", "changed owner", "changed creator"
				}
				return nil
			}
			got, err := s.CreateIssue(ctx, "beads/copied", request)
			s.afterWrite = nil
			if err != nil || !touched {
				t.Fatalf("copy control did not execute: touched=%t err=%v", touched, err)
			}
			assertInitialNotes(t, got, want)
			assertIssueEditCounts(t, ctx, s, got.Properties.ID, 1)
			assertIssueEditVersion(t, ctx, s, "beads/copied", got)
		})
	}
}

func TestIssueInitialNotesRefusalAndRollback(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, s := createFieldsStore(t, backend)
			state := createFieldsState(t, ctx, s)
			for _, tc := range []struct {
				name string
				set  func(*publicops.Issue)
			}{
				{"notes-utf8", func(i *publicops.Issue) { i.Notes = "\xff" }},
				{"owner-utf8", func(i *publicops.Issue) { i.Owner = "\xff" }},
				{"creator-utf8", func(i *publicops.Issue) { i.CreatedBy = "\xff" }},
				{"owner-ascii-overlong", func(i *publicops.Issue) { i.Owner = strings.Repeat("o", 256) }},
				{"owner-unicode-overlong", func(i *publicops.Issue) { i.Owner = strings.Repeat("雪", 256) }},
				{"creator-ascii-overlong", func(i *publicops.Issue) { i.CreatedBy = strings.Repeat("c", 256) }},
				{"creator-unicode-overlong", func(i *publicops.Issue) { i.CreatedBy = strings.Repeat("😀", 256) }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					request := initialNotesRequest("Must not exist")
					tc.set(request.Issue)
					s.afterWrite = func(stage string) error { return fmt.Errorf("invalid input reached %s", stage) }
					got, err := s.CreateIssue(ctx, "beads/refused", request)
					s.afterWrite = nil
					if !errors.Is(err, storage.ErrValidation) || !reflect.DeepEqual(got, IssueRecord{}) || !reflect.DeepEqual(state, createFieldsState(t, ctx, s)) {
						t.Fatalf("invalid initial value did not refuse before writing: %v", err)
					}
				})
			}
			t.Run("actor-utf8", func(t *testing.T) {
				request := initialNotesRequest("Must not exist")
				request.Actor = "\xff"
				s.afterWrite = func(stage string) error { return fmt.Errorf("invalid actor reached %s", stage) }
				got, err := s.CreateIssue(ctx, "beads/refused", request)
				s.afterWrite = nil
				if !errors.Is(err, storage.ErrValidation) || !reflect.DeepEqual(got, IssueRecord{}) || !reflect.DeepEqual(state, createFieldsState(t, ctx, s)) {
					t.Fatalf("invalid independent actor did not refuse before writing: %v", err)
				}
			})
			for _, stage := range []string{"issue", "retained"} {
				t.Run("rollback-"+stage, func(t *testing.T) {
					fault := errors.New("initial-notes callback failure")
					s.afterWrite = func(at string) error {
						if at == stage {
							return fault
						}
						return nil
					}
					got, err := s.CreateIssue(ctx, "beads/rollback", initialNotesRequest("Must roll back"))
					s.afterWrite = nil
					if !errors.Is(err, fault) || !reflect.DeepEqual(got, IssueRecord{}) || !reflect.DeepEqual(state, createFieldsState(t, ctx, s)) {
						t.Fatalf("initial-notes callback leaked state: %v", err)
					}
				})
			}
			t.Run("cancellation", func(t *testing.T) {
				canceled, cancel := context.WithCancel(ctx)
				defer cancel()
				s.afterWrite = func(stage string) error {
					if stage == "retained" {
						cancel()
						return canceled.Err()
					}
					return nil
				}
				got, err := s.CreateIssue(canceled, "beads/canceled", initialNotesRequest("Canceled"))
				s.afterWrite = nil
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, IssueRecord{}) || !reflect.DeepEqual(state, createFieldsState(t, ctx, s)) {
					t.Fatalf("late callback cancellation leaked initial-notes state: %v", err)
				}
			})
		})
	}
}

// One input is modest; it becomes unreadable only when combined with the
// unrelated current Memory and the retained copies acquired by normal reads.
func TestIssueInitialNotesPreservesCurrentReadBudget(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, s := createFieldsStore(t, backend)
			original, err := s.CreateIssue(ctx, "beads/existing", initialNotesRequest("Existing readable Issue"))
			if err != nil {
				t.Fatal(err)
			}
			memory, err := s.Create(ctx, CreateRequest{Path: "beads/large-context", Body: strings.Repeat("m", 7<<20)})
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.CurrentSnapshot(ctx)
			if err != nil {
				t.Fatalf("budget fixture must begin readable: %v", err)
			}
			state := createFieldsState(t, ctx, s)
			request := initialNotesRequest("Must roll back all initial fields")
			request.Issue.Notes = strings.Repeat("n", 1536<<10)
			got, err := s.CreateIssue(ctx, "beads/budgeted", request)
			if !errors.Is(err, ErrLimitExceeded) || !reflect.DeepEqual(got, IssueRecord{}) {
				t.Fatalf("cumulative initial notes must refuse with zero result: %v", err)
			}
			if !reflect.DeepEqual(state, createFieldsState(t, ctx, s)) {
				t.Fatal("read-budget refusal leaked create, labels, audit, mapping or retained state")
			}
			if after, err := s.CurrentSnapshot(ctx); err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("refusal changed or stranded current inventory: %v", err)
			}
			for path, want := range map[string]any{"beads/existing": original, "beads/large-context": memory} {
				if current, err := s.Read(ctx, path); err != nil || !reflect.DeepEqual(current, want) {
					t.Fatalf("refusal changed or stranded %s: %v", path, err)
				}
			}
			assertIssueEditCounts(t, ctx, s, original.Properties.ID, 1)
			assertIssueEditVersion(t, ctx, s, "beads/existing", original)
			request.Issue.Notes = strings.Repeat("s", 64<<10)
			accepted, err := s.CreateIssue(ctx, "beads/budgeted", request)
			if err != nil {
				t.Fatalf("smaller create at refused path must fit: %v", err)
			}
			assertInitialNotes(t, accepted, request)
			assertIssueEditCounts(t, ctx, s, accepted.Properties.ID, 1)
			assertIssueEditVersion(t, ctx, s, "beads/budgeted", accepted)
			if current, err := s.Read(ctx, "beads/budgeted"); err != nil || !reflect.DeepEqual(current, accepted) {
				t.Fatalf("accepted small notes are unreadable: %v", err)
			}
			if _, err := s.CurrentSnapshot(ctx); err != nil {
				t.Fatalf("accepted small notes stranded inventory: %v", err)
			}
		})
	}
}
