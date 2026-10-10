package cli

import (
	"github.com/spf13/cobra"

	"github.com/productos-technology/productos-agent/internal/service"
)

func serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install, remove, start or stop the background service",
		Long: `Keeps "productos-agent run" alive in the background as the current user: a systemd user unit on Linux, a launchd
agent on macOS, a logon task in the Task Scheduler on Windows. Sign in with "login" first.`,
	}
	cmd.AddCommand(
		&cobra.Command{Use: "install", Short: "Install and start the service", RunE: func(cmd *cobra.Command, args []string) error {
			s, err := newSession()
			if err != nil {
				return err
			}
			defer s.close()
			if err := s.requireLogin(); err != nil {
				return err
			}
			m, err := service.New()
			if err != nil {
				return err
			}
			bin, err := service.Binary()
			if err != nil {
				return err
			}
			path, err := m.Install(bin)
			if err != nil {
				return err
			}
			out(cmd.OutOrStdout(), "Installed: %s\n%s", path, m.Describe())
			return nil
		}},
		&cobra.Command{Use: "uninstall", Short: "Stop and remove the service", RunE: func(cmd *cobra.Command, args []string) error {
			m, err := service.New()
			if err != nil {
				return err
			}
			if err := m.Uninstall(); err != nil {
				return err
			}
			out(cmd.OutOrStdout(), "Removed.")
			return nil
		}},
		&cobra.Command{Use: "start", Short: "Start the service", RunE: func(cmd *cobra.Command, args []string) error {
			m, err := service.New()
			if err != nil {
				return err
			}
			return m.Start()
		}},
		&cobra.Command{Use: "stop", Short: "Stop the service", RunE: func(cmd *cobra.Command, args []string) error {
			m, err := service.New()
			if err != nil {
				return err
			}
			return m.Stop()
		}},
	)
	return cmd
}
