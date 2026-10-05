package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// CloseIssue delegates to the checked Issue-domain close, in the same fenced
// transaction as retained state and graph identity. It offers no force bypass.
func (s *Store) CloseIssue(ctx context.Context, path, reason, actor string) (IssueMutationResult, error) {
	if err := validatePath(path); err != nil {
		return IssueMutationResult{}, err
	}
	if actor == "" || !utf8.ValidString(actor) || !utf8.ValidString(reason) {
		return IssueMutationResult{}, fmt.Errorf("%w: close requires UTF-8 actor and reason", storage.ErrValidation)
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
		if before.Properties.Status == types.StatusClosed {
			result = IssueMutationResult{Issue: before}
			return nil
		}
		if err := s.touchCoordination(ctx, tx); err != nil {
			return err
		}
		unscope := issueops.ScopeVersionedHistoryTransaction(tx, true)
		defer unscope()
		closed, _, err := issueops.ExecuteClose(ctx, tx, publicops.CloseRequest{IssueID: before.Properties.ID, Reason: reason, Actor: actor})
		if err != nil {
			return err
		}
		if !closed.Changed {
			return fmt.Errorf("%w: close unexpectedly became a no-op", ErrInvalidStore)
		}
		if err := s.afterStage("close"); err != nil {
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

// IssueDeferralRequest changes one durable Issue's deferral state. An optional
// graph revision guard is checked before the native Issue writer records the
// sole retained successor.
type IssueDeferralRequest struct {
	Path, Actor, ExpectedRevision string
	Unconditional, Deferred       bool
	Until                         *time.Time
	Reason                        string
}

// SetIssueDeferred uses the existing native Issue update funnel. The native
// writer owns its History stamp; graphstore records only the resulting graph
// projection in the same transaction.
func (s *Store) SetIssueDeferred(ctx context.Context, request IssueDeferralRequest) (IssueMutationResult, error) {
	if err := validatePath(request.Path); err != nil {
		return IssueMutationResult{}, fmt.Errorf("%w: %v", storage.ErrValidation, err)
	}
	if request.Actor == "" || !utf8.ValidString(request.Actor) {
		return IssueMutationResult{}, fmt.Errorf("%w: deferral requires a nonempty UTF-8 actor", storage.ErrValidation)
	}
	if !utf8.ValidString(request.Reason) || (!request.Deferred && (request.Until != nil || request.Reason != "")) {
		return IssueMutationResult{}, fmt.Errorf("%w: invalid deferral options", storage.ErrValidation)
	}
	var result IssueMutationResult
	err := s.withTx(ctx, true, func(tx *sql.Tx) error {
		if err := checkBinding(ctx, tx, s.options); err != nil {
			return err
		}
		before, err := s.requireIssueInTx(ctx, tx, request.Path)
		if err != nil {
			return err
		}
		if err := checkRevisionGuard(request.ExpectedRevision, request.Unconditional, before.Revision, false, "Issue"); err != nil {
			return err
		}
		untilMatches := (request.Until == nil && before.Properties.DeferUntil == nil) ||
			(request.Until != nil && before.Properties.DeferUntil != nil && request.Until.Equal(*before.Properties.DeferUntil))
		if (request.Deferred && before.Properties.Status == types.StatusDeferred && untilMatches && request.Reason == "") ||
			(!request.Deferred && before.Properties.Status != types.StatusDeferred && before.Properties.DeferUntil == nil) {
			result = IssueMutationResult{Issue: before}
			return nil
		}
		patch := publicops.IssuePatch{DeferUntil: publicops.Field[*time.Time]{Set: true}}
		if request.Deferred {
			patch.Status = publicops.Field[types.Status]{Set: true, Value: types.StatusDeferred}
			patch.DeferUntil.Value = request.Until
			if request.Reason != "" {
				patch.AppendNotes = publicops.Field[string]{Set: true, Value: request.Reason}
			}
		} else if before.Properties.Status == types.StatusDeferred {
			patch.Status = publicops.Field[types.Status]{Set: true, Value: types.StatusOpen}
		}
		if err := s.touchCoordination(ctx, tx); err != nil {
			return err
		}
		unscope := issueops.ScopeVersionedHistoryTransaction(tx, true)
		defer unscope()
		updated, _, err := issueops.ExecuteUpdate(ctx, tx, publicops.UpdateRequest{
			IssueID: before.Properties.ID, Actor: request.Actor, Patch: patch, IssuePlaneOnly: true,
		})
		if err != nil {
			return err
		}
		if !updated.Changed {
			return fmt.Errorf("%w: deferral unexpectedly became a no-op", ErrInvalidStore)
		}
		if err := s.afterStage("issue-deferral"); err != nil {
			return err
		}
		if err := s.recordIssueMappingInTx(ctx, tx, request.Path, before.Properties.ID); err != nil {
			return err
		}
		after, err := s.showIssueInTx(ctx, tx, request.Path)
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

// wakeExpiredDefersAdvisory mirrors the native ready-front behavior. The
// Issue-domain sweep owns each wake event and History successor; this adapter
// only retains the corresponding graph projection in the same transaction.
// Failure is advisory to the subsequent read, as in the ordinary backends.
func (s *Store) wakeExpiredDefersAdvisory(ctx context.Context) {
	err := s.withTx(ctx, true, func(tx *sql.Tx) error {
		if err := checkBinding(ctx, tx, s.options); err != nil {
			return err
		}
		var due int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE status='deferred' AND defer_until IS NOT NULL AND defer_until<=UTC_TIMESTAMP()`).Scan(&due); err != nil {
			return err
		}
		if due == 0 {
			return nil
		}
		if err := s.touchCoordination(ctx, tx); err != nil {
			return err
		}
		unscope := issueops.ScopeVersionedHistoryTransaction(tx, true)
		defer unscope()
		woke, err := issueops.WakeExpiredDefersInTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range woke.Issues {
			var path string
			if err := tx.QueryRowContext(ctx, `SELECT path FROM graph_preview_catalog WHERE backing='issue' AND backing_key=? AND allocation_state='live'`, id).Scan(&path); err != nil {
				return fmt.Errorf("%w: unmapped woken Issue: %v", ErrInvalidStore, err)
			}
			if err := s.recordIssueMappingInTx(ctx, tx, path, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "warning: graph defer-wake sweep skipped: %s\n", strings.TrimSpace(err.Error()))
	}
}

// ReadyIssues uses the native lazy defer wake and scheduling query, then
// resolves every result into the same canonical graph.
func (s *Store) ReadyIssues(ctx context.Context) ([]IssueRecord, error) {
	s.wakeExpiredDefersAdvisory(ctx)
	result := []IssueRecord{}
	err := s.withTx(ctx, false, func(tx *sql.Tx) error {
		if err := checkBinding(ctx, tx, s.options); err != nil {
			return err
		}
		ready, err := issueops.GetReadyWorkInTx(ctx, tx, types.WorkFilter{})
		if err != nil {
			return err
		}
		for _, issue := range ready {
			var path string
			if err := tx.QueryRowContext(ctx, `SELECT path FROM graph_preview_catalog WHERE backing='issue' AND backing_key=? AND allocation_state='live'`, issue.ID).Scan(&path); err != nil {
				return fmt.Errorf("%w: unmapped ready Issue: %v", ErrInvalidStore, err)
			}
			record, err := s.showIssueInTx(ctx, tx, path)
			if err != nil {
				return err
			}
			result = append(result, record)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
