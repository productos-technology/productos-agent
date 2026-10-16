package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/productos-technology/productos-agent/internal/api"
	"github.com/productos-technology/productos-agent/internal/daemon"
	"github.com/productos-technology/productos-agent/internal/service"
	"github.com/productos-technology/productos-agent/internal/update"
	"github.com/productos-technology/productos-agent/internal/version"
)

func updateCmd() *cobra.Command {
	var check bool
	var to string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update the agent to the latest release (or to --to vX.Y.Z)",
		Long: `Downloads checksums.txt, its release signature and the archive over HTTPS, verifies the signature with the key
compiled into this binary and the SHA-256, tries the new binary and swaps it in place, then restarts the background service if one is installed. The latest version comes from the
running agent or the ProductOS server, else from GitHub's latest release. Homebrew and winget installs are updated
by their package manager: the command prints what to run. The daemon updates itself (before a run, or when idle)
unless "auto_update": false is in config.json or PRODUCTOS_AGENT_NO_UPDATE=1.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd.Context(), cmd.OutOrStdout(), check, strings.TrimSpace(to))
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether an update is available")
	cmd.Flags().StringVar(&to, "to", "", "install this version (vX.Y.Z) instead of the latest, to roll back or pin")
	return cmd
}

func runUpdate(ctx context.Context, w io.Writer, check bool, to string) error {
	invoked, exe, err := update.Executable()
	if err != nil {
		return err
	}
	method := update.DetectMethod(invoked, exe)
	current := version.String()
	target, base, source := to, "", "--to"
	if to != "" {
		if !update.Valid(to) {
			return fmt.Errorf("%q is not a version (vX.Y.Z)", to)
		}
		if state, _ := daemon.ReadState(); state != nil && state.Agent != nil {
			base = state.Agent.DownloadBase
		}
	} else {
		target, base, source = latestRelease(ctx)
		if target == "" {
			return errors.New("cannot tell the latest release (neither ProductOS nor GitHub answered); pass --to vX.Y.Z")
		}
	}
	out(w, "Installed: v%s (%s install, %s)", update.Number(current), method, exe)
	out(w, "Latest:    v%s (from %s)", update.Number(target), source)
	switch c := update.Compare(current, target); {
	case c == 0:
		out(w, "Up to date.")
		return nil
	case c > 0 && to == "":
		out(w, "This build is newer than the latest release.")
		return nil
	}
	if method != update.MethodSelf {
		out(w, "This copy is managed by %s; update it with: %s", method, method.Command())
		return nil
	}
	if check {
		out(w, "An update is available: run `productos-agent update`.")
		return nil
	}
	out(w, "Downloading v%s for this machine…", update.Number(target))
	staged, err := update.New(base, exe).Stage(ctx, target)
	if err != nil {
		return err
	}
	if err := update.Replace(staged, exe); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("cannot replace %s: %w", exe, err)
	}
	out(w, "Updated %s to v%s.", exe, update.Number(target))
	restartAfterUpdate(w, exe)
	return nil
}

// latestRelease asks, in order: the running daemon (the server's block it last heard), the server (the device view's
// latest_version, through a heartbeat), GitHub's latest-release redirect.
func latestRelease(ctx context.Context) (latest, base, source string) {
	if state, _ := daemon.ReadState(); state != nil && state.Alive() && state.Agent != nil && update.Valid(state.Agent.LatestVersion) {
		return state.Agent.LatestVersion, state.Agent.DownloadBase, "ProductOS, through the running agent"
	}
	if s, err := newSession(); err == nil {
		defer s.close()
		if s.requireLogin() == nil {
			reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			device, err := s.api.Heartbeat(reqCtx, s.cfg.DeviceID, api.HeartbeatInput{AgentVersion: version.String()})
			cancel()
			if err == nil && device.LatestVersion != nil && update.Valid(*device.LatestVersion) {
				return *device.LatestVersion, "", "ProductOS (" + s.cfg.Server + ")"
			}
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if v, err := update.Latest(reqCtx, nil, update.LatestURL); err == nil {
		return v, "", "GitHub"
	}
	return "", "", ""
}

// restartAfterUpdate restarts the installed service so it runs the new binary: only when the service runs the binary
// that was replaced, and not while runs are in progress (the daemon then updates itself when idle).
func restartAfterUpdate(w io.Writer, exe string) {
	state, _ := daemon.ReadState()
	alive := state != nil && state.Alive()
	m, err := service.New()
	if err != nil || !m.Installed() {
		if alive {
			out(w, "An agent is running (pid %d): restart `productos-agent run` to use the new version.", state.PID)
		}
		return
	}
	if svc := m.Binary(); svc == "" || !sameFile(svc, exe) {
		if svc != "" {
			out(w, "The background service runs %s, not this binary: it was not restarted.", svc)
		} else {
			out(w, "Restart the background service to use the new version: `productos-agent service stop` and `service start`.")
		}
		return
	}
	if alive && len(state.ActiveRuns) > 0 {
		out(w, "The service has %d run(s) in progress; it picks up the new version once they finish (or run `productos-agent service stop` and `service start`).", len(state.ActiveRuns))
		return
	}
	if err := m.Restart(); err != nil {
		out(w, "Could not restart the service (%v): run `productos-agent service stop` and `productos-agent service start`.", err)
		return
	}
	out(w, "Restarted the service.")
}

// sameFile follows symlinks (Homebrew's opt link, a PATH alias) and compares the files.
func sameFile(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// updateLine is the doctor/status verdict about the version: whether this build is current and what to run if not.
func updateLine(current string, latest *string, method update.Method) (good bool, detail string) {
	switch {
	case update.IsDev(current):
		return true, "development build (never updates itself)"
	case latest == nil || !update.Valid(*latest):
		return true, "the server does not know the latest release yet"
	case !update.Behind(current, *latest):
		return true, "up to date (latest v" + update.Number(*latest) + ")"
	}
	how := "`productos-agent update`"
	if method != update.MethodSelf {
		how = "`" + method.Command() + "`"
	}
	return false, fmt.Sprintf("v%s is available: run %s", update.Number(*latest), how)
}
