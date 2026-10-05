//go:build cgo

package graphstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// This scenario exercises the real domain writers, retained source membership,
// canonical allocation and scheduling query in one normal initialized database.
// The process-level installed CLI boundary is covered separately by the harness.
func TestDependencyWorkflowDurableSequence(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, o := issueExperimentOptions(t, backend)
			s, err := OpenExisting(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			source, err := s.CreateIssue(ctx, "beads/release", plainIssue("Release"))
			if err != nil {
				t.Fatal(err)
			}
			target, err := s.CreateIssue(ctx, "beads/prerequisite", plainIssue("Prerequisite"))
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"beads/plan", "beads/notes"} {
				if _, err := s.Create(ctx, CreateRequest{Path: path, Title: path, Body: "Memory body"}); err != nil {
					t.Fatal(err)
				}
			}
			assertReadyIDs(t, ctx, s, source.ID, target.ID)
			before := workflowState(t, ctx, s)
			for _, request := range []DependencyRequest{
				{SourcePath: "beads/plan", TargetPath: "beads/release", Actor: "test"},
				{SourcePath: "beads/release", TargetPath: "beads/notes", Actor: "test"},
				{SourcePath: "beads/release", TargetPath: "beads/release", Actor: "test"},
			} {
				if _, err := s.AddDependency(ctx, request); !errors.Is(err, storage.ErrValidation) {
					t.Fatalf("endpoint refusal: %v", err)
				}
			}
			if _, err := s.CloseIssue(ctx, "beads/plan", "done", "test"); !errors.Is(err, storage.ErrValidation) {
				t.Fatalf("Memory close: %v", err)
			}
			if after := workflowState(t, ctx, s); !reflect.DeepEqual(before, after) {
				t.Fatal("refused operation left effects")
			}
			fault := errors.New("injected workflow failure")
			request := DependencyRequest{SourcePath: "beads/release", TargetPath: "beads/prerequisite", Path: "links/release-needs-prerequisite", Actor: "test"}
			for _, stage := range []string{"dependency", "link-catalog", "link-retained", "source-catalog", "source-retained"} {
				t.Run("rollback-"+stage, func(t *testing.T) {
					s.afterWrite = func(at string) error {
						if at == stage {
							return fault
						}
						return nil
					}
					_, err := s.AddDependency(ctx, request)
					s.afterWrite = nil
					if !errors.Is(err, fault) {
						t.Fatalf("want injected refusal, got %v", err)
					}
					if after := workflowState(t, ctx, s); !reflect.DeepEqual(before, after) {
						t.Fatalf("%s rollback left effects", stage)
					}
					if _, err := s.ShowLink(ctx, request.Path); !errors.Is(err, ErrNotFound) {
						t.Fatalf("failed Link visible: %v", err)
					}
				})
			}
			added, err := s.AddDependency(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if !added.Changed || added.Link.Source != source.ID || added.Link.Target != target.ID || added.Source.Version == source.Version || len(added.Source.Owned) != 1 || len(added.Source.Properties.Dependencies) != 0 {
				t.Fatalf("wrong Dependency result: %+v", added)
			}
			var owned LinkRecord
			if err := json.Unmarshal(added.Source.Owned[0], &owned); err != nil || !reflect.DeepEqual(owned, added.Link) {
				t.Fatalf("owned Link differs: %v", err)
			}
			unchanged, err := s.ShowIssue(ctx, "beads/prerequisite")
			if err != nil || !reflect.DeepEqual(unchanged, target) {
				t.Fatalf("target changed on incoming Dependency: %v", err)
			}
			assertReadyIDs(t, ctx, s, target.ID)
			before = workflowState(t, ctx, s)
			stale := request
			stale.ExpectedSourceRevision = source.Revision
			if _, err := s.AddDependency(ctx, stale); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale source guard bypassed by repeat pair: %v", err)
			}
			stale.SourcePath, stale.TargetPath, stale.Path = "beads/prerequisite", "beads/release", "links/stale"
			if _, err := s.AddDependency(ctx, stale); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale source guard did not precede mutation: %v", err)
			}
			request.ExpectedSourceRevision = added.Source.Revision
			request.Path = "" // A repeated user intent must not allocate another Link.
			repeated, err := s.AddDependency(ctx, request)
			if err != nil || repeated.Changed || !reflect.DeepEqual(repeated.Link, added.Link) || !reflect.DeepEqual(repeated.Source, added.Source) {
				t.Fatalf("reassertion differs: %+v %v", repeated, err)
			}
			if _, err := s.AddDependency(ctx, DependencyRequest{SourcePath: request.SourcePath, TargetPath: request.TargetPath, Path: "links/second", Actor: "test"}); !errors.Is(err, storage.ErrValidation) {
				t.Fatalf("pair reallocation: %v", err)
			}
			if _, err := s.AddDependency(ctx, DependencyRequest{SourcePath: request.TargetPath, TargetPath: request.SourcePath, Actor: "test"}); !errors.Is(err, storage.ErrValidation) {
				t.Fatalf("cycle refusal: %v", err)
			}
			if _, err := s.CloseIssue(ctx, "beads/release", "premature", "test"); !errors.Is(err, storage.ErrCloseBlocked) {
				t.Fatalf("blocked close: %v", err)
			}
			if after := workflowState(t, ctx, s); !reflect.DeepEqual(before, after) {
				t.Fatal("no-op/refused writes changed state")
			}
			for _, stage := range []string{"close", "source-catalog", "source-retained"} {
				t.Run("rollback-"+stage, func(t *testing.T) {
					s.afterWrite = func(at string) error {
						if at == stage {
							return fault
						}
						return nil
					}
					_, err := s.CloseIssue(ctx, "beads/prerequisite", "done", "test")
					s.afterWrite = nil
					if !errors.Is(err, fault) {
						t.Fatalf("want injected refusal, got %v", err)
					}
					if after := workflowState(t, ctx, s); !reflect.DeepEqual(before, after) {
						t.Fatalf("%s rollback left effects", stage)
					}
				})
			}
			closed, err := s.CloseIssue(ctx, "beads/prerequisite", "done", "test")
			if err != nil {
				t.Fatal(err)
			}
			if !closed.Changed || closed.Issue.Version == target.Version || closed.Issue.Properties.Status != types.StatusClosed {
				t.Fatalf("close result: %+v", closed)
			}
			assertReadyIDs(t, ctx, s, source.ID)
			currentSource, err := s.ShowIssue(ctx, "beads/release")
			if err != nil || !reflect.DeepEqual(currentSource, added.Source) {
				t.Fatalf("target close changed source's retained state: %v", err)
			}
			before = workflowState(t, ctx, s)
			repeatedClose, err := s.CloseIssue(ctx, "beads/prerequisite", "different ignored reason", "test")
			if err != nil || repeatedClose.Changed || !reflect.DeepEqual(repeatedClose.Issue, closed.Issue) {
				t.Fatalf("repeat close: %+v %v", repeatedClose, err)
			}
			if after := workflowState(t, ctx, s); !reflect.DeepEqual(before, after) {
				t.Fatal("repeat close wrote effects")
			}
			// Read old membership directly from its retained version, never current links.
			for _, tc := range []struct {
				path, version string
				count         int
			}{{"beads/release", source.Version, 0}, {"beads/release", added.Source.Version, 1}, {"beads/prerequisite", target.Version, 0}, {"beads/prerequisite", closed.Issue.Version, 0}} {
				var raw []byte
				if err := s.db.QueryRowContext(ctx, `SELECT owned FROM graph_preview_issue_versions WHERE path=? AND version=?`, tc.path, tc.version).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var links []LinkRecord
				if err := json.Unmarshal(raw, &links); err != nil || len(links) != tc.count {
					t.Fatalf("retained owned membership: %s %v", raw, err)
				}
				if tc.count == 1 && !reflect.DeepEqual(links[0], added.Link) {
					t.Fatal("retained Link state differs")
				}
			}
			var sourceCount, targetCount int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_versions WHERE issue_id=?`, source.Properties.ID).Scan(&sourceCount); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_versions WHERE issue_id=?`, target.Properties.ID).Scan(&targetCount); err != nil {
				t.Fatal(err)
			}
			if sourceCount != 2 || targetCount != 2 {
				t.Fatalf("wrong mint count source=%d target=%d", sourceCount, targetCount)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = OpenExisting(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			read, err := s.Read(ctx, strings.TrimPrefix(added.Link.ID, o.Binding.ScopeURL))
			if err != nil || !reflect.DeepEqual(read, added.Link) {
				t.Fatalf("Link reopen: %v", err)
			}
			currentSource, err = s.ShowIssue(ctx, "beads/release")
			if err != nil || !reflect.DeepEqual(currentSource, added.Source) {
				t.Fatalf("Issue reopen: %v", err)
			}
			assertReadyIDs(t, ctx, s, source.ID)
		})
	}
}

