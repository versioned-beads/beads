//go:build cgo

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/httpapi/bdpwire"
	"github.com/steveyegge/beads/internal/httpapi/graphread"
	"github.com/steveyegge/beads/internal/storage/graphstore"
	"github.com/steveyegge/beads/issueops"
)

// The real listener reads normally initialized, normally written server state.
// Writes remain store API operations; this is not an HTTP Write or History test.
func TestGraphReadMemoryDeletionHTTP(t *testing.T) {
	portText := os.Getenv("BEADS_GRAPH_TEST_SERVER_PORT")
	if portText == "" {
		t.Skip("ordinary shared-server Dolt port not configured")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("invalid ordinary shared-server Dolt port: %q", portText)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const scope = "https://authority.example/deletion/"
	opts := graphstore.Options{Backend: "server", ServerHost: "127.0.0.1", ServerPort: port, ServerUser: "root", Database: fmt.Sprintf("graph_http_delete_%d", time.Now().UnixNano()), Branch: "main", DataDir: filepath.Join(workspace, "dolt"), Binding: graphstore.Binding{WorkspaceID: workspace, ScopeURL: scope, AuthorityID: "0123456789abcdef0123456789abcdef", SchemaVersion: graphstore.SchemaVersion}}
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
		// The empty-value presence case follows sjarmak's gastownhall/beads#5964;
		// its ordinary KV-memory implementation remains a separate surface.
		if _, err := store.Create(ctx, graphstore.CreateRequest{Path: path, Title: "Memory — 雪", Body: "", Actor: "human:original-author"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateIssue(ctx, "beads/work", issueops.CreateRequest{Issue: &issueops.Issue{Title: "Surviving Issue", Status: "open", IssueType: "task", Priority: 2}, Actor: "human:issue-author"}); err != nil {
		t.Fatal(err)
	}
	contextLink, err := store.AddInformationalLink(ctx, graphstore.LinkCreateRequest{Path: "links/context", SourcePath: "beads/plan", TargetPath: "beads/alpha", UnconditionalSource: true, Actor: "human:link-author"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddInformationalLink(ctx, graphstore.LinkCreateRequest{Path: "links/survivor", SourcePath: "beads/work", TargetPath: "beads/alpha", Actor: "human:link-author"}); err != nil {
		t.Fatal(err)
	}
	graph, err := NewGraphRead(graphread.New(store), scope)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(graph.Close)
	const token = "memory-delete-test-token"
	tokenFile := filepath.Join(workspace, "http-token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	auth, err := NewTokenFileAuth(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, Config{GraphRead: graph, Auth: auth})
	get := func(canonical string, headers http.Header) (*http.Response, []byte) {
		t.Helper()
		if !strings.HasPrefix(canonical, scope) {
			t.Fatalf("request left canonical Scope: %q", canonical)
		}
		u, err := url.Parse(canonical)
		if err != nil {
			t.Fatal(err)
		}
		// Only transport changes. Canonical IDs and complete continuation query
		// bytes come from the real authority, never reconstructed cursor values.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.base+u.RequestURI(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = headers.Clone()
		if req.Header == nil {
			req.Header = http.Header{}
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := srv.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatal("authenticated current/retained response was cacheable")
		}
		return resp, body
	}
	read := func(canonical string, output any) *http.Response {
		t.Helper()
		resp, raw := get(canonical, nil)
		if resp.StatusCode != http.StatusOK || bdpwire.Unmarshal(raw, output) != nil {
			t.Fatalf("read %s: %d %s", canonical, resp.StatusCode, raw)
		}
		return resp
	}
	var alpha, plan, work bdpwire.BeadRecord
	read(scope+"beads/alpha", &alpha)
	read(scope+"beads/plan", &plan)
	read(scope+"beads/work", &work)
	if string(plan.Properties["body"]) != `""` || plan.Attribution == nil || plan.Attribution.Principal != "human:link-author" || len(plan.OwnedLinks[contextLink.Link.Type]) != 1 {
		t.Fatal("predelete record lost empty content, attribution or owned Link")
	}
	var linksBefore bdpwire.LinkCollection
	read(scope+"links/?limit=100", &linksBefore)
	if linksBefore.Next != nil || len(linksBefore.Items) != 2 {
		t.Fatal("initial Link inventory incomplete")
	}
	if linksBefore.Items[0].ID != scope+"links/context" || linksBefore.Items[1].ID != scope+"links/survivor" {
		t.Fatal("initial Link inventory identity/order differs")
	}
	var first bdpwire.BeadCollection
	read(scope+"beads/?limit=1", &first)
	if first.Next == nil || !reflect.DeepEqual(first.Items, bdpwire.BeadRecords{alpha}) {
		t.Fatal("first page did not retain the later Memory before deletion")
	}
	retainedURL := *first.Next
	if _, err := store.DeleteMemory(ctx, graphstore.MemoryDeleteRequest{Path: "beads/plan", Unconditional: true}); !errors.Is(err, graphstore.ErrIncidentLinkConstraint) {
		t.Fatalf("linked Memory deletion was accepted: %v", err)
	}
	var stillPlan bdpwire.BeadRecord
	read(scope+"beads/plan", &stillPlan)
	if !reflect.DeepEqual(stillPlan, plan) {
		t.Fatal("refused linked deletion changed HTTP state")
	}
	if _, err := store.Unlink(ctx, graphstore.LinkDeleteRequest{Path: "links/context", ExpectedRevision: contextLink.Link.Revision, ExpectedSourceRevision: plan.Revision, Actor: "human:unlinker"}); err != nil {
		t.Fatal(err)
	}
	var final bdpwire.BeadRecord
	finalResponse := read(scope+"beads/plan", &final)
	if final.Revision == plan.Revision || len(final.OwnedLinks) != 0 || final.Attribution == nil || final.Attribution.Principal != "human:unlinker" || finalResponse.Header.Get("ETag") == "" {
		t.Fatal("explicit unlink did not expose final current Memory")
	}
	deleted, err := store.DeleteMemory(ctx, graphstore.MemoryDeleteRequest{Path: "beads/plan", ExpectedRevision: final.Revision, Actor: "human:deleter"})
	if err != nil || !deleted.Deleted || deleted.Memory.Revision != final.Revision {
		t.Fatalf("guarded deletion: %+v %v", deleted, err)
	}
	for _, path := range []string{"beads/plan", "beads/plan?view=properties", "beads/plan?view=links", "beads/plan?include=links", "beads/missing", "links/context"} {
		resp, raw := get(scope+path, http.Header{"If-None-Match": {finalResponse.Header.Get("ETag")}})
		var problem bdpwire.ReadProblem
		if resp.StatusCode != http.StatusNotFound || bdpwire.Unmarshal(raw, &problem) != nil || problem.Code != bdpwire.CodeResourceNotFound {
			t.Fatalf("current absent Resource %s: %d %s", path, resp.StatusCode, raw)
		}
	}
	collect := func(page bdpwire.BeadCollection) bdpwire.BeadRecords {
		t.Helper()
		items := append(bdpwire.BeadRecords(nil), page.Items...)
		for pages := 1; page.Next != nil; pages++ {
			if pages >= 4 {
				t.Fatal("collection did not terminate within fixture page count")
			}
			read(*page.Next, &page)
			items = append(items, page.Items...)
		}
		return items
	}
	var fresh bdpwire.BeadCollection
	read(scope+"beads/?limit=1", &fresh)
	if got := collect(fresh); !reflect.DeepEqual(got, bdpwire.BeadRecords{alpha, work}) {
		t.Fatalf("fresh complete mixed inventory contains deleted state: %+v", got)
	}
	for _, expected := range []bdpwire.BeadRecord{alpha, work} {
		var current bdpwire.BeadRecord
		read(expected.ID, &current)
		if !reflect.DeepEqual(current, expected) {
			t.Fatal("deletion changed a surviving canonical current Bead")
		}
	}
	var linksAfter bdpwire.LinkCollection
	read(scope+"links/?limit=1", &linksAfter)
	if linksAfter.Next != nil || !reflect.DeepEqual(linksAfter.Items, linksBefore.Items[1:]) {
		t.Fatal("deletion changed surviving Link inventory")
	}
	var survivor bdpwire.LinkRecord
	read(scope+"links/survivor", &survivor)
	if !reflect.DeepEqual(survivor, linksBefore.Items[1]) {
		t.Fatal("deletion changed surviving Link resource")
	}
	// Consume and replay the cursor issued before unlink/delete. It returns the
	// old Memory and its then-owned Link and attribution, not the final live state.
	if got := collect(first); !reflect.DeepEqual(got, bdpwire.BeadRecords{alpha, plan, work}) {
		t.Fatal("retained cursor refreshed deleted Memory state")
	}
	var replay bdpwire.BeadCollection
	read(retainedURL, &replay)
	if !reflect.DeepEqual(replay.Items, bdpwire.BeadRecords{plan}) || replay.Next == nil {
		t.Fatal("retained cursor replay lost old owned state or attribution")
	}
	if strings.Contains(srv.stderr.String(), token) {
		t.Fatal("deletion HTTP request leaked credential")
	}
	t.Log("HTTP_RECEIPT Memory deletion: current 404, complete fresh inventory exclusion, unchanged survivors, retained pre-unlink cursor and replay; no public History or HTTP Write")
}
