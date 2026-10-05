//go:build cgo

package graphread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/httpapi/bdpwire"
	"github.com/steveyegge/beads/internal/storage/graphstore"
	"github.com/steveyegge/beads/issueops"
)

// Retained collection pages follow the pinned BDP snapshot semantics; they are
// not public History. All state below comes from normal Init and store writers.
func TestMemoryDeletionCurrentReadAndRetainedPages(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const scope = "https://example.test/deletion/"
			opts := graphstore.Options{Backend: backend, Database: fmt.Sprintf("graph_read_delete_%d", time.Now().UnixNano()), DataDir: filepath.Join(workspace, "dolt"), Branch: "main", Binding: graphstore.Binding{WorkspaceID: workspace, ScopeURL: scope, AuthorityID: "0123456789abcdef0123456789abcdef", SchemaVersion: graphstore.SchemaVersion}}
			if backend == "server" {
				port := os.Getenv("BEADS_GRAPH_TEST_SERVER_PORT")
				if port == "" {
					t.Skip("ordinary shared-server Dolt port not configured")
				}
				opts.ServerPort, err = strconv.Atoi(port)
				if err != nil || opts.ServerPort < 1 || opts.ServerPort > 65535 {
					t.Fatalf("invalid ordinary shared-server Dolt port: %q", port)
				}
				opts.ServerHost, opts.ServerUser = "127.0.0.1", "root"
			}
			if err := graphstore.Init(ctx, opts); err != nil {
				t.Fatal(err)
			}
			store, err := graphstore.OpenExisting(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			for _, path := range []string{"beads/alpha", "beads/plan"} {
				// Empty content remains present, as highlighted by sjarmak's
				// gastownhall/beads#5964 on the separate ordinary KV-memory path.
				if _, err := store.Create(ctx, graphstore.CreateRequest{Path: path, Title: path, Body: "", Actor: "human:original-author"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.CreateIssue(ctx, "beads/work", issueops.CreateRequest{Issue: &issueops.Issue{Title: "Surviving Issue", Status: "open", IssueType: "task", Priority: 2}, Actor: "human:issue-author"}); err != nil {
				t.Fatal(err)
			}
			owned, err := store.AddInformationalLink(ctx, graphstore.LinkCreateRequest{Path: "links/context", SourcePath: "beads/plan", TargetPath: "beads/alpha", UnconditionalSource: true, Actor: "human:link-author"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AddInformationalLink(ctx, graphstore.LinkCreateRequest{Path: "links/survivor", SourcePath: "beads/work", TargetPath: "beads/alpha", Actor: "human:link-author"}); err != nil {
				t.Fatal(err)
			}
			reader := New(store)
			before := checkInventory(t, ctx, reader, 3, 2)
			if before.Beads[1].ID != scope+"beads/plan" || before.Links[0].ID != scope+"links/context" {
				t.Fatal("fixture does not place the deleted record after the first page")
			}
			query, err := CompileCollection(scope, "beads", url.Values{"limit": {"1"}}, selectionLimits)
			if err != nil {
				t.Fatal(err)
			}
			items, err := reader.Select(ctx, query)
			if err != nil || len(items) != 3 {
				t.Fatalf("initial mixed selection: %d %v", len(items), err)
			}
			pager, err := NewPagination(DefaultPaginationOptions(scope))
			if err != nil {
				t.Fatal(err)
			}
			defer pager.Close()
			first, err := pager.FirstPage(FirstPageInput{Items: items, Limit: 1, AuthorizationView: "test-view", ScopeEpoch: "test-epoch", Projection: query.Projection(), ContinuationURL: query.ContinuationURL()})
			if err != nil || first.Next == nil || len(first.Items) != 1 || !reflect.DeepEqual(first.Items, items[:1]) {
				t.Fatalf("first retained page: %+v %v", first, err)
			}
			if _, err := store.DeleteMemory(ctx, graphstore.MemoryDeleteRequest{Path: "beads/plan", Unconditional: true}); !errors.Is(err, graphstore.ErrIncidentLinkConstraint) {
				t.Fatalf("linked deletion policy was bypassed: %v", err)
			}
			if !reflect.DeepEqual(before, checkInventory(t, ctx, reader, 3, 2)) {
				t.Fatal("refused deletion changed wire inventory")
			}
			if _, err := store.Unlink(ctx, graphstore.LinkDeleteRequest{Path: "links/context", ExpectedRevision: owned.Link.Revision, ExpectedSourceRevision: before.Beads[1].Revision, Actor: "human:unlinker"}); err != nil {
				t.Fatal(err)
			}
			final, err := store.Show(ctx, "beads/plan")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.DeleteMemory(ctx, graphstore.MemoryDeleteRequest{Path: "beads/plan", ExpectedRevision: final.Revision, Actor: "human:deleter"}); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"beads/plan", "links/context"} {
				if _, err := reader.Resource(ctx, path); !errors.Is(err, graphstore.ErrGone) {
					t.Fatalf("current deleted Resource %s: %v", path, err)
				}
			}
			after := checkInventory(t, ctx, reader, 2, 1)
			if after.WriterToken == before.WriterToken || !reflect.DeepEqual(after.Beads, []bdpwire.BeadRecord{before.Beads[0], before.Beads[2]}) || !reflect.DeepEqual(after.Links, before.Links[1:]) || !reflect.DeepEqual(after.Types, before.Types) {
				t.Fatal("new inventory lost surviving records or retained a deleted Memory/Link")
			}
			fresh, err := reader.Select(ctx, query)
			if err != nil || !reflect.DeepEqual(fresh, []json.RawMessage{items[0], items[2]}) {
				t.Fatalf("new complete selection contains deleted state: %v", err)
			}
			// Consume every already-issued page only after the unlink and delete.
			// The old Memory's attribution and owned Link remain its captured bytes.
			retained := append([]json.RawMessage(nil), first.Items...)
			for page := first; page.Next != nil; {
				if len(retained) >= len(items) {
					t.Fatal("retained traversal failed to terminate")
				}
				next, err := url.Parse(*page.Next)
				if err != nil {
					t.Fatal(err)
				}
				continuation, err := CompileCollection(scope, "beads", next.Query(), selectionLimits)
				if err != nil {
					t.Fatal(err)
				}
				input := ContinuationInput{Token: continuation.Cursor(), AuthorizationView: "test-view", ScopeEpoch: "test-epoch", Projection: continuation.Projection()}
				page, err = pager.ContinuePage(input)
				if err != nil {
					t.Fatal(err)
				}
				checkCollectionWire(t, "beadCollection", page, &bdpwire.BeadCollection{})
				replay, err := pager.ContinuePage(input)
				if err != nil || !reflect.DeepEqual(replay, page) {
					t.Fatalf("retained deletion replay changed: %v", err)
				}
				retained = append(retained, page.Items...)
			}
			if !reflect.DeepEqual(retained, items) {
				t.Fatal("deletion refreshed an already-issued collection snapshot")
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = graphstore.OpenExisting(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			reader = New(store)
			if _, err := reader.Resource(ctx, "beads/plan"); !errors.Is(err, graphstore.ErrGone) || !reflect.DeepEqual(after, checkInventory(t, ctx, reader, 2, 1)) {
				t.Fatalf("reopen lost current deletion state: %v", err)
			}
		})
	}
}
