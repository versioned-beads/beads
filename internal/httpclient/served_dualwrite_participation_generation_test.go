//go:build cgo

package httpclient

import (
	"context"
	"database/sql"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The ParticipationGenerationFixture contracts (design §16.2b's write fence),
// run through client → in-process bd serve → reference store: the same shape,
// and the same binding rule, as served_dualwrite_history_test.go.
//
// Mutate and MutateExisting are bound to the CLIENT'S Lifecycle role (Create and
// Update over the wire), not to the reference store: the contract is a claim
// about what a client-driven write causes the SERVER to record, and a fixture
// that wrote through the reference store would only prove the storage engine
// fences its own direct writes, which the dolt/embeddeddolt/uow legs already
// cover. SetFlag and the three postcondition reads stay on the REFERENCE side,
// as every other served fixture's hooks do: the flag is a knob on the store
// instance the server's roles are bound to, and participation_generation is a
// column no read role projects.
//
// FIVE OF THE SEVEN ENTRYPOINTS RUN HERE. The other two
// (RunParticipationGenerationWispPromotionBeginsHistory and
// RunParticipationGenerationWispPersistenceMoveBeginsHistory) begin a wisp's
// history by moving it onto the issues plane, and this wire publishes neither
// way of doing that: it has no promotion operation, and updateIssue and
// issues:batchApply both publish no `persistence` member. They are named
// waivers in internal/storage/leg_contract_wiring_test.go, not parked cases
// here, and CreateWisp, PromoteWisp and MovePersistent are left unbound for the
// same reason: binding them to the reference store would let a direct store
// mutation stand in for client coverage.

func newServedParticipationGenerationFixture(t *testing.T, prefix string) conformance.ParticipationGenerationFixture {
	t.Helper()
	env := newServedEnv(t, prefix)
	// Through the type assertion `bd serve` makes, never the concrete method set
	// — same discipline as newServedDualWriteFixture. The flag is read per
	// transaction, so a toggle here reaches the in-process server on its next
	// request.
	configurer, ok := any(env.reference).(storage.VersionedHistoryConfigurer)
	if !ok {
		t.Fatalf("%T does not implement storage.VersionedHistoryConfigurer", env.reference)
	}

	lifecycle, err := env.subject.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle(): %v", err)
	}

	return conformance.ParticipationGenerationFixture{
		IssuePrefix: env.prefix,
		SetFlag: func(ctx context.Context, enabled bool) error {
			configurer.SetVersionedHistoryEnabled(enabled)
			return nil
		},
		Mutate: func(ctx context.Context, id string) error {
			_, err := lifecycle.Create(ctx, issueops.CreateRequest{
				Actor:         "writer",
				ForceIDPrefix: true,
				Issue: &types.Issue{
					ID: id, Title: "t-" + id, Status: types.StatusOpen,
					Priority: 1, IssueType: types.TypeTask,
				},
			})
			return err
		},
		MutateExisting: func(ctx context.Context, id string) error {
			// A different title than Mutate gave it, not a re-assertion of the
			// same one: issueops.DiscardNoopIssueUpdates would discard the latter
			// before it ever reached the fence these cases test.
			_, err := lifecycle.Update(ctx, issueops.UpdateRequest{
				Actor:   "writer",
				IssueID: id,
				Patch:   issueops.IssuePatch{Title: set("updated-" + id)},
			})
			return err
		},
		CurrentRevision: func(ctx context.Context, id string) (int64, error) {
			var revision int64
			err := env.queryScalar(ctx, "SELECT current_revision FROM issues WHERE id = ?", []any{id}, &revision)
			return revision, err
		},
		VersionRowCount: func(ctx context.Context, id string) (int, error) {
			var count int
			err := env.queryScalar(ctx, "SELECT COUNT(*) FROM issue_versions WHERE issue_id = ?", []any{id}, &count)
			return count, err
		},
		ParticipationGeneration: func(ctx context.Context, id string) (*int64, error) {
			var gen sql.NullInt64
			if err := env.queryScalar(ctx,
				"SELECT participation_generation FROM issues WHERE id = ?", []any{id}, &gen); err != nil {
				return nil, err
			}
			if !gen.Valid {
				return nil, nil
			}
			return &gen.Int64, nil
		},
	}
}

// TestServedParticipationGenerationFence runs the five fence cases this wire can
// express. One env serves all five, as in TestServedDualWriteContract: every
// case sets the flag state it needs before it writes anything and mints its ids
// under a suffix of its own, so none depends on what another left behind.
func TestServedParticipationGenerationFence(t *testing.T) {
	ctx := t.Context()
	fixture := newServedParticipationGenerationFixture(t, "hpg")

	t.Run("CreateWithFlagOnStampsAValue", func(t *testing.T) {
		conformance.RunParticipationGenerationCreateWithFlagOnStampsAValue(t, ctx, fixture)
	})
	t.Run("CreateWithFlagOffLeavesLegacyNull", func(t *testing.T) {
		conformance.RunParticipationGenerationCreateWithFlagOffLeavesLegacyNull(t, ctx, fixture)
	})
	t.Run("SkipsUpdateOnLegacyRecord", func(t *testing.T) {
		conformance.RunParticipationGenerationSkipsUpdateOnLegacyRecord(t, ctx, fixture)
	})
	t.Run("ProceedsForPromotedRecord", func(t *testing.T) {
		conformance.RunParticipationGenerationProceedsForPromotedRecord(t, ctx, fixture)
	})
	t.Run("FenceIgnoresSchemaSkewHatch", func(t *testing.T) {
		conformance.RunParticipationGenerationFenceIgnoresSchemaSkewHatch(t, ctx, fixture)
	})
}
