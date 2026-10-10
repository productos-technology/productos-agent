// Package cli is the cobra command tree of productos-agent.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/productos-technology/productos-agent/internal/api"
	"github.com/productos-technology/productos-agent/internal/config"
	"github.com/productos-technology/productos-agent/internal/logx"
	"github.com/productos-technology/productos-agent/internal/oauth"
	"github.com/productos-technology/productos-agent/internal/secrets"
	"github.com/productos-technology/productos-agent/internal/version"
)

var verbose bool

// Execute runs the CLI.
func Execute() int {
	root := &cobra.Command{
		Use:   "productos-agent",
		Short: "Runs ProductOS change definitions in your own checkouts with Claude Code or Codex",
		Long: `The ProductOS Agent binds this machine to your ProductOS account. Sign in once, link the git checkouts of the
repositories your workspaces connect, keep it running, and press Run in the Deliver dialog of a pain: the change
definition is executed here, streamed to ProductOS, committed to a branch and opened as a pull request.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "log to stderr as well as the log file")
	root.AddCommand(loginCmd(), logoutCmd(), statusCmd(), linkCmd(), unlinkCmd(), runCmd(), serviceCmd(), versionCmd(), doctorCmd())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var exit *exitError
		if errors.As(err, &exit) {
			return exit.code
		}
		return 1
	}
	return 0
}

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// session is what most commands need: config, OAuth client, secrets, token source and the REST client.
type session struct {
	cfg    *config.Config
	oauth  *oauth.Client
	store  *secrets.Store
	tokens *oauth.TokenSource
	api    *api.Client
	log    *slog.Logger
	close  func()
}

func newSession() (*session, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	log, closer, err := logx.Setup(verbose)
	if err != nil {
		return nil, err
	}
	oc := oauth.New(cfg.Server)
	store := secrets.New(oc.Server)
	tokens := oauth.NewTokenSource(oc, cfg.ClientID, store)
	return &session{cfg: cfg, oauth: oc, store: store, tokens: tokens, api: api.New(oc.Server, tokens), log: log, close: closer}, nil
}

// requireLogin fails early with a clear message when nobody is signed in.
func (s *session) requireLogin() error {
	if _, err := s.tokens.Current(); err != nil {
		return oauth.ErrSignedOut
	}
	if s.cfg.DeviceID == "" {
		return errors.New("this machine is not registered: run `productos-agent login`")
	}
	return nil
}

func out(w io.Writer, format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, args []string) {
			out(cmd.OutOrStdout(), "productos-agent %s (%s, %s)", version.Version, version.Commit, version.Date)
		},
	}
}