func assertReadyIDs(t *testing.T, ctx context.Context, s *Store, want ...string) {
	t.Helper()
	ready, err := s.ReadyIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(ready))
	for _, r := range ready {
		got = append(got, r.ID)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("ready got %v want %v", got, want)
	}
}

// Fingerprint all affected authoritative/retained tables, including the writer
// fence and derived readiness. This detects updates as well as orphan rows.
func workflowState(t *testing.T, ctx context.Context, s *Store) map[string]string {
	t.Helper()
	state := map[string]string{}
	for _, table := range []string{"issues", "dependencies", "events", "bd_events_journal", "bd_events_seq", "leases", "issue_versions", "store_epoch", "local_metadata", "graph_preview_scope", "graph_preview_catalog", "graph_preview_versions", "graph_preview_issue_versions", "graph_preview_links", "graph_preview_payloads"} {
		rows, err := s.db.QueryContext(ctx, "SELECT * FROM "+table)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		values := []string{}
		for rows.Next() {
			fields := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range fields {
				pointers[i] = &fields[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, string(b))
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		sort.Strings(values)
		state[table] = strings.Join(values, "\n")
	}
	return state
}

// Both backend paths use real transactions. Embedded opens serialize; the
// ordinary server test forces fully overlapping dependency/close snapshots.
// The shared writer cell must reject one server commit, including when Dolt
// could otherwise merge disjoint edge, status and derived is_blocked cells.
func TestDependencyCloseConflict(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		for _, closePath := range []string{"beads/release", "beads/prerequisite"} {
			t.Run(backend+"/close-"+strings.TrimPrefix(closePath, "beads/"), func(t *testing.T) {
				ctx, o := issueExperimentOptions(t, backend)
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				initial, err := OpenExisting(ctx, o)
				if err != nil {
					t.Fatal(err)
				}
				source, err := initial.CreateIssue(ctx, "beads/release", plainIssue("Release"))
				if err != nil {
					t.Fatal(err)
				}
				target, err := initial.CreateIssue(ctx, "beads/prerequisite", plainIssue("Prerequisite"))
				if err != nil {
					t.Fatal(err)
				}
				if err := initial.Close(); err != nil {
					t.Fatal(err)
				}
				reached := make(chan struct{}, 2)
				release := make(chan struct{})
				barrier := func(stage string) error {
					if backend != "server" || stage != "source-retained" {
						return nil
					}
					reached <- struct{}{}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				type outcome struct {
					add bool
					err error
				}
				results := make(chan outcome, 2)
				for _, add := range []bool{true, false} {
					go func() {
						store, err := OpenExisting(ctx, o)
						if err != nil {
							results <- outcome{add, err}
							return
						}
						store.afterWrite = barrier
						if add {
							_, err = store.AddDependency(ctx, DependencyRequest{SourcePath: "beads/release", TargetPath: "beads/prerequisite", Path: "links/race", Actor: "race"})
						} else {
							_, err = store.CloseIssue(ctx, closePath, "done", "race")
						}
						results <- outcome{add, errors.Join(err, store.Close())}
					}()
				}
				if backend == "server" {
					for range 2 {
						select {
						case <-reached:
						case result := <-results:
							t.Fatalf("writer exited before overlap: %+v", result)
						case <-ctx.Done():
							t.Fatal("overlap deadline:", ctx.Err())
						}
					}
					close(release)
				}
				accepted := map[bool]bool{}
				conflicts, blocked := 0, 0
				for range 2 {
					result := <-results
					t.Logf("add=%v outcome=%v", result.add, result.err)
					switch {
					case result.err == nil:
						accepted[result.add] = true
					case errors.Is(result.err, ErrConflict) && !errors.Is(result.err, ErrOutcomeUnknown):
						conflicts++
					case errors.Is(result.err, storage.ErrCloseBlocked):
						blocked++
					default:
						t.Fatal(result.err)
					}
				}
				if backend == "server" && (len(accepted) != 1 || conflicts != 1 || blocked != 0) {
					t.Fatalf("accepted=%v conflicts=%d blocked=%d", accepted, conflicts, blocked)
				}
				if backend == "embedded" && (!accepted[true] || conflicts != 0 || len(accepted)+blocked != 2) {
					t.Fatalf("embedded serialization: accepted=%v conflicts=%d blocked=%d", accepted, conflicts, blocked)
				}
				current, err := OpenExisting(ctx, o)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := current.Close(); err != nil {
						t.Error(err)
					}
				}()
				src, err := current.ShowIssue(ctx, "beads/release")
				if err != nil {
					t.Fatal(err)
				}
				dst, err := current.ShowIssue(ctx, "beads/prerequisite")
				if err != nil {
					t.Fatal(err)
				}
				if accepted[true] {
					if _, err := current.ShowLink(ctx, "links/race"); err != nil {
						t.Fatal(err)
					}
					if len(src.Owned) != 1 {
						t.Fatal("accepted Dependency missing owned state")
					}
				} else {
					if _, err := current.ShowLink(ctx, "links/race"); !errors.Is(err, ErrNotFound) {
						t.Fatalf("losing Link visible: %v", err)
					}
					if len(src.Owned) != 0 {
						t.Fatal("losing Dependency leaked owned state")
					}
				}
				expectedReady := []string{}
				if src.Properties.Status != types.StatusClosed && (!accepted[true] || dst.Properties.Status == types.StatusClosed) {
					expectedReady = append(expectedReady, source.ID)
				}
				if dst.Properties.Status != types.StatusClosed {
					expectedReady = append(expectedReady, target.ID)
				}
				assertReadyIDs(t, ctx, current, expectedReady...)
				expectedVersions := 2 + len(accepted)
				for _, table := range []string{"issue_versions", "graph_preview_issue_versions"} {
					var n int
					if err := current.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n != expectedVersions {
						t.Fatalf("%s count=%d want=%d", table, n, expectedVersions)
					}
				}
				for _, table := range []string{"dependencies", "graph_preview_versions"} {
					var n int
					if err := current.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
						t.Fatal(err)
					}
					expected := 0
					if accepted[true] {
						expected = 1
					}
					if n != expected {
						t.Fatalf("%s count=%d want=%d", table, n, expected)
					}
				}
			})
		}
	}
}

