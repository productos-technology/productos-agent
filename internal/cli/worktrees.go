package cli

import (
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/productos-technology/productos-agent/internal/daemon"
	"github.com/productos-technology/productos-agent/internal/gitx"
	"github.com/productos-technology/productos-agent/internal/tools"
	"github.com/productos-technology/productos-agent/internal/worktree"
)

func worktreesCmd() *cobra.Command {
	var noFetch bool
	cmd := &cobra.Command{
		Use:   "worktrees",
		Short: "List the worktrees runs work in (one per change)",
		Long: `A run the server isolates works in its own git worktree under the agent's state directory, one per change, so
several changes run at once and your checkout is never touched. A worktree stays after its run: the next round of the
same change (Adjust) continues there. The list shows each worktree's path, branch, last use and whether its branch is
merged into origin's default branch. "worktrees prune" removes the merged ones and those unused for 14 days; the agent
does it by itself once a day when idle.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := worktree.Root()
			if err != nil {
				return err
			}
			opts := worktree.Options{Root: root, Fetch: !noFetch, GH: ghOnPath()}
			entries, err := worktree.List(cmd.Context(), opts)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(entries) == 0 {
				out(w, "No worktrees (%s).", root)
				return nil
			}
			busy := busyFilter()
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "PATH\tBRANCH\tLAST USED\tMERGED\tSTATE")
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Path, firstOr(e.Branch, "-"), ago(e.LastUsedAt), merged(e), entryState(e, busy(e)))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "do not fetch origin before telling merged branches")
	cmd.AddCommand(worktreesPruneCmd())
	return cmd
}

func worktreesPruneCmd() *cobra.Command {
	var force, dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove the worktrees whose branch is merged or that nobody used for 14 days",
		Long: `Removes the worktrees whose branch is merged into origin's default branch (a squash merge counts when gh is signed
in) and those no run used for 14 days, plus the leftovers of worktrees git forgot. A worktree a run is using is never
removed, nor one with uncommitted changes unless --force. Branches are never deleted: only the directories go.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := worktree.Root()
			if err != nil {
				return err
			}
			outcomes, err := worktree.Prune(cmd.Context(), worktree.Options{Root: root, Fetch: true, GH: ghOnPath(), Force: force, DryRun: dryRun, Skip: busyFilter()})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			removed := 0
			for _, o := range outcomes {
				switch {
				case o.Err != nil:
					out(w, "kept     %s (%s): %v", o.Path, firstOr(o.Branch, "-"), o.Err)
				case o.Removed && dryRun:
					removed++
					out(w, "would go %s (%s): %s", o.Path, firstOr(o.Branch, "-"), o.Reason)
				case o.Removed:
					removed++
					out(w, "removed  %s (%s): %s", o.Path, firstOr(o.Branch, "-"), o.Reason)
				default:
					out(w, "kept     %s (%s): %s", o.Path, firstOr(o.Branch, "-"), o.Reason)
				}
			}
			if len(outcomes) == 0 {
				out(w, "No worktrees (%s).", root)
			} else if dryRun {
				out(w, "%d of %d would be removed.", removed, len(outcomes))
			} else {
				out(w, "%d of %d removed.", removed, len(outcomes))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove worktrees with uncommitted changes too")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "say what would be removed, remove nothing")
	return cmd
}

// busyFilter reads the running daemon's state: a worktree one of its runs works in is never removed.
func busyFilter() func(worktree.Entry) bool {
	state, _ := daemon.ReadState()
	if state == nil || !state.Alive() {
		return func(worktree.Entry) bool { return false }
	}
	return func(e worktree.Entry) bool {
		if e.LastRunID != "" && slices.Contains(state.ActiveRuns, e.LastRunID) {
			return true
		}
		for _, dir := range state.Worktrees {
			if gitx.SamePath(dir, e.Path) {
				return true
			}
		}
		return false
	}
}

func ghOnPath() string {
	if p, ok := tools.Path(tools.GH); ok {
		return p
	}
	return ""
}

func merged(e worktree.Entry) string {
	switch {
	case e.Merged:
		return "yes"
	case e.MergedKnown:
		return "no"
	}
	return "?"
}

func entryState(e worktree.Entry, busy bool) string {
	var parts []string
	if busy {
		parts = append(parts, "in use")
	}
	if e.Missing {
		parts = append(parts, "directory gone")
	}
	if e.Orphan && !e.Missing {
		parts = append(parts, "left over")
	}
	if e.Dirty {
		parts = append(parts, "uncommitted changes")
	}
	if len(parts) == 0 {
		return "ok"
	}
	return strings.Join(parts, ", ")
}
