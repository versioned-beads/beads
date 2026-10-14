//go:build cgo

package graphstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	publicops "github.com/steveyegge/beads/issueops"
)

func createFieldsStore(t *testing.T, backend string) (context.Context, *Store) {
	t.Helper()
	ctx, options := issueExperimentOptions(t, backend)
	s, err := OpenExisting(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return ctx, s
}

func createFieldsRequest(title string) publicops.CreateRequest {
	request := plainIssue(title)
	request.Issue.Design = "  Design — 雪\r\n-  "
	request.Issue.AcceptanceCriteria = "  Accept café e\u0301\t\r\n"
	request.Issue.Assignee = "  crew.É 雪  "
	request.Issue.EstimatedMinutes = issuePriority(45)
	request.Issue.ExternalRef = issueEditString("  external — 雪  ")
	request.Issue.SpecID = "  spec — café\r\n"
	return request
}

func assertCreateFields(t *testing.T, got IssueRecord, request publicops.CreateRequest) {
	t.Helper()
	want := request.Issue
	if got.Properties == nil || got.ID == "" || got.Type == "" || got.Version == "" ||
		got.Version != got.Revision || got.Attribution.Actor != request.Actor || got.Owned == nil || len(got.Owned) != 0 {
		t.Fatal("initial record lacks complete identity, attribution or explicit empty ownership")
	}
	p := got.Properties
	if p.Design != want.Design || p.AcceptanceCriteria != want.AcceptanceCriteria || p.Assignee != want.Assignee ||
		!reflect.DeepEqual(p.EstimatedMinutes, want.EstimatedMinutes) || !reflect.DeepEqual(p.ExternalRef, want.ExternalRef) ||
		p.SpecID != want.SpecID || p.Title != want.Title || p.Description != want.Description || p.Priority != want.Priority ||
		p.Status != want.Status || p.IssueType != want.IssueType || !reflect.DeepEqual(p.Labels, want.Labels) {
		t.Fatal("initial record lost literal fields, nullable presence or existing Issue values")
	}
	if p.StartedAt != nil || p.LeaseExpiresAt != nil || p.HeartbeatAt != nil || p.LeaseGrantedNode != "" {
		t.Fatal("initial assignee unexpectedly started or leased the Issue")
	}
}

// Existing workflow fingerprints include mappings, snapshots, events and lease
// state. Create also authors labels, so observe their actual rows explicitly.
func createFieldsState(t *testing.T, ctx context.Context, s *Store) map[string]string {
	t.Helper()
	state := reopenState(t, ctx, s)
	rows, err := s.db.QueryContext(ctx, "SELECT issue_id,label FROM labels ORDER BY issue_id,label")
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		encoded, err := json.Marshal([]string{id, label})
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		labels = append(labels, string(encoded))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	state["labels"] = strings.Join(labels, "\n")
	return state
}

func TestIssueCreateFieldsLifecycle(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, s := createFieldsStore(t, backend)
			request := createFieldsRequest("Authored in first version")
			first, err := s.CreateIssue(ctx, "beads/work", request)
			if err != nil {
				t.Fatal(err)
			}
			assertCreateFields(t, first, request)
			if request.Issue.ID != "" {
				t.Fatal("create mutated caller identity")
			}
			assertIssueEditCounts(t, ctx, s, first.Properties.ID, 1)
			assertIssueEditVersion(t, ctx, s, "beads/work", first)
			assertAssigneeLeaseCount(t, ctx, s, first.Properties.ID, 0)
			for _, event := range []struct {
				kind string
				want int
			}{{"created", 1}, {"updated", 0}} {
				var count int
				if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE issue_id=? AND event_type=?", first.Properties.ID, event.kind).Scan(&count); err != nil || count != event.want {
					t.Fatalf("%s events=%d want%d: %v", event.kind, count, event.want, err)
				}
			}
			for _, table := range []string{"graph_preview_payloads", "graph_preview_versions"} {
				var count int
				if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("create stored an extra generic Issue copy in %s: %d %v", table, count, err)
				}
			}
			if current, err := s.Read(ctx, "beads/work"); err != nil || !reflect.DeepEqual(current, first) {
				t.Fatalf("initial complete current read differs: %v", err)
			}
			target, err := s.CreateIssue(ctx, "beads/prereq", plainIssue("Prerequisite"))
			if err != nil {
				t.Fatal(err)
			}
			memory, err := s.Create(ctx, CreateRequest{Path: "beads/context", Title: "Context", Body: "Unchanged rationale"})
			if err != nil {
				t.Fatal(err)
			}
			dependency, err := s.AddDependency(ctx, DependencyRequest{Path: "links/block", SourcePath: "beads/work", TargetPath: "beads/prereq", Actor: "linker"})
			if err != nil {
				t.Fatal(err)
			}
			assertDependencyOwned(t, dependency.Source, dependency.Link)
			assertDependencyReadiness(t, ctx, s, dependency.Source, false)
			// is_blocked is a persisted readiness projection, not part of the
			// hydrated Issue properties at this pinned base. Check SQL/readiness
			// through the existing helper, and preserve all hydrated properties.
			if !reflect.DeepEqual(dependency.Source.Properties, first.Properties) {
				t.Fatal("adding a Dependency changed initial properties")
			}
			info, err := s.AddInformationalLink(ctx, LinkCreateRequest{Path: "links/context", SourcePath: "beads/context", TargetPath: "beads/work", Actor: "linker", ExpectedSourceRevision: memory.Revision})
			if err != nil {
				t.Fatal(err)
			}
			updated, err := s.UpdateIssue(ctx, UpdateIssueRequest{Path: "beads/work", Actor: "editor", ExpectedRevision: dependency.Source.Revision, Title: issueEditString("Later title")})
			if err != nil || !updated.Changed {
				t.Fatalf("later edit: %v", err)
			}
			assertIssueReferencesTransition(t, dependency.Source, updated.Issue, first.Properties.ExternalRef, first.Properties.SpecID, "Later title", "editor")
			assertIssueEditCounts(t, ctx, s, first.Properties.ID, 3)
			for _, record := range []IssueRecord{first, dependency.Source, updated.Issue} {
				assertIssueEditVersion(t, ctx, s, "beads/work", record)
			}
			for path, want := range map[string]any{"beads/work": updated.Issue, "beads/prereq": target, "beads/context": info.Source, "links/block": dependency.Link, "links/context": info.Link} {
				if got, err := s.Read(ctx, path); err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("later edit changed complete %s: %v", path, err)
				}
			}
		})
	}
}

