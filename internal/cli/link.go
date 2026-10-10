package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/productos-technology/productos-agent/internal/api"
	"github.com/productos-technology/productos-agent/internal/gitx"
)

func linkCmd() *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use:   "link [path]",
		Short: "Link a git checkout to the repository it belongs to in ProductOS",
		Long: `Reads the checkout's origin remote and default branch and registers it on this device. The remote must match a
GitHub repository connected to one of your workspaces; otherwise the command lists what ProductOS expected. Without a
path, the current directory is used.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := newSession()
			if err != nil {
				return err
			}
			defer s.close()
			if err := s.requireLogin(); err != nil {
				return err
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			repo, err := gitx.Open(dir)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			remote, err := repo.RemoteURL(ctx)
			if err != nil {
				return err
			}
			if branch == "" {
				branch, err = repo.DefaultBranch(ctx)
				if err != nil {
					return fmt.Errorf("%w; pass --branch", err)
				}
			}
			link, err := s.api.LinkRepository(ctx, s.cfg.DeviceID, api.LinkRepositoryInput{LocalPath: repo.Dir, RemoteURL: remote, DefaultBranch: branch})
			if err != nil {
				return explainLinkError(err, remote)
			}
			out(cmd.OutOrStdout(), "Linked %s to %s (%s, default branch %s).", repo.Dir, link.FullName, link.Workspace.Name, link.DefaultBranch)
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "the default branch runs start from (default: what origin says)")
	return cmd
}

func explainLinkError(err error, remote string) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "unknown_remote" {
		return err
	}
	var details api.UnknownRemote
	_ = json.Unmarshal(apiErr.Details, &details)
	var b strings.Builder
	normalized, isGitHub := gitx.NormalizeRemote(remote)
	if !isGitHub {
		fmt.Fprintf(&b, "the remote %q is not a GitHub remote, and ProductOS connects GitHub repositories only", remote)
	} else {
		fmt.Fprintf(&b, "no workspace of yours connects the repository %s", normalized)
	}
	if len(details.Expected) > 0 {
		b.WriteString(".\nProductOS was looking for one of:\n")
		for _, e := range details.Expected {
			fmt.Fprintf(&b, "  - %s\n", e)
		}
		b.WriteString("Connect the repository under Settings, Integrations, GitHub in the workspace, or run `link` in one of those checkouts")
	} else {
		b.WriteString(".\nNo workspace of yours connects a GitHub repository yet: connect one under Settings, Integrations, GitHub")
	}
	return errors.New(b.String())
}

func unlinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlink [path]",
		Short: "Unlink a checkout from this device",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := newSession()
			if err != nil {
				return err
			}
			defer s.close()
			if err := s.requireLogin(); err != nil {
				return err
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			repo, err := gitx.Open(dir)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			device, err := s.api.Heartbeat(ctx, s.cfg.DeviceID, api.HeartbeatInput{})
			if err != nil {
				return err
			}
			for _, link := range device.Repositories {
				if samePath(link.LocalPath, repo.Dir) {
					if err := s.api.UnlinkRepository(ctx, s.cfg.DeviceID, link.RepositoryID); err != nil {
						return err
					}
					out(cmd.OutOrStdout(), "Unlinked %s from %s.", repo.Dir, link.FullName)
					return nil
				}
			}
			return fmt.Errorf("%s is not linked on this device", repo.Dir)
		},
	}
}

func samePath(a, b string) bool {
	ra, errA := os.Stat(a)
	rb, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(ra, rb)
	}
	return strings.TrimRight(a, "/\\") == strings.TrimRight(b, "/\\")
}
