package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	graph "github.com/steveyegge/beads/graphops"
	"github.com/steveyegge/beads/internal/storage/graphstore"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/uimd"
)

func runGraphPreviewComments(cmd *cobra.Command, args []string) error {
	if err := graphPreviewFlags(cmd, "local-time"); err != nil {
		return err
	}
	path, err := graphPreviewResourcePath(graphPreviewConfig.GraphScopeURL, args[0])
	if err != nil {
		return graphFailure("invalid_selector", err.Error(), 2)
	}
	if err := graph.ValidateBeadPath(path); err != nil {
		return graphFailure("invalid_selector", err.Error(), 2)
	}
	localTime, _ := cmd.Flags().GetBool("local-time")
	return withGraphStore(func(ctx context.Context, store *graphstore.Store) (any, string, error) {
		comments, err := store.IssueComments(ctx, path)
		if err != nil {
			return nil, "", err
		}
		if comments == nil {
			comments = []*types.Comment{}
		}
		if len(comments) == 0 {
			return comments, "No comments on " + path, nil
		}
		var out strings.Builder
		fmt.Fprintf(&out, "\nComments on %s:\n\n", path)
		for _, comment := range comments {
			ts := comment.CreatedAt
			if localTime {
				ts = ts.Local()
			}
			fmt.Fprintf(&out, "[%s] at %s\n", comment.Author, ts.Format("2006-01-02 15:04"))
			rendered := uimd.RenderMarkdown(comment.Text)
			for _, line := range strings.Split(strings.TrimRight(rendered, "\n"), "\n") {
				fmt.Fprintf(&out, "  %s\n", line)
			}
			out.WriteByte('\n')
		}
		return comments, strings.TrimRight(out.String(), "\n"), nil
	})
}
