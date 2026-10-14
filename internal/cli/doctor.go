package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/productos-technology/productos-agent/internal/api"
	"github.com/productos-technology/productos-agent/internal/paths"
	"github.com/productos-technology/productos-agent/internal/tools"
	"github.com/productos-technology/productos-agent/internal/update"
	"github.com/productos-technology/productos-agent/internal/version"
)

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the tools on PATH, git, gh, the login and the server",
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			ok := func(label string, good bool, detail string) {
				mark := "ok  "
				if !good {
					mark = "FAIL"
				}
				out(w, "[%s] %-14s %s", mark, label, detail)
			}
			out(w, "productos-agent %s on %s/%s", version.String(), runtime.GOOS, runtime.GOARCH)
			invoked, exe, _ := update.Executable()
			method := update.DetectMethod(invoked, exe)
			out(w, "       install:   %s (%s)", method, exe)
			ctx := cmd.Context()
			detected := tools.Detect(ctx)
			for _, name := range tools.Known {
				info := detected[name]
				detail := "not found on PATH"
				if info.Available {
					detail = info.Path
					if info.Version != "" {
						detail += " (" + info.Version + ")"
					}
				}
				ok(name, info.Available, detail)
			}
			gitPath, err := exec.LookPath("git")
			ok("git", err == nil, firstOr(gitPath, "not found: git is required"))
			ghPath, err := exec.LookPath("gh")
			ok("gh", err == nil, firstOr(ghPath, "not found: pull requests will not be opened"))
			if dir, err := paths.ConfigDir(); err == nil {
				out(w, "       config:    %s", dir)
			}
			if f, err := paths.LogFile(); err == nil {
				out(w, "       log:       %s", f)
			}

			s, err := newSession()
			if err != nil {
				return err
			}
			defer s.close()
			out(w, "       server:    %s", s.cfg.Server)
			if _, err := s.tokens.Current(); err != nil {
				ok("login", false, "not signed in: run `productos-agent login`")
				return nil
			}
			if s.store.FallbackUsed {
				ok("keychain", false, "credentials in a file (the OS keychain was not reachable)")
			} else {
				ok("keychain", true, "credentials in the OS keychain")
			}
			if s.cfg.DeviceID == "" {
				ok("device", false, "not registered")
				return nil
			}
			reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			device, err := s.api.Heartbeat(reqCtx, s.cfg.DeviceID, api.HeartbeatInput{AgentVersion: version.String()})
			switch {
			case err == nil:
				ok("device", true, device.Name+" ("+device.ID+")")
				ok("links", len(device.Repositories) > 0, pluralLinks(len(device.Repositories)))
				ok("version", !device.Outdated, versionDetail(device.Outdated))
				good, detail := updateLine(version.String(), device.LatestVersion, method)
				ok("update", good, detail)
			case api.IsCode(err, "not_found"):
				ok("device", false, "revoked in ProductOS; run `productos-agent login`")
			default:
				ok("server", false, err.Error())
			}
			return nil
		},
	}
}

func versionDetail(outdated bool) string {
	if outdated {
		return version.String() + " is below the server's minimum: runs cannot start until you update"
	}
	return version.String()
}

func firstOr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func pluralLinks(n int) string {
	switch n {
	case 0:
		return "no checkout linked; run `productos-agent link` inside one"
	case 1:
		return "1 checkout linked"
	}
	return fmt.Sprintf("%d checkouts linked", n)
}
