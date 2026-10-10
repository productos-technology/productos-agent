package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/productos/productos-agent/internal/config"
)

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke this machine in ProductOS and forget the credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := newSession()
			if err != nil {
				return err
			}
			defer s.close()
			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			if s.cfg.DeviceID != "" {
				revokeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				if err := s.api.RevokeDevice(revokeCtx, s.cfg.DeviceID); err != nil {
					out(w, "The device could not be revoked on the server (%v); revoke it from Account, Devices.", err)
				} else {
					out(w, "Device revoked; its active runs were cancelled and its checkouts unlinked.")
				}
				cancel()
			}
			if tokens, err := s.tokens.Current(); err == nil && tokens.RefreshToken != "" {
				revokeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				_ = s.oauth.Revoke(revokeCtx, s.cfg.ClientID, tokens.RefreshToken, "refresh_token")
				cancel()
			}
			if err := s.store.Delete(); err != nil {
				return err
			}
			s.cfg.DeviceID = ""
			if err := config.Save(s.cfg); err != nil {
				return err
			}
			out(w, "Signed out.")
			return nil
		},
	}
}
