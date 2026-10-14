package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/productos-technology/productos-agent/internal/api"
	"github.com/productos-technology/productos-agent/internal/daemon"
	"github.com/productos-technology/productos-agent/internal/tools"
	"github.com/productos-technology/productos-agent/internal/update"
	"github.com/productos-technology/productos-agent/internal/version"
)

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the device, server, tools, linked checkouts and connection",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := newSession()
			if err != nil {
				return err
			}
			defer s.close()
			w := cmd.OutOrStdout()
			out(w, "Server:   %s", s.cfg.Server)
			invoked, exe, _ := update.Executable()
			method := update.DetectMethod(invoked, exe)
			out(w, "Version:  %s (%s install, %s)", version.String(), method, exe)
			tokens, err := s.tokens.Current()
			if err != nil {
				out(w, "Login:    not signed in (run `productos-agent login`)")
				return nil
			}
			where := "OS keychain"
			if s.store.FallbackUsed {
				where = "credentials file"
			}
			out(w, "Login:    signed in (%s; access token renews at %s)", where, tokens.ExpiresAt.Local().Format(time.Stamp))
			detected := tools.Detect(cmd.Context())
			out(w, "Tools:    %s", describeTools(detected))

			if state, _ := daemon.ReadState(); state != nil && state.Alive() {
				conn := "connecting"
				if state.Connected {
					conn = "connected"
				}
				out(w, "Daemon:   running (pid %d, %s, last heartbeat %s, %d active run(s))", state.PID, conn, ago(state.LastHeartbeat), len(state.ActiveRuns))
				if state.Error != "" {
					out(w, "          last error: %s", state.Error)
				}
			} else {
				out(w, "Daemon:   not running (start it with `productos-agent run` or `productos-agent service install`)")
			}

			if s.cfg.DeviceID == "" {
				out(w, "Device:   not registered")
				return nil
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			device, err := s.api.Heartbeat(ctx, s.cfg.DeviceID, api.HeartbeatInput{AgentVersion: version.String()})
			if err != nil {
				if api.IsCode(err, "not_found") {
					out(w, "Device:   %s was revoked in ProductOS; run `productos-agent login` again", s.cfg.DeviceID)
					return nil
				}
				out(w, "Device:   %s (server unreachable: %v)", s.cfg.DeviceID, err)
				return nil
			}
			out(w, "Device:   %s (%s, %s/%s)", device.Name, device.ID, device.Platform, device.Arch)
			if _, detail := updateLine(version.String(), device.LatestVersion, method); detail != "" {
				out(w, "Update:   %s", detail)
			}
			if device.Outdated {
				out(w, "          this version is below the server's minimum: runs cannot start until you update")
			}
			if len(device.Repositories) == 0 {
				out(w, "Links:    none (run `productos-agent link` inside a checkout)")
				return nil
			}
			out(w, "Links:")
			for _, link := range device.Repositories {
				out(w, "  %-40s %s  [%s] (%s)", link.FullName, link.LocalPath, link.DefaultBranch, link.Workspace.Slug)
			}
			return nil
		},
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	if d < time.Minute {
		return d.String() + " ago"
	}
	return d.Round(time.Minute).String() + " ago"
}
