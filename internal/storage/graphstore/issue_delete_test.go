//go:build cgo

package graphstore

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestIssueDeleteRetainsHistoryAndRefusesIncidentLinks(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, _, s := memoryDeleteStore(t, backend)
			issue, err := s.CreateIssue(ctx, "beads/work", plainIssue("Work"))
			if err != nil {
				t.Fatal(err)
			}
			before := workflowState(t, ctx, s)
			preview, err := s.DeleteIssue(ctx, IssueDeleteRequest{Path: "beads/work", Preview: true})
			if err != nil || !preview.Preview || preview.Deleted || !reflect.DeepEqual(preview.Issue, issue) {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			if !reflect.DeepEqual(before, workflowState(t, ctx, s)) {
				t.Fatal("preview changed state")
			}
			if _, err := s.DeleteIssue(ctx, IssueDeleteRequest{Path: "beads/work"}); err == nil {
				t.Fatal("apply without a guard succeeded")
			}
			if _, err := s.DeleteIssue(ctx, IssueDeleteRequest{Path: "beads/work", ExpectedRevision: "stale"}); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale guard: %v", err)
			}
			other, err := s.CreateIssue(ctx, "beads/other", plainIssue("Other"))
			if err != nil {
				t.Fatal(err)
			}
			link, err := s.AddInformationalLink(ctx, LinkCreateRequest{Path: "links/relation", SourcePath: "beads/work", TargetPath: "beads/other", TypeURL: RelatedTypeURL(s.ScopeURL())})
			if err != nil {
				t.Fatal(err)
			}
			for _, preview := range []bool{true, false} {
				_, err := s.DeleteIssue(ctx, IssueDeleteRequest{Path: "beads/work", ExpectedRevision: issue.Revision, Preview: preview})
				if !errors.Is(err, ErrIncidentLinkConstraint) || !strings.Contains(err.Error(), link.Link.ID) {
					t.Fatalf("incident Link refusal: %v", err)
				}
			}
			if got, err := s.ShowIssue(ctx, "beads/work"); err != nil || !reflect.DeepEqual(got, issue) {
				t.Fatalf("refusal changed Issue: %+v %v", got, err)
			}
			if got, err := s.ShowIssue(ctx, "beads/other"); err != nil || !reflect.DeepEqual(got, other) {
				t.Fatalf("refusal changed neighbor: %+v %v", got, err)
			}
			if _, err := s.Unlink(ctx, LinkDeleteRequest{Path: "links/relation", ExpectedRevision: link.Link.Revision, DefaultInformationalSource: true}); err != nil {
				t.Fatal(err)
			}
			final, err := s.ShowIssue(ctx, "beads/work")
			if err != nil {
				t.Fatal(err)
			}
			beforeDelete := workflowState(t, ctx, s)
			for _, stage := range []string{"coordination", "issue-delete-native", "issue-delete-allocation"} {
				fault := errors.New("injected Issue delete failure")
				s.afterWrite = func(at string) error {
					if at == stage {
						return fault
					}
					return nil
				}
				got, err := s.DeleteIssue(ctx, IssueDeleteRequest{Path: "beads/work", ExpectedRevision: final.Revision})
				s.afterWrite = nil
				if !errors.Is(err, fault) || !reflect.ValueOf(got).IsZero() || !reflect.DeepEqual(beforeDelete, workflowState(t, ctx, s)) {
					t.Fatalf("%s rollback: %+v %v", stage, got, err)
				}
			}
			kind, prior, err := s.Versions(ctx, "beads/work")
			if err != nil || kind != KindIssue {
				t.Fatalf("prior history: %s %+v %v", kind, prior, err)
			}
			deleted, err := s.DeleteIssue(ctx, IssueDeleteRequest{Path: "beads/work", ExpectedRevision: final.Revision, Actor: "deleter"})
			if err != nil || !deleted.Deleted || deleted.Preview || !reflect.DeepEqual(deleted.Issue, final) {
				t.Fatalf("apply: %+v %v", deleted, err)
			}
			if _, err := s.ShowIssue(ctx, "beads/work"); !errors.Is(err, ErrGone) {
				t.Fatalf("current Issue: %v", err)
			}
			kind, retained, err := s.Versions(ctx, "beads/work")
			if err != nil || kind != KindIssue || !reflect.DeepEqual(retained, prior) {
				t.Fatalf("deletion invented or lost history: %s %+v %v", kind, retained, err)
			}
			old, err := s.ReadVersion(ctx, "beads/work", final.Version)
			retainedIssue, ok := old.(IssueRecord)
			if err != nil || !ok || retainedIssue.ID != final.ID || retainedIssue.Version != final.Version ||
				retainedIssue.Properties == nil || retainedIssue.Properties.ID != final.Properties.ID ||
				retainedIssue.Properties.Title != final.Properties.Title || len(retainedIssue.Owned) != 0 {
				t.Fatalf("final live snapshot: %+v %v", old, err)
			}
			if _, err := s.CreateIssue(ctx, "beads/work", plainIssue("Reuse")); !errors.Is(err, ErrAlreadyExists) {
				t.Fatalf("identity reused: %v", err)
			}
			snapshot, err := s.CurrentSnapshot(ctx)
			if err != nil || len(snapshot.Records) != 1 {
				t.Fatalf("current inventory: %+v %v", snapshot, err)
			}
			if _, err := s.DeleteIssue(ctx, IssueDeleteRequest{Path: "beads/work", Unconditional: true}); !errors.Is(err, ErrGone) {
				t.Fatalf("repeat delete: %v", err)
			}
		})
	}
}

func TestIssueDeleteRefusesBlockingLinksWithoutNativeCascade(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			f := setupDependencyUnlink(t, backend, 2)
			before := workflowState(t, f.ctx, f.store)
			for _, preview := range []bool{true, false} {
				_, err := f.store.DeleteIssue(f.ctx, IssueDeleteRequest{Path: "beads/source", ExpectedRevision: f.source.Revision, Preview: preview})
				if !errors.Is(err, ErrIncidentLinkConstraint) {
					t.Fatalf("blocking Link refusal: %v", err)
				}
				first, second := f.links[0].ID, f.links[1].ID
				if !strings.Contains(err.Error(), first+", "+second) {
					t.Fatalf("incident Link IDs not sorted and complete: %v", err)
				}
				if !reflect.DeepEqual(before, workflowState(t, f.ctx, f.store)) {
					t.Fatal("refusal changed native Issue, dependency or graph state")
				}
			}
		})
	}
}
