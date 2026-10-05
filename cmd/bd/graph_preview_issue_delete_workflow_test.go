//go:build cgo

package main

import (
	"os"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/storage/graphstore"
)

func TestGraphPreviewIssueDeleteWorkflow(t *testing.T) {
	bd := buildBDUnderTest(t)
	for _, engine := range []string{"embedded", "server"} {
		t.Run(engine, func(t *testing.T) {
			work, home := t.TempDir(), t.TempDir()
			const scope = "https://example.invalid/issue-delete/"
			initArgs := []string{"init", "--graph-mode", "link", "--scope-url", scope, "--skip-hooks", "--skip-agents", "--non-interactive"}
			if engine == "server" {
				port := os.Getenv("BEADS_GRAPH_TEST_SERVER_PORT")
				if port == "" {
					t.Skip("set BEADS_GRAPH_TEST_SERVER_PORT for ordinary shared-server CLI qualification")
				}
				initArgs = append(initArgs, "--server", "--external", "--server-host", "127.0.0.1", "--server-port", port, "--server-user", "root")
			}
			call := func(args ...string) string {
				t.Helper()
				return graphPolicyCLI(t, bd, work, home, nil, "", append(args, "--json")...)
			}
			refuse := func(code string, args ...string) {
				t.Helper()
				graphPolicyCLI(t, bd, work, home, nil, code, append(args, "--json")...)
			}
			call(initArgs...)
			issue := graphMixedResult[graphstore.IssueRecord](t, call("create", "Work", "--id", "beads/work"))
			preview := graphMixedResult[graphstore.IssueDeleteResult](t, call("delete", "work"))
			if !preview.Preview || preview.Deleted || !reflect.DeepEqual(preview.Issue, issue) {
				t.Fatal("default preview did not disclose unchanged Issue")
			}
			refuse("invalid_selector", "delete", "work", "--force")
			refuse("revision_conflict", "delete", "work", "--force", "--if-revision", "stale")
			refuse("capability_unavailable", "forget", "work", "--unconditional")
			call("create", "Other", "--id", "beads/other")
			call("link", "work", "other", "--link-type", "types/preview-related-v2", "--id", "links/relation")
			refuse("constraint_violation", "delete", "work")
			refuse("constraint_violation", "delete", "work", "--force", "--if-revision", issue.Revision)
			if got := graphMixedResult[graphstore.IssueRecord](t, call("show", "work")); !reflect.DeepEqual(got, issue) {
				t.Fatal("incident Link refusal changed Issue")
			}
			link := graphMixedResult[graphstore.LinkRecord](t, call("show", "links/relation"))
			call("unlink", "links/relation", "--if-revision", link.Revision)
			final := graphMixedResult[graphstore.IssueRecord](t, call("show", "work"))
			before := call("versions", "work")
			deleted := graphMixedResult[graphstore.IssueDeleteResult](t, call("delete", "work", "--force", "--if-revision", final.Revision))
			if !deleted.Deleted || deleted.Preview || !reflect.DeepEqual(deleted.Issue, final) {
				t.Fatal("apply did not disclose final live Issue")
			}
			refuse("gone", "show", "work")
			refuse("gone", "delete", "work", "--force", "--unconditional")
			refuse("identity_reserved", "create", "Replacement", "--id", "beads/work")
			if after := call("versions", "work"); after != before {
				t.Fatal("deletion invented or lost an Issue version")
			}
			if old := graphMixedResult[graphstore.IssueRecord](t, call("show", "work", "--version", final.Version)); old.ID != final.ID || old.Version != final.Version {
				t.Fatal("final live snapshot unavailable")
			}
			call("show", "other")
		})
	}
}
