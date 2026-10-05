//go:build cgo

package graphstore

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

func TestIssueHolderUnclaim(t *testing.T) {
	for _, backend := range []string{"embedded", "server"} {
		t.Run(backend, func(t *testing.T) {
			ctx, _, s, before, target, dependency := reopenFixture(t, backend)
			claimed, err := s.ClaimIssue(ctx, "beads/work", "rig.agent")
			if err != nil || !claimed.Changed {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			assertClaimLease(t, ctx, s, before.Properties.ID, "rig.agent")
			state := reopenState(t, ctx, s)
			foreign, err := s.UnclaimIssue(ctx, "beads/work", "foreign")
			if !errors.Is(err, storage.ErrNotOwner) || !reflect.DeepEqual(foreign, IssueMutationResult{}) || !reflect.DeepEqual(state, reopenState(t, ctx, s)) {
				t.Fatalf("foreign unclaim changed claim: %+v %v", foreign, err)
			}
			fault := errors.New("release must roll back")
			s.afterWrite = func(stage string) error {
				if stage == "issue-unclaim" {
					return fault
				}
				return nil
			}
			failed, err := s.UnclaimIssue(ctx, "beads/work", "rig.agent")
			s.afterWrite = nil
			if !errors.Is(err, fault) || !reflect.DeepEqual(failed, IssueMutationResult{}) || !reflect.DeepEqual(state, reopenState(t, ctx, s)) {
				t.Fatalf("unclaim rollback: %+v %v", failed, err)
			}
			released, err := s.UnclaimIssue(ctx, "beads/work", "rig_agent")
			if err != nil || !released.Changed {
				t.Fatalf("holder alias release: %+v %v", released, err)
			}
			p := released.Issue.Properties
			if p == nil || p.Status != types.StatusOpen || p.Assignee != "" || p.StartedAt != nil || p.LeaseExpiresAt != nil || p.HeartbeatAt != nil || released.Issue.Revision == claimed.Issue.Revision || released.Issue.Version != released.Issue.Revision || released.Issue.Attribution.Actor != "rig_agent" {
				t.Fatalf("incomplete release: %+v", released.Issue)
			}
			assertAssigneeLeaseCount(t, ctx, s, before.Properties.ID, 0)
			assertIssueEditCounts(t, ctx, s, before.Properties.ID, 4)
			if err := s.AddIssueComment(ctx, "beads/work", "rig_agent", "handoff"); err != nil {
				t.Fatalf("reason comment: %v", err)
			}
			comments, err := s.IssueComments(ctx, "beads/work")
			if err != nil || len(comments) != 1 || comments[0].Text != "handoff" {
				t.Fatalf("native comment feed: %+v %v", comments, err)
			}
			if shown, err := s.ShowIssue(ctx, "beads/work"); err != nil || !reflect.DeepEqual(shown, released.Issue) {
				t.Fatalf("comment affected Issue snapshot: %+v %v", shown, err)
			}
			assertIssueEditCounts(t, ctx, s, before.Properties.ID, 4)
			for _, record := range []IssueRecord{before, claimed.Issue, released.Issue} {
				assertIssueEditVersion(t, ctx, s, "beads/work", record)
			}
			state = reopenState(t, ctx, s)
			again, err := s.UnclaimIssue(ctx, "beads/work", "rig.agent")
			if !errors.Is(err, publicops.ErrNotReleasable) || !reflect.DeepEqual(again, IssueMutationResult{}) || !reflect.DeepEqual(state, reopenState(t, ctx, s)) {
				t.Fatalf("repeat release changed state: %+v %v", again, err)
			}
			if got, err := s.ShowIssue(ctx, "beads/prereq"); err != nil || !reflect.DeepEqual(got, target) {
				t.Fatalf("target changed: %+v %v", got, err)
			}
			if got, err := s.ShowLink(ctx, "links/block"); err != nil || !reflect.DeepEqual(got, dependency) {
				t.Fatalf("dependency changed: %+v %v", got, err)
			}
			assigned, err := s.UpdateIssue(ctx, UpdateIssueRequest{Path: "beads/work", Actor: "assigner", ExpectedRevision: released.Issue.Revision, Assignee: issueEditString("rig.agent")})
			if err != nil || !assigned.Changed || assigned.Issue.Properties.Status != types.StatusOpen {
				t.Fatalf("open assignment: %+v %v", assigned, err)
			}
			state = reopenState(t, ctx, s)
			openRelease, err := s.UnclaimIssue(ctx, "beads/work", "rig.agent")
			if err != nil || !openRelease.Changed || openRelease.Issue.Properties.Status != types.StatusOpen || openRelease.Issue.Properties.Assignee != "" {
				t.Fatalf("native open assignment release failed: %+v %v", openRelease, err)
			}
			if reflect.DeepEqual(state, reopenState(t, ctx, s)) {
				t.Fatal("open assignment release did not record the native transition")
			}
			reclaimed, err := s.ClaimIssue(ctx, "beads/work", "next.agent")
			if err != nil || !reclaimed.Changed || reclaimed.Issue.Properties.Assignee != "next.agent" {
				t.Fatalf("reclaim after release: %+v %v", reclaimed, err)
			}
			assertClaimLease(t, ctx, s, before.Properties.ID, "next.agent")
			state = reopenState(t, ctx, s)
			mismatch, err := s.UnclaimIssueWithPolicy(ctx, "beads/work", "supervisor", false, "old.agent", true)
			if !errors.Is(err, storage.ErrAssigneeMismatch) || !reflect.DeepEqual(mismatch, IssueMutationResult{}) || !reflect.DeepEqual(state, reopenState(t, ctx, s)) {
				t.Fatalf("conditional mismatch changed claim: %+v %v", mismatch, err)
			}
			forced, err := s.UnclaimIssueWithPolicy(ctx, "beads/work", "supervisor", true, "", false)
			if err != nil || !forced.Changed || forced.Issue.Properties.Assignee != "" {
				t.Fatalf("force did not release native claim: %+v %v", forced, err)
			}
			if _, err := s.ClaimIssue(ctx, "beads/work", "next.agent"); err != nil {
				t.Fatalf("claim after force: %v", err)
			}
			conditional, err := s.UnclaimIssueWithPolicy(ctx, "beads/work", "supervisor", false, "next.agent", true)
			if err != nil || !conditional.Changed || conditional.Issue.Properties.Assignee != "" {
				t.Fatalf("conditional release failed: %+v %v", conditional, err)
			}
		})
	}
}
