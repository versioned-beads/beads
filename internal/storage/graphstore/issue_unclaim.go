package graphstore

import (
	"context"
	"database/sql"
	"fmt"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// UnclaimIssue keeps the holder-only spelling for existing graph callers.
func (s *Store) UnclaimIssue(ctx context.Context, path, actor string) (IssueMutationResult, error) {
	return s.UnclaimIssueWithPolicy(ctx, path, actor, false, "", false)
}

// UnclaimIssueWithPolicy delegates authorization, lease cleanup, row CAS and
// the single native History stamp to the ordinary Issue writer. Force bypasses
// holder authorization only; conditional release checks the expected holder.
func (s *Store) UnclaimIssueWithPolicy(ctx context.Context, path, actor string, force bool, expectedAssignee string, conditional bool) (IssueMutationResult, error) {
	if err := validatePath(path); err != nil {
		return IssueMutationResult{}, fmt.Errorf("%w: %v", storage.ErrValidation, err)
	}
	if actor == "" || !utf8.ValidString(actor) {
		return IssueMutationResult{}, fmt.Errorf("%w: Issue unclaim requires a nonempty UTF-8 actor", storage.ErrValidation)
	}
	if err := types.CheckFieldLen("actor", actor); err != nil {
		return IssueMutationResult{}, fmt.Errorf("%w: %w", storage.ErrValidation, err)
	}
	if force && conditional {
		return IssueMutationResult{}, fmt.Errorf("%w: force and conditional unclaim are mutually exclusive", storage.ErrValidation)
	}
	if conditional && (expectedAssignee == "" || !utf8.ValidString(expectedAssignee)) {
		return IssueMutationResult{}, fmt.Errorf("%w: expected assignee must be nonempty UTF-8", storage.ErrValidation)
	}
	if conditional {
		if err := types.CheckFieldLen("expected assignee", expectedAssignee); err != nil {
			return IssueMutationResult{}, fmt.Errorf("%w: %w", storage.ErrValidation, err)
		}
	}
	var result IssueMutationResult
	err := s.withTx(ctx, true, func(tx *sql.Tx) error {
		if err := checkBinding(ctx, tx, s.options); err != nil {
			return err
		}
		before, err := s.requireIssueInTx(ctx, tx, path)
		if err != nil {
			return err
		}
		if before.Properties.Status == types.StatusClosed || (!conditional && before.Properties.Assignee == "") {
			return fmt.Errorf("%w: Issue %s is closed or unassigned", publicops.ErrNotReleasable, before.Properties.ID)
		}
		if err := s.touchCoordination(ctx, tx); err != nil {
			return err
		}
		unscope := issueops.ScopeVersionedHistoryTransaction(tx, true)
		defer unscope()
		if conditional {
			err = issueops.UnclaimIssueIfAssigneeInTx(ctx, tx, before.Properties.ID, actor, expectedAssignee)
		} else {
			err = issueops.UnclaimIssueInTx(ctx, tx, before.Properties.ID, actor, force)
		}
		if err != nil {
			return err
		}
		if err := s.afterStage("issue-unclaim"); err != nil {
			return err
		}
		if err := s.recordIssueMappingInTx(ctx, tx, path, before.Properties.ID); err != nil {
			return err
		}
		after, err := s.showIssueInTx(ctx, tx, path)
		if err != nil {
			return err
		}
		result = IssueMutationResult{Issue: after, Changed: true}
		return nil
	})
	if err != nil {
		return IssueMutationResult{}, err
	}
	return result, nil
}