// The Issue schema has ON UPDATE CURRENT_TIMESTAMP. Advancing a local History
// ordinal must not silently alter the durable payload after taking its snapshot.
func TestHistoryBookkeepingPreservesIssueTimestamp(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, o := issueExperimentOptions(t, backend)
			s, err := OpenExisting(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			}()
			issue, err := s.CreateIssue(ctx, "beads/time", plainIssue("Preserve authored timestamp"))
			if err != nil {
				t.Fatal(err)
			}
			// Force a timestamp distinct from the current second, without sleeping.
			want := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
			err = s.withTx(ctx, true, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, `UPDATE issues SET updated_at=? WHERE id=?`, want, issue.Properties.ID); err != nil {
					return err
				}
				unscope := issueops.ScopeVersionedHistoryTransaction(tx, true)
				defer unscope()
				if err := issueops.RecordVersionInTx(ctx, tx, issue.Properties.ID, "test"); err != nil {
					return err
				}
				if err := s.recordIssueMappingInTx(ctx, tx, "beads/time", issue.Properties.ID); err != nil {
					return err
				}
				_, err := s.showIssueInTx(ctx, tx, "beads/time")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.ShowIssue(ctx, "beads/time")
			if err != nil {
				t.Fatal(err)
			}
			if !got.Properties.UpdatedAt.Equal(want) {
				t.Fatalf("History bookkeeping rewrote updated_at: got=%s want=%s", got.Properties.UpdatedAt, want)
			}
		})
	}
}

