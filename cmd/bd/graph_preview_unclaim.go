package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	graph "github.com/steveyegge/beads/graphops"
	"github.com/steveyegge/beads/internal/storage/graphstore"
)

func runGraphPreviewUnclaim(cmd *cobra.Command, args []string) error {
	if err := graphPreviewWritePolicy(); err != nil {
		return err
	}
	if err := graphPreviewFlags(cmd, "force", "if-assignee", "reason"); err != nil {
		return err
	}
	if len(args) == 0 {
		return graphFailure("invalid_selector", "graph unclaim requires at least one Bead ID or beads/PATH", 2)
	}
	force, _ := cmd.Flags().GetBool("force")
	expected, _ := cmd.Flags().GetString("if-assignee")
	conditional := cmd.Flags().Changed("if-assignee")
	if conditional && expected == "" {
		return graphFailure("invalid_properties", "--if-assignee requires a nonempty assignee", 2)
	}
	if force && conditional {
		return graphFailure("invalid_properties", "--force and --if-assignee cannot be combined", 2)
	}
	reason, _ := cmd.Flags().GetString("reason")
	type selectedPath struct {
		path string
		err  error
	}
	paths := make([]selectedPath, len(args))
	for i, selector := range args {
		path, err := graphPreviewResourcePath(graphPreviewConfig.GraphScopeURL, selector)
		if err == nil {
			err = graph.ValidateBeadPath(path)
		}
		if err != nil && len(args) == 1 {
			return graphFailure("invalid_selector", err.Error(), 2)
		}
		paths[i] = selectedPath{path, err}
	}
	actor := getActorWithGit()
	var failures []string
	return withGraphStoreOutput(func(ctx context.Context, store *graphstore.Store) (any, string, error) {
		results := make([]graphstore.IssueMutationResult, 0, len(paths))
		var human []string
		for i, item := range paths {
			if item.err != nil {
				failures = append(failures, fmt.Sprintf("Error resolving %s: %v", args[i], item.err))
				continue
			}
			path := item.path
			result, err := store.UnclaimIssueWithPolicy(ctx, path, actor, force, expected, conditional)
			if err != nil {
				if len(paths) == 1 {
					return nil, "", err
				}
				failures = append(failures, fmt.Sprintf("Error unclaiming %s: %v", path, err))
				continue
			}
			if reason != "" {
				if err := store.AddIssueComment(ctx, path, actor, reason); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to add reason comment: %v\n", err)
				}
			}
			results = append(results, result)
			suffix := ""
			if reason != "" {
				suffix = ": " + reason
			}
			human = append(human, fmt.Sprintf("Unclaimed %s%s", result.Issue.ID, suffix))
		}
		if len(paths) == 1 {
			return results[0], human[0], nil
		}
		return results, strings.Join(human, "\n"), nil
	}, func(result any, human string) error {
		if len(paths) == 1 || len(result.([]graphstore.IssueMutationResult)) > 0 {
			if err := graphPrint(result, human, quietFlag); err != nil {
				return err
			}
		}
		for _, message := range failures {
			fmt.Fprintln(os.Stderr, message)
		}
		if len(failures) > 0 {
			return SilentExit()
		}
		return nil
	})
}
