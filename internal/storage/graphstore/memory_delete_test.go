//go:build cgo

package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
)

func memoryDeleteStore(t *testing.T, backend string) (context.Context, Options, *Store) {
	t.Helper()
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
	return ctx, o, s
}

func TestMemoryDeleteLifecycle(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, o, s := memoryDeleteStore(t, backend)
			// sjarmak's gastownhall/beads#5964 highlighted that an empty value is
			// present. This exercises that lesson on graph Memory, not the older KV API.
			memory, err := s.Create(ctx, CreateRequest{Path: "beads/empty", Actor: "author"})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := s.Show(ctx, "beads/empty"); err != nil || !reflect.DeepEqual(got, memory) {
				t.Fatalf("empty present: %+v %v", got, err)
			}
			before := workflowState(t, ctx, s)
			preview, err := s.DeleteMemory(ctx, MemoryDeleteRequest{Path: "beads/empty", Preview: true})
			if err != nil || !preview.Preview || preview.Deleted || !reflect.DeepEqual(preview.Memory, memory) {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			if !reflect.DeepEqual(before, workflowState(t, ctx, s)) {
				t.Fatal("preview wrote state")
			}
			got, err := s.DeleteMemory(ctx, MemoryDeleteRequest{Path: "beads/empty", ExpectedRevision: memory.Revision, Actor: "deleter"})
			if err != nil || got.Preview || !got.Deleted || !reflect.DeepEqual(got.Memory, memory) {
				t.Fatalf("delete: %+v %v", got, err)
			}
			after := workflowState(t, ctx, s)
			for table, value := range before {
				if table != "graph_preview_scope" && table != "graph_preview_catalog" && table != "graph_preview_payloads" && after[table] != value {
					t.Fatalf("delete changed %s", table)
				}
			}
			for _, read := range []func() error{
				func() error { _, err := s.Show(ctx, "beads/empty"); return err },
				func() error { _, err := s.Read(ctx, "beads/empty"); return err },
			} {
				if err := read(); !errors.Is(err, ErrGone) {
					t.Fatalf("current: %v", err)
				}
			}
			snapshot, err := s.CurrentSnapshot(ctx)
			if err != nil || len(snapshot.Records) != 0 {
				t.Fatalf("deleted inventory: %+v %v", snapshot, err)
			}
			if old, err := s.ReadVersion(ctx, "beads/empty", memory.Version); err != nil || !reflect.DeepEqual(old, memory) {
				t.Fatalf("final live revision: %+v %v", old, err)
			}
			if _, err := s.Create(ctx, CreateRequest{Path: "beads/empty", Body: "reuse"}); !errors.Is(err, ErrAlreadyExists) {
				t.Fatalf("identity reused: %v", err)
			}
			for _, preview := range []bool{true, false} {
				got, err := s.DeleteMemory(ctx, MemoryDeleteRequest{Path: "beads/empty", Unconditional: true, Preview: preview})
				if !errors.Is(err, ErrGone) || !reflect.ValueOf(got).IsZero() {
					t.Fatalf("repeat: %+v %v", got, err)
				}
			}
			if !reflect.DeepEqual(after, workflowState(t, ctx, s)) {
				t.Fatal("reads or repeated delete changed state")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenExisting(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			}()
			if _, err := reopened.Show(ctx, "beads/empty"); !errors.Is(err, ErrGone) {
				t.Fatalf("reopened current: %v", err)
			}
			if old, err := reopened.ReadVersion(ctx, "beads/empty", memory.Version); err != nil || !reflect.DeepEqual(old, memory) {
				t.Fatalf("reopened retained: %+v %v", old, err)
			}
		})
	}
}