func TestIssueCreateFieldsPresenceAndBounds(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, s := createFieldsStore(t, backend)
			for index, tc := range []struct {
				name string
				set  func(*publicops.Issue)
			}{
				{"nil", func(i *publicops.Issue) {}},
				{"empty-pointer-zero", func(i *publicops.Issue) { i.ExternalRef, i.EstimatedMinutes = issueEditString(""), issuePriority(0) }},
				{"positive-ascii-bound", func(i *publicops.Issue) {
					i.Assignee, i.ExternalRef, i.SpecID = strings.Repeat("a", 255), issueEditString(strings.Repeat("e", 255)), strings.Repeat("s", 1024)
					i.EstimatedMinutes, i.Design, i.AcceptanceCriteria = issuePriority(7), "-", ""
				}},
				{"unicode-bound-max", func(i *publicops.Issue) {
					i.Assignee, i.ExternalRef, i.SpecID = strings.Repeat("😀", 255), issueEditString(strings.Repeat("雪", 255)), strings.Repeat("😀", 1024)
					i.EstimatedMinutes = issuePriority(math.MaxInt32)
				}},
				{"longtext", func(i *publicops.Issue) {
					i.Design, i.AcceptanceCriteria = strings.Repeat("d", 65<<10), strings.Repeat("雪", 24<<10)
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					request := plainIssue(tc.name)
					tc.set(request.Issue)
					path := fmt.Sprintf("beads/presence-%d", index)
					got, err := s.CreateIssue(ctx, path, request)
					if err != nil {
						t.Fatal(err)
					}
					assertCreateFields(t, got, request)
					assertIssueEditCounts(t, ctx, s, got.Properties.ID, 1)
					assertIssueEditVersion(t, ctx, s, path, got)
					if current, err := s.Read(ctx, path); err != nil || !reflect.DeepEqual(current, got) {
						t.Fatalf("complete current values differ: %v", err)
					}
					var externalNull, estimateNull bool
					if err := s.db.QueryRowContext(ctx, "SELECT external_ref IS NULL,estimated_minutes IS NULL FROM issues WHERE id=?", got.Properties.ID).Scan(&externalNull, &estimateNull); err != nil ||
						externalNull != (request.Issue.ExternalRef == nil) || estimateNull != (request.Issue.EstimatedMinutes == nil) {
						t.Fatalf("nullable API presence lost: externalNull=%v estimateNull=%v err=%v", externalNull, estimateNull, err)
					}
					raw, err := json.Marshal(got.Properties)
					if err != nil {
						t.Fatal(err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw, &fields); err != nil {
						t.Fatal(err)
					}
					if _, present := fields["external_ref"]; present != (request.Issue.ExternalRef != nil) {
						t.Fatal("JSON erased the Go API's nil versus nonnil-empty external reference")
					}
					if _, present := fields["estimated_minutes"]; present != (request.Issue.EstimatedMinutes != nil) {
						t.Fatal("JSON erased the nil versus present-zero estimate")
					}
				})
			}
		})
	}
}