func TestIssueDatelessDeferral(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, o := issueExperimentOptions(t, backend)
			s, err := OpenExisting(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			}()
			original, err := s.CreateIssue(ctx, "beads/work", plainIssue("Work"))
			if err != nil {
				t.Fatal(err)
			}
			const path = "beads/work"
			deferRequest := IssueDeferralRequest{Path: path, Actor: "operator", ExpectedRevision: original.Revision, Deferred: true}
			beforeFault := workflowState(t, ctx, s)
			fault := errors.New("injected deferral failure")
			s.afterWrite = func(stage string) error {
				if stage == "issue-deferral" {
					return fault
				}
				return nil
			}
			_, err = s.SetIssueDeferred(ctx, deferRequest)
			s.afterWrite = nil
			if !errors.Is(err, fault) || !reflect.DeepEqual(beforeFault, workflowState(t, ctx, s)) {
				t.Fatalf("failed deferral left effects: %v", err)
			}
			deferred, err := s.SetIssueDeferred(ctx, deferRequest)
			if err != nil || !deferred.Changed || deferred.Issue.Properties.Status != types.StatusDeferred || deferred.Issue.Properties.DeferUntil != nil || deferred.Issue.Version == original.Version {
				t.Fatalf("defer: %+v %v", deferred, err)
			}
			assertIssueEditVersion(t, ctx, s, path, original)
			assertIssueEditVersion(t, ctx, s, path, deferred.Issue)
			assertIssueEditCounts(t, ctx, s, original.Properties.ID, 2)
			ready, err := s.ReadyIssues(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range ready {
				if item.ID == original.ID {
					t.Fatalf("deferred Issue appeared ready: %+v", ready)
				}
			}
			if _, err := s.SetIssueDeferred(ctx, deferRequest); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale guard: %v", err)
			}
			beforeNoop := workflowState(t, ctx, s)
			noop, err := s.SetIssueDeferred(ctx, IssueDeferralRequest{Path: path, Actor: "operator", ExpectedRevision: deferred.Issue.Revision, Deferred: true})
			if err != nil || noop.Changed || !reflect.DeepEqual(noop.Issue, deferred.Issue) || !reflect.DeepEqual(beforeNoop, workflowState(t, ctx, s)) {
				t.Fatalf("repeat defer wrote state: %+v %v", noop, err)
			}
			opened, err := s.SetIssueDeferred(ctx, IssueDeferralRequest{Path: path, Actor: "operator", ExpectedRevision: deferred.Issue.Revision})
			if err != nil || !opened.Changed || opened.Issue.Properties.Status != types.StatusOpen || opened.Issue.Properties.DeferUntil != nil {
				t.Fatalf("undefer: %+v %v", opened, err)
			}
			assertIssueEditCounts(t, ctx, s, original.Properties.ID, 3)
			ready, err = s.ReadyIssues(ctx)
			if err != nil {
				t.Fatal(err)
			}
			foundWork := false
			for _, item := range ready {
				foundWork = foundWork || item.ID == original.ID
			}
			if !foundWork {
				t.Fatalf("undeferred Issue not ready: %+v", ready)
			}
			beforeNoop = workflowState(t, ctx, s)
			noop, err = s.SetIssueDeferred(ctx, IssueDeferralRequest{Path: path, Actor: "operator", Unconditional: true})
			if err != nil || noop.Changed || !reflect.DeepEqual(beforeNoop, workflowState(t, ctx, s)) {
				t.Fatalf("repeat undefer wrote state: %+v %v", noop, err)
			}
		})
	}
}