func TestMemoryDeleteRefusalRollback(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, _, s := memoryDeleteStore(t, backend)
			memory, err := s.Create(ctx, CreateRequest{Path: "beads/plan", Body: "present"})
			if err != nil {
				t.Fatal(err)
			}
			issue, err := s.CreateIssue(ctx, "beads/work", plainIssue("Work"))
			if err != nil {
				t.Fatal(err)
			}
			request := MemoryDeleteRequest{Path: "beads/plan", ExpectedRevision: memory.Revision}
			before := workflowState(t, ctx, s)
			for _, tc := range []struct {
				name    string
				request MemoryDeleteRequest
				want    error
			}{
				{"missing-guard", MemoryDeleteRequest{Path: request.Path}, storage.ErrValidation},
				{"both-guards", MemoryDeleteRequest{Path: request.Path, ExpectedRevision: memory.Revision, Unconditional: true}, storage.ErrValidation},
				{"stale", MemoryDeleteRequest{Path: request.Path, ExpectedRevision: "stale"}, ErrConflict},
				{"stale-preview", MemoryDeleteRequest{Path: request.Path, ExpectedRevision: "stale", Preview: true}, ErrConflict},
				{"issue", MemoryDeleteRequest{Path: "beads/work", ExpectedRevision: issue.Revision}, ErrCapabilityUnavailable},
				{"missing", MemoryDeleteRequest{Path: "beads/missing", Unconditional: true}, ErrNotFound},
				{"link-path", MemoryDeleteRequest{Path: "links/not-a-bead", Unconditional: true}, storage.ErrValidation},
				{"actor", MemoryDeleteRequest{Path: request.Path, Actor: string([]byte{255}), Unconditional: true}, storage.ErrValidation},
			} {
				got, err := s.DeleteMemory(ctx, tc.request)
				if !errors.Is(err, tc.want) || !reflect.ValueOf(got).IsZero() {
					t.Fatalf("%s: %+v %v", tc.name, got, err)
				}
				if !reflect.DeepEqual(before, workflowState(t, ctx, s)) {
					t.Fatalf("%s changed state", tc.name)
				}
			}
			for _, stage := range []string{"coordination", "memory-delete-allocation", "memory-delete-payload"} {
				fault := errors.New("injected delete failure")
				s.afterWrite = func(at string) error {
					if at == stage {
						return fault
					}
					return nil
				}
				got, err := s.DeleteMemory(ctx, request)
				s.afterWrite = nil
				if !errors.Is(err, fault) || !reflect.ValueOf(got).IsZero() {
					t.Fatalf("%s: %+v %v", stage, got, err)
				}
				if !reflect.DeepEqual(before, workflowState(t, ctx, s)) {
					t.Fatalf("%s leaked state", stage)
				}
			}
			canceled, cancel := context.WithCancel(ctx)
			s.afterWrite = func(stage string) error {
				if stage == "memory-delete-payload" {
					cancel()
					return canceled.Err()
				}
				return nil
			}
			got, err := s.DeleteMemory(canceled, request)
			cancel()
			s.afterWrite = nil
			if !errors.Is(err, context.Canceled) || !reflect.ValueOf(got).IsZero() {
				t.Fatalf("cancel: %+v %v", got, err)
			}
			if !reflect.DeepEqual(before, workflowState(t, ctx, s)) {
				t.Fatal("cancel leaked state")
			}
			original := s.options
			s.options.Binding.AuthorityID = "ffffffffffffffffffffffffffffffff"
			_, err = s.DeleteMemory(ctx, request)
			s.options = original
			if !errors.Is(err, ErrInvalidStore) {
				t.Fatalf("authority: %v", err)
			}
			if !reflect.DeepEqual(before, workflowState(t, ctx, s)) {
				t.Fatal("authority refusal wrote state")
			}
			// Explicit unconditional intent discloses the final live predecessor.
			got, err = s.DeleteMemory(ctx, MemoryDeleteRequest{Path: request.Path, Unconditional: true})
			if err != nil || !got.Deleted || !reflect.DeepEqual(got.Memory, memory) {
				t.Fatalf("unconditional: %+v %v", got, err)
			}
		})
	}
}