func TestIssueCreateFieldsCopiesInput(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, s := createFieldsStore(t, backend)
			request := createFieldsRequest("Captured intent")
			expected := issueops.CloneCreateRequest(request)
			touched := false
			s.afterWrite = func(stage string) error {
				if stage == "coordination" {
					touched = true
					*request.Issue.EstimatedMinutes = 900
					*request.Issue.ExternalRef = "mutated caller"
					request.Issue.Design, request.Issue.Labels[0] = "mutated design", "mutated-label"
				}
				return nil
			}
			got, err := s.CreateIssue(ctx, "beads/copied", request)
			s.afterWrite = nil
			if err != nil || !touched {
				t.Fatalf("copied-input control did not execute: touched=%t err=%v", touched, err)
			}
			assertCreateFields(t, got, expected)
			assertIssueEditCounts(t, ctx, s, got.Properties.ID, 1)
			assertIssueEditVersion(t, ctx, s, "beads/copied", got)
		})
	}
}

func TestIssueCreateFieldsRefusalAndRollback(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, s := createFieldsStore(t, backend)
			state := createFieldsState(t, ctx, s)
			cases := []struct {
				name string
				set  func(*publicops.Issue)
			}{
				{"negative-estimate", func(i *publicops.Issue) { i.EstimatedMinutes = issuePriority(-1) }},
				{"assignee-overlong", func(i *publicops.Issue) { i.Assignee = strings.Repeat("😀", 256) }},
				{"external-overlong", func(i *publicops.Issue) { i.ExternalRef = issueEditString(strings.Repeat("雪", 256)) }},
				{"spec-overlong", func(i *publicops.Issue) { i.SpecID = strings.Repeat("😀", 1025) }},
				{"design-utf8", func(i *publicops.Issue) { i.Design = "\xff" }},
				{"acceptance-utf8", func(i *publicops.Issue) { i.AcceptanceCriteria = "\xff" }},
				{"assignee-utf8", func(i *publicops.Issue) { i.Assignee = "\xff" }},
				{"external-utf8", func(i *publicops.Issue) { i.ExternalRef = issueEditString("\xff") }},
				{"spec-utf8", func(i *publicops.Issue) { i.SpecID = "\xff" }},
			}
			wide := int64(math.MaxInt32) + 1
			if int64(int(wide)) == wide {
				cases = append(cases, struct {
					name string
					set  func(*publicops.Issue)
				}{"overflow-estimate", func(i *publicops.Issue) { i.EstimatedMinutes = issuePriority(int(wide)) }})
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					request := createFieldsRequest("Must not exist")
					tc.set(request.Issue)
					got, err := s.CreateIssue(ctx, "beads/refused", request)
					if !errors.Is(err, storage.ErrValidation) || !reflect.DeepEqual(got, IssueRecord{}) || !reflect.DeepEqual(state, createFieldsState(t, ctx, s)) {
						t.Fatalf("invalid create returned success or leaked state: err=%v", err)
					}
				})
			}
			for _, stage := range []string{"coordination", "issue", "issue-catalog", "retained"} {
				t.Run("rollback-"+stage, func(t *testing.T) {
					fault := errors.New("injected create callback failure")
					s.afterWrite = func(at string) error {
						if at == stage {
							return fault
						}
						return nil
					}
					got, err := s.CreateIssue(ctx, "beads/rollback", createFieldsRequest("Must roll back"))
					s.afterWrite = nil
					if !errors.Is(err, fault) || !reflect.DeepEqual(got, IssueRecord{}) || !reflect.DeepEqual(state, createFieldsState(t, ctx, s)) {
						t.Fatalf("callback rollback leaked field/label/snapshot state: %v", err)
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
				got, err := s.CreateIssue(canceled, "beads/canceled", createFieldsRequest("Canceled"))
				s.afterWrite = nil
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, IssueRecord{}) || !reflect.DeepEqual(state, createFieldsState(t, ctx, s)) {
					t.Fatalf("late callback cancellation leaked state: %v", err)
				}
			})
		})
	}
}

