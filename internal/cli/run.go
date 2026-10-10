package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/productos/productos-agent/internal/daemon"
)

func runCmd() *cobra.Command {
	var once bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Stay connected to ProductOS and execute runs on this machine (foreground)",
		Long: `Keeps this device online: connects to ProductOS, heartbeats every 30 seconds, and executes the runs you start from
the Deliver dialog in the checkouts linked here. Stop with Ctrl-C. Use "service install" to keep it running in the
background.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := newSession()
			if err != nil {
				return err
			}
			defer s.close()
			if err := s.requireLogin(); err != nil {
				return err
			}
			if state, _ := daemon.ReadState(); state != nil && state.Alive() {
				return fmt.Errorf("another agent is already running (pid %d)", state.PID)
			}
			d := &daemon.Daemon{Config: s.cfg, API: s.api, Tokens: s.tokens, Log: s.log, Once: once}
			err = d.Run(cmd.Context())
			if errors.Is(err, daemon.ErrDeviceRevoked) {
				return &exitError{code: 3, err: err}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "exit after the first run finishes")
	return cmd
}