func TestMemoryDeleteIncidentLinks(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, _, s := memoryDeleteStore(t, backend)
			for i, shape := range []string{"incoming-owned", "incoming-unowned", "outgoing-memory", "outgoing-issue", "self"} {
				t.Run(shape, func(t *testing.T) {
					path := fmt.Sprintf("beads/target%d", i)
					otherPath := fmt.Sprintf("beads/other%d", i)
					linkPath := fmt.Sprintf("links/incident%d", i)
					memory, err := s.Create(ctx, CreateRequest{Path: path, Body: "target"})
					if err != nil {
						t.Fatal(err)
					}
					sourcePath, targetPath, sourceRevision := path, otherPath, memory.Revision
					if shape == "incoming-unowned" || shape == "outgoing-issue" {
						issue, err := s.CreateIssue(ctx, otherPath, plainIssue("Issue"))
						if err != nil {
							t.Fatal(err)
						}
						if shape == "incoming-unowned" {
							sourcePath, targetPath, sourceRevision = otherPath, path, issue.Revision
						}
					} else if shape == "self" {
						targetPath = path
					} else {
						other, err := s.Create(ctx, CreateRequest{Path: otherPath, Body: "other"})
						if err != nil {
							t.Fatal(err)
						}
						if shape == "incoming-owned" {
							sourcePath, targetPath, sourceRevision = otherPath, path, other.Revision
						}
					}
					link, err := s.AddInformationalLink(ctx, LinkCreateRequest{Path: linkPath, SourcePath: sourcePath, TargetPath: targetPath, ExpectedSourceRevision: sourceRevision})
					if err != nil {
						t.Fatal(err)
					}
					current, err := s.Show(ctx, path)
					if err != nil {
						t.Fatal(err)
					}
					before := workflowState(t, ctx, s)
					for _, preview := range []bool{true, false} {
						got, err := s.DeleteMemory(ctx, MemoryDeleteRequest{Path: path, ExpectedRevision: current.Revision, Preview: preview})
						if !errors.Is(err, ErrIncidentLinkConstraint) || !strings.Contains(err.Error(), linkPath) || !reflect.ValueOf(got).IsZero() {
							t.Fatalf("incident preview=%v: %+v %v", preview, got, err)
						}
						if !reflect.DeepEqual(before, workflowState(t, ctx, s)) {
							t.Fatal("incident refusal changed state")
						}
					}
					source, err := s.Read(ctx, sourcePath)
					if err != nil {
						t.Fatal(err)
					}
					switch source := source.(type) {
					case Record:
						sourceRevision = source.Revision
					case IssueRecord:
						sourceRevision = source.Revision
					default:
						t.Fatalf("source: %T", source)
					}
					if _, err := s.Unlink(ctx, LinkDeleteRequest{Path: linkPath, ExpectedRevision: link.Link.Revision, ExpectedSourceRevision: sourceRevision}); err != nil {
						t.Fatal(err)
					}
					unlinked, err := s.Show(ctx, path)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.DeleteMemory(ctx, MemoryDeleteRequest{Path: path, ExpectedRevision: unlinked.Revision}); err != nil {
						t.Fatal(err)
					}
					for _, want := range []Record{memory, current, unlinked} {
						old, err := s.ReadVersion(ctx, path, want.Version)
						if err != nil || !reflect.DeepEqual(old, want) {
							t.Fatalf("owned history preserved: %+v %v", old, err)
						}
					}
					if old, err := s.ReadVersion(ctx, linkPath, link.Link.Version); err != nil || !reflect.DeepEqual(old, link.Link) {
						t.Fatalf("historical Link: %+v %v", old, err)
					}
					if _, err := s.CurrentSnapshot(ctx); err != nil {
						t.Fatalf("inventory after unlink and delete: %v", err)
					}
				})
			}
		})
	}
}

func TestMemoryDeleteCorruptAbsentState(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, _, s := memoryDeleteStore(t, backend)
			memory, err := s.Create(ctx, CreateRequest{Path: "beads/plan"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateIssue(ctx, "beads/work", plainIssue("Work")); err != nil {
				t.Fatal(err)
			}
			linked, err := s.AddInformationalLink(ctx, LinkCreateRequest{Path: "links/out", SourcePath: "beads/plan", TargetPath: "beads/work", ExpectedSourceRevision: memory.Revision})
			if err != nil {
				t.Fatal(err)
			}
			owned := linked.Source.(Record)
			if _, err := s.Unlink(ctx, LinkDeleteRequest{Path: "links/out", ExpectedRevision: linked.Link.Revision, ExpectedSourceRevision: owned.Revision}); err != nil {
				t.Fatal(err)
			}
			memory, err = s.Show(ctx, "beads/plan")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DeleteMemory(ctx, MemoryDeleteRequest{Path: "beads/plan", ExpectedRevision: memory.Revision}); err != nil {
				t.Fatal(err)
			}
			before := workflowState(t, ctx, s)
			for _, tc := range []struct{ name, query string }{
				{"owned-head", fmt.Sprintf("UPDATE graph_preview_catalog SET revision='%s' WHERE path='beads/plan'", owned.Revision)},
				{"incident-row", `INSERT INTO graph_preview_links(path,source_path,target_path,properties,attribution) VALUES('links/corrupt','beads/work','beads/plan','{}','{}')`},
				{"payload", `INSERT INTO graph_preview_payloads(path,properties) VALUES('beads/plan','{"title":"","body":""}')`},
				{"missing-head", `DELETE FROM graph_preview_versions WHERE path='beads/plan'`},
				{"malformed-head", `UPDATE graph_preview_versions SET snapshot='{}' WHERE path='beads/plan'`},
				{"wrong-type", `UPDATE graph_preview_catalog SET type_url='https://wrong.example/memory' WHERE path='beads/plan'`},
				{"wrong-backing", `UPDATE graph_preview_catalog SET backing='issue' WHERE path='beads/plan'`},
				{"backing-key", `UPDATE graph_preview_catalog SET backing_key='bad' WHERE path='beads/plan'`},
				{"wrong-kind", `UPDATE graph_preview_catalog SET resource_kind='link' WHERE path='beads/plan'`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					err := s.withTx(ctx, false, func(tx *sql.Tx) error {
						if _, err := tx.ExecContext(ctx, tc.query); err != nil {
							return err
						}
						if _, err := s.showMemoryInTx(ctx, tx, "beads/plan"); !errors.Is(err, ErrInvalidStore) {
							return fmt.Errorf("current accepted corrupt deletion: %v", err)
						}
						if _, err := s.readVersionInTx(ctx, tx, "beads/plan", memory.Version); !errors.Is(err, ErrInvalidStore) {
							return fmt.Errorf("exact accepted corrupt deletion: %v", err)
						}
						if _, err := s.currentSnapshotInTx(ctx, tx); !errors.Is(err, ErrInvalidStore) {
							return fmt.Errorf("inventory accepted corrupt deletion: %v", err)
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before, workflowState(t, ctx, s)) {
						t.Fatal("corruption control escaped rollback")
					}
				})
			}
		})
	}
}