func TestIssueDatedDeferralWakesWithOneNativeAndGraphSuccessor(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, o := issueExperimentOptions(t, backend)
			s, err := OpenExisting(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			created, err := s.CreateIssue(ctx, "beads/snooze", plainIssue("Snooze"))
			if err != nil {
				t.Fatal(err)
			}
			past := time.Now().UTC().Add(-time.Hour)
			deferred, err := s.SetIssueDeferred(ctx, IssueDeferralRequest{
				Path: "beads/snooze", Actor: "operator", Deferred: true,
				Until: &past, Reason: "waiting on review",
			})
			if err != nil || !deferred.Changed || deferred.Issue.Properties.Status != types.StatusDeferred ||
				deferred.Issue.Properties.DeferUntil == nil || deferred.Issue.Properties.Notes != "waiting on review" {
				t.Fatalf("dated defer: %+v %v", deferred, err)
			}
			assertIssueEditCounts(t, ctx, s, created.Properties.ID, 2)
			ready, err := s.ReadyIssues(ctx)
			if err != nil || len(ready) != 1 || ready[0].ID != created.ID {
				t.Fatalf("ready after wake: %+v %v", ready, err)
			}
			woken := ready[0]
			if woken.Properties.Status != types.StatusOpen || woken.Properties.DeferUntil != nil || woken.Revision == deferred.Issue.Revision {
				t.Fatalf("wake did not project native state: %+v", woken)
			}
			assertIssueEditCounts(t, ctx, s, created.Properties.ID, 3)
			assertIssueEditVersion(t, ctx, s, "beads/snooze", woken)
			if _, err := s.ReadyIssues(ctx); err != nil {
				t.Fatal(err)
			}
			assertIssueEditCounts(t, ctx, s, created.Properties.ID, 3)
		})
	}
}