// Storage may keep the creator a request implies, and nothing else. The table
// drives the whole exactness guard over otherwise identical Issues, so it also
// fails if the creator comparison is deleted from the guard.
func TestCreateFieldsCreatorExactness(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		requestCreator, actor, stored string
		accepted                      bool
	}{
		{"defaulted-to-actor", "", "a", "a", true},
		{"default-dropped", "", "a", "", false},
		{"default-replaced", "", "a", "b", false},
		{"explicit-overwritten-by-actor", "x", "a", "a", false},
		{"explicit-kept", "x", "a", "x", true},
		{"explicit-dropped", "x", "a", "", false},
		{"no-creator-no-actor-stays-empty", "", "", "", true},
		{"no-creator-no-actor-invented", "", "", "b", false},
		{"actor-kept-verbatim", "", "  a ", "  a ", true},
		{"actor-not-trimmed", "", "  a ", "a", false},
		{"actor-not-case-folded", "", "Alice", "alice", false},
		{"actor-not-canonicalized", "", "gastown.mayor", "gastown_mayor", false},
		{"explicit-kept-verbatim", "  x ", "a", "  x ", true},
		{"explicit-not-trimmed", "  x ", "a", "x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := &publicops.Issue{Title: "Creator", CreatedBy: tc.requestCreator}
			got := &publicops.Issue{Title: "Creator", CreatedBy: tc.stored}
			if accepted := sameIssueCreateFields(want, got, tc.actor); accepted != tc.accepted {
				t.Fatalf("creator %q actor %q stored %q: accepted=%t want %t", tc.requestCreator, tc.actor, tc.stored, accepted, tc.accepted)
			}
			if want.CreatedBy != tc.requestCreator {
				t.Fatal("the guard rewrote the caller's creator")
			}
		})
	}
	// Only the creator is relaxed. Storage has no Owner default, and changed
	// notes are still a coercion, even when the creator is the defaulted actor.
	for _, tc := range []struct {
		name   string
		change func(*publicops.Issue)
	}{
		{"owner", func(i *publicops.Issue) { i.Owner = "a" }},
		{"notes", func(i *publicops.Issue) { i.Notes = "changed" }},
	} {
		t.Run("defaulted-creator-keeps-"+tc.name+"-exact", func(t *testing.T) {
			want := &publicops.Issue{Title: "Creator"}
			got := &publicops.Issue{Title: "Creator", CreatedBy: "a"}
			if !sameIssueCreateFields(want, got, "a") {
				t.Fatal("control: the defaulted creator alone was refused")
			}
			tc.change(got)
			if sameIssueCreateFields(want, got, "a") {
				t.Fatalf("a defaulted creator also admitted a changed %s", tc.name)
			}
		})
	}
}