func TestMemoryDeleteConcurrentWriters(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		for _, other := range []string{"memory-update", "incoming-link"} {
			t.Run(backend+"/"+other, func(t *testing.T) {
				ctx, o, first := memoryDeleteStore(t, backend)
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				memory, err := first.Create(ctx, CreateRequest{Path: "beads/plan", Body: "original"})
				if err != nil {
					t.Fatal(err)
				}
				issue, err := first.CreateIssue(ctx, "beads/work", plainIssue("Work"))
				if err != nil {
					t.Fatal(err)
				}
				// Embedded callers use the production one-session pool; only the
				// ordinary server branch below forces overlapping SQL snapshots.
				second := &Store{db: first.db, options: o}
				if backend == "server" {
					second, err = OpenExisting(ctx, o)
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := second.Close(); err != nil {
							t.Error(err)
						}
					}()
				}
				reached := make(chan struct{}, 2)
				release := make(chan struct{})
				results := make(chan error, 2)
				barrier := func(stage string) error {
					if backend != "server" || (stage != "memory-delete-payload" && stage != "source-retained" && stage != "link-retained") {
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
				first.afterWrite, second.afterWrite = barrier, barrier
				var writers sync.WaitGroup
				writers.Add(2)
				defer func() { cancel(); writers.Wait() }()
				go func() {
					defer writers.Done()
					_, err := first.DeleteMemory(ctx, MemoryDeleteRequest{Path: "beads/plan", ExpectedRevision: memory.Revision})
					results <- err
				}()
				go func() {
					defer writers.Done()
					var err error
					if other == "memory-update" {
						_, err = second.UpdateMemory(ctx, MemoryUpdateRequest{Path: "beads/plan", ExpectedRevision: memory.Revision, Properties: Properties{Body: "updated"}})
					} else {
						_, err = second.AddInformationalLink(ctx, LinkCreateRequest{Path: "links/in", SourcePath: "beads/work", TargetPath: "beads/plan", ExpectedSourceRevision: issue.Revision})
					}
					results <- err
				}()
				if backend == "server" {
					for range 2 {
						select {
						case <-reached:
						case err := <-results:
							t.Fatalf("writer before overlap: %v", err)
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						}
					}
					close(release)
				}
				successes, refusals := 0, 0
				for range 2 {
					select {
					case err := <-results:
						if err == nil {
							successes++
						} else if !errors.Is(err, ErrOutcomeUnknown) && (errors.Is(err, ErrConflict) || (backend == "embedded" && (errors.Is(err, ErrGone) || errors.Is(err, ErrIncidentLinkConstraint)))) {
							refusals++
						} else {
							t.Fatalf("writer outcome: %v", err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				writers.Wait()
				first.afterWrite, second.afterWrite = nil, nil
				if successes != 1 || refusals != 1 {
					t.Fatalf("successes=%d refusals=%d", successes, refusals)
				}
				current, readErr := first.Show(ctx, "beads/plan")
				if other == "memory-update" {
					if readErr != nil && !errors.Is(readErr, ErrGone) {
						t.Fatal(readErr)
					}
					if readErr == nil && (current.Properties.Body != "updated" || current.Version == memory.Version) {
						t.Fatalf("incomplete update: %+v", current)
					}
				} else {
					link, linkErr := first.ShowLink(ctx, "links/in")
					if errors.Is(readErr, ErrGone) {
						if !errors.Is(linkErr, ErrNotFound) {
							t.Fatalf("deleted endpoint acquired Link: %+v %v", link, linkErr)
						}
					} else if readErr != nil || linkErr != nil || !reflect.DeepEqual(current, memory) {
						t.Fatalf("incomplete Link winner: %+v %v %+v %v", current, readErr, link, linkErr)
					}
				}
				if _, err := first.CurrentSnapshot(ctx); err != nil {
					t.Fatalf("race corrupted inventory: %v", err)
				}
				if got, err := first.ShowIssue(ctx, "beads/work"); err != nil || !reflect.DeepEqual(got, issue) {
					t.Fatalf("race changed unowned Issue source: %+v %v", got, err)
				}
				wantVersions := 1
				if other == "memory-update" && readErr == nil {
					wantVersions = 2
				}
				var versions int
				if err := first.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM graph_preview_versions WHERE path='beads/plan'`).Scan(&versions); err != nil || versions != wantVersions {
					t.Fatalf("race versions=%d want=%d: %v", versions, wantVersions, err)
				}
				if old, err := first.ReadVersion(ctx, "beads/plan", memory.Version); err != nil || !reflect.DeepEqual(old, memory) {
					t.Fatalf("race changed retained: %+v %v", old, err)
				}
			})
		}
	}
}

func TestMemoryDeleteLostCommitResponse(t *testing.T) {
	ctx, o, direct := memoryDeleteStore(t, "server")
	memory, err := direct.Create(ctx, CreateRequest{Path: "beads/plan", Body: "retained"})
	if err != nil {
		t.Fatal(err)
	}
	before := workflowState(t, ctx, direct)
	port, observed := startCommitLossProxy(t, net.JoinHostPort(o.ServerHost, strconv.Itoa(o.ServerPort)))
	proxied := o
	proxied.ServerPort = port
	s, err := OpenExisting(ctx, proxied)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	got, err := s.DeleteMemory(ctx, MemoryDeleteRequest{Path: "beads/plan", ExpectedRevision: memory.Revision})
	if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrConflict) || !reflect.ValueOf(got).IsZero() {
		t.Fatalf("unknown deletion: %+v %v", got, err)
	}
	select {
	case packet := <-observed:
		if len(packet) == 0 || packet[0] != 0 {
			t.Fatalf("no server commit witness: %x", packet)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Only the proxy witnessed COMMIT. The writer returns zero success state and
	// performs no replay; a fresh connection establishes the actual outcome.
	reopened, err := OpenExisting(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := reopened.Show(ctx, "beads/plan"); !errors.Is(err, ErrGone) {
		t.Fatalf("witnessed absence: %v", err)
	}
	if old, err := reopened.ReadVersion(ctx, "beads/plan", memory.Version); err != nil || !reflect.DeepEqual(old, memory) {
		t.Fatalf("witnessed retention: %+v %v", old, err)
	}
	if after := workflowState(t, ctx, reopened); after["graph_preview_versions"] != before["graph_preview_versions"] {
		t.Fatal("deletion minted a version")
	}
}

// Current inventory validates deleted Memory heads, so their acquisition must be
// charged even though they are not returned as live Resources.
func TestMemoryDeleteCurrentReadBudget(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, _, s := memoryDeleteStore(t, backend)
			memory, err := s.Create(ctx, CreateRequest{Path: "beads/deleted", Body: strings.Repeat("d", 3<<20)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DeleteMemory(ctx, MemoryDeleteRequest{Path: "beads/deleted", ExpectedRevision: memory.Revision}); err != nil {
				t.Fatal(err)
			}
			if inventory, err := s.CurrentSnapshot(ctx); err != nil || len(inventory.Records) != 0 {
				t.Fatalf("bounded deleted head: %+v %v", inventory, err)
			}
			if _, err := s.Create(ctx, CreateRequest{Path: "beads/small", Body: "small"}); err != nil {
				t.Fatal(err)
			}
			// Filler has two current copies and alone fits; the retained deleted head
			// takes the complete acquired state beyond the existing 16 MiB limit.
			if _, err := s.Create(ctx, CreateRequest{Path: "beads/filler", Body: strings.Repeat("f", 7<<20)}); err != nil {
				t.Fatal(err)
			}
			before := workflowState(t, ctx, s)
			assertCurrentReadBudgetRefusal(t, ctx, s, "beads/deleted")
			if !reflect.DeepEqual(before, workflowState(t, ctx, s)) {
				t.Fatal("budget refusal changed state")
			}
		})
	}
}
