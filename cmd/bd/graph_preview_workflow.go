package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	graph "github.com/steveyegge/beads/graphops"
	"github.com/steveyegge/beads/internal/storage/graphstore"
	"github.com/steveyegge/beads/internal/timeparsing"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
)

// Returned canonical IDs can be fed back to the CLI. A bare local Bead path
// is CLI shorthand for beads/PATH; mixed commands keep links/PATH explicit.
// No store lookup, alias resolution or remote routing chooses the kind.
func graphPreviewResourcePath(scope, selector string) (string, error) {
	if graph.ValidateBeadPath(selector) == nil || graph.ValidateLinkPath(selector) == nil {
		return selector, nil
	}
	if path, kind, ok := graph.SplitCanonicalURL(scope, selector); ok && (kind == graph.KindBead || kind == graph.KindLink) {
		return path, nil
	}
	return graphPreviewBareBeadPath(selector)
}

func graphPreviewBareBeadPath(selector string) (string, error) {
	for _, root := range []string{"beads/", "links/", "alias/", "types/"} {
		if strings.HasPrefix(selector, root) {
			return "", fmt.Errorf("invalid local selector %q; use a valid beads/PATH or links/PATH", selector)
		}
	}
	if strings.Contains(selector, "://") {
		return "", fmt.Errorf("invalid local selector %q; a URL must be this workspace's Scope URL followed by beads/PATH or links/PATH, and --id accepts only a bare ID or beads/PATH", selector)
	}
	path := "beads/" + selector
	if err := graph.ValidateBeadPath(path); err != nil {
		return "", fmt.Errorf("invalid Bead ID %q: %w", selector, err)
	}
	return path, nil
}

// Both familiar spellings and the experimental generic Type selector reach
// one domain operation. No alternate Link writer can bypass Issue policy.
func runGraphPreviewAddDependency(cmd *cobra.Command, args []string) error {
	if err := graphPreviewWritePolicy(); err != nil {
		return err
	}
	if err := graphPreviewFlags(cmd, "type", "link-type", "resource-type", "if-source-revision", "unconditional-source"); err != nil {
		return err
	}
	if len(args) != 2 {
		return graphFailure("invalid_selector", "graph Dependency creation requires two Bead IDs or beads/PATH arguments", 2)
	}
	paths := make([]string, len(args))
	for i, selector := range args {
		path, err := graphPreviewResourcePath(graphPreviewConfig.GraphScopeURL, selector)
		if err != nil {
			return graphFailure("invalid_selector", err.Error(), 2)
		}
		if err := graph.ValidateBeadPath(path); err != nil {
			return graphFailure("invalid_selector", err.Error(), 2)
		}
		paths[i] = path
	}
	var sourceRevision string
	if graphPreviewLinkTypeChanged(cmd) {
		if cmd.Flags().Changed("type") {
			return graphFailure("invalid_selector", "select either --type or --link-type", 2)
		}
		sourceRevision, _ = cmd.Flags().GetString("if-source-revision")
		unconditional, _ := cmd.Flags().GetBool("unconditional-source")
		if cmd.Flags().Changed("if-source-revision") == cmd.Flags().Changed("unconditional-source") || (sourceRevision == "" && !unconditional) {
			return graphFailure("invalid_selector", "generic owned Link creation requires either --if-source-revision REVISION or --unconditional-source", 2)
		}
		resourceType, err := graphPreviewLinkType(cmd)
		if err != nil {
			return err
		}
		if resourceType != graphstore.DependencyTypeURL(graphPreviewConfig.GraphScopeURL) {
			return graphFailure("capability_unavailable", "this preview accepts only the installed Type "+graphstore.DependencyTypeURL(graphPreviewConfig.GraphScopeURL), 5)
		}
	} else {
		if cmd.Flags().Changed("if-source-revision") || cmd.Flags().Changed("unconditional-source") {
			return graphFailure("invalid_selector", "source guard options require --link-type", 2)
		}
		raw, _ := cmd.Flags().GetString("type")
		typ := canonicalDependencyType(types.DependencyType(raw))
		if err := validateDependencyType(typ); err != nil {
			return graphFailure("invalid_properties", err.Error(), 2)
		}
		if typ != types.DepBlocks {
			return graphFailure("capability_unavailable", "this preview supports only local blocking Dependencies", 5)
		}
	}
	return withGraphStore(func(ctx context.Context, store *graphstore.Store) (any, string, error) {
		result, err := store.AddDependency(ctx, graphstore.DependencyRequest{SourcePath: paths[0], TargetPath: paths[1], Actor: getActorWithGit(), ExpectedSourceRevision: sourceRevision})
		if err != nil {
			if strings.Contains(err.Error(), "operation requires a live durable Issue, not generic") {
				return nil, "", fmt.Errorf("blocking Link Type requires two live Issues; use an informational Type such as types/preview-related-v2 for Memory endpoints: %w", err)
			}
			return nil, "", err
		}
		verb := "Created"
		if !result.Changed {
			verb = "Unchanged"
		}
		return result, fmt.Sprintf("%s %s: %s depends on %s\n", verb, result.Link.ID, args[0], args[1]), nil
	})
}

func runGraphPreviewClose(cmd *cobra.Command, args []string) error {
	if err := graphPreviewWritePolicy(); err != nil {
		return err
	}
	if err := graphPreviewFlags(cmd, "reason", "resolution", "message", "comment"); err != nil {
		return err
	}
	if len(args) != 1 {
		return graphFailure("invalid_selector", "graph close requires one Bead ID or beads/PATH", 2)
	}
	path, err := graphPreviewResourcePath(graphPreviewConfig.GraphScopeURL, args[0])
	if err != nil {
		return graphFailure("invalid_selector", err.Error(), 2)
	}
	if err := graph.ValidateBeadPath(path); err != nil {
		return graphFailure("invalid_selector", err.Error(), 2)
	}
	reasons, _, err := resolveCloseReasons(cmd, args)
	if err != nil {
		return graphFailure("invalid_properties", err.Error(), 2)
	}
	if err := validateCloseReasons(reasons); err != nil {
		return graphFailure("invalid_properties", err.Error(), 2)
	}
	return withGraphStore(func(ctx context.Context, store *graphstore.Store) (any, string, error) {
		result, err := store.CloseIssue(ctx, path, reasons[0], getActorWithGit())
		if err != nil {
			return nil, "", err
		}
		verb := "Closed"
		if !result.Changed {
			verb = "Already closed"
		}
		return result, fmt.Sprintf("%s %s\n", verb, args[0]), nil
	})
}

func runGraphPreviewDeferral(cmd *cobra.Command, args []string, deferred bool) error {
	if err := graphPreviewWritePolicy(); err != nil {
		return err
	}
	allowed := []string{"if-revision", "unconditional"}
	if deferred {
		allowed = append(allowed, "until", "reason")
	}
	if err := graphPreviewFlags(cmd, allowed...); err != nil {
		return err
	}
	if len(args) == 0 {
		return graphFailure("invalid_selector", "graph deferral requires at least one Issue ID or beads/PATH", 2)
	}
	if len(args) > 1 && cmd.Flags().Changed("if-revision") {
		return graphFailure("invalid_selector", "--if-revision applies to one Issue; use separate commands for guarded updates", 2)
	}
	paths := make([]string, len(args))
	for i, selector := range args {
		path, err := graphPreviewResourcePath(graphPreviewConfig.GraphScopeURL, selector)
		if err != nil {
			return graphFailure("invalid_selector", err.Error(), 2)
		}
		if err := graph.ValidateBeadPath(path); err != nil {
			return graphFailure("invalid_selector", err.Error(), 2)
		}
		paths[i] = path
	}
	revision, unconditional, err := graphPreviewRevisionGuard(cmd, false, false)
	if err != nil {
		return err
	}
	var until *time.Time
	var reason string
	if deferred {
		raw, _ := cmd.Flags().GetString("until")
		if raw != "" {
			parsed, err := timeparsing.ParseRelativeTime(raw, time.Now())
			if err != nil {
				return graphFailure("invalid_properties", fmt.Sprintf("invalid --until format %q. %s", raw, deferUntilFormatHint), 2)
			}
			until = &parsed
			if parsed.Before(time.Now()) && !jsonOutput {
				fmt.Fprintf(os.Stderr, "%s Defer date %q is in the past. Issue will appear in bd ready immediately.\n",
					ui.RenderWarn("!"), parsed.Local().Format("2006-01-02 15:04"))
				fmt.Fprintln(os.Stderr, "  Did you mean a future date? Use --until=+1h or --until=tomorrow")
			}
		}
		reason, _ = cmd.Flags().GetString("reason")
		reason = strings.TrimSpace(reason)
		if cmd.Flags().Changed("reason") && reason == "" {
			return graphFailure("invalid_properties", "reason cannot be empty", 2)
		}
	}
	return withGraphStore(func(ctx context.Context, store *graphstore.Store) (any, string, error) {
		results := make([]graphstore.IssueMutationResult, 0, len(paths))
		var human strings.Builder
		for i, path := range paths {
			result, err := store.SetIssueDeferred(ctx, graphstore.IssueDeferralRequest{
				Path: path, Actor: getActorWithGit(), ExpectedRevision: revision,
				Unconditional: unconditional, Deferred: deferred, Until: until, Reason: reason,
			})
			if err != nil {
				if len(paths) == 1 {
					return nil, "", err
				}
				fmt.Fprintf(os.Stderr, "Error changing deferral for %s: %v\n", args[i], err)
				continue
			}
			results = append(results, result)
			verb := "Undeferred"
			if deferred {
				verb = "Deferred"
			}
			if !result.Changed {
				verb = "Unchanged"
			}
			fmt.Fprintf(&human, "%s %s\n", verb, result.Issue.ID)
		}
		if len(paths) == 1 {
			return results[0], strings.TrimSuffix(human.String(), "\n"), nil
		}
		return results, strings.TrimSuffix(human.String(), "\n"), nil
	})
}

func runGraphPreviewReady(cmd *cobra.Command, args []string) error {
	if err := graphPreviewFlags(cmd); err != nil {
		return err
	}
	if len(args) != 0 {
		return graphFailure("invalid_selector", "graph ready takes no positional arguments", 2)
	}
	// The legacy resolver warns and ignores malformed environment values.
	// This deliberately bounded preview refuses unsupported policy instead.
	if raw := os.Getenv(maxRowsEnvVar); raw != "" {
		maxRows, err := strconv.Atoi(raw)
		if err != nil || maxRows < 0 {
			return graphFailure("invalid_properties", "BEADS_MAX_ROWS must be a non-negative integer", 2)
		}
		if maxRows > 0 {
			return graphFailure("capability_unavailable", "graph ready preview does not implement a configured row cap; unset BEADS_MAX_ROWS to request the unfiltered view", 5)
		}
	}
	return withGraphStore(func(ctx context.Context, store *graphstore.Store) (any, string, error) {
		issues, err := store.ReadyIssues(ctx)
		if err != nil {
			return nil, "", err
		}
		var human strings.Builder
		for _, issue := range issues {
			fmt.Fprintf(&human, "%s  %s\n", issue.ID, issue.Properties.Title)
		}
		if len(issues) == 0 {
			human.WriteString("No ready Issues.\n")
		}
		return issues, strings.TrimSuffix(human.String(), "\n"), nil
	})
}
