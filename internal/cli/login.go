package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hamdan85/productos-agent/internal/api"
	"github.com/Hamdan85/productos-agent/internal/config"
	"github.com/Hamdan85/productos-agent/internal/oauth"
	"github.com/Hamdan85/productos-agent/internal/protocol"
	"github.com/Hamdan85/productos-agent/internal/secrets"
	"github.com/Hamdan85/productos-agent/internal/tools"
	"github.com/Hamdan85/productos-agent/internal/version"
)

func loginCmd() *cobra.Command {
	var server, name string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login [server]",
		Short: "Sign in to ProductOS and register this machine",
		Long: `Opens your browser on ProductOS to approve the ProductOS Agent (OAuth 2.1 with PKCE, scope "agent"), keeps the
tokens in the OS keychain and registers this machine as one of your devices. Pass the server as an argument or with
--server; the default is ` + config.DefaultServer + `.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				server = args[0]
			}
			return login(cmd.Context(), server, name, noBrowser, cmd)
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "ProductOS server URL (default "+config.DefaultServer+")")
	cmd.Flags().StringVar(&name, "name", "", "how this machine is listed in ProductOS (default: the hostname)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the URL instead of opening the browser")
	return cmd
}

func login(ctx context.Context, server, name string, noBrowser bool, cmd *cobra.Command) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if server != "" {
		cfg.Server = oauth.NormalizeServer(server)
	}
	if cfg.Server == "" {
		cfg.Server = config.DefaultServer
	}
	oc := oauth.New(cfg.Server)
	w := cmd.OutOrStdout()
	out(w, "Signing in to %s", oc.Server)
	meta, err := oc.Discover(ctx)
	if err != nil {
		return err
	}
	store := secrets.New(oc.Server)

	// A previous sign-in on this machine: revoke it so the old device does not linger.
	if cfg.DeviceID != "" {
		old := oauth.NewTokenSource(oc, cfg.ClientID, store)
		revokeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = api.New(oc.Server, old).RevokeDevice(revokeCtx, cfg.DeviceID)
		cancel()
		cfg.DeviceID = ""
	}

	clientID := cfg.ClientID
	if clientID == "" {
		clientID, err = oc.Register(ctx)
		if err != nil {
			return err
		}
	}

	tokens, err := authorize(ctx, oc, meta, clientID, noBrowser, cmd)
	var te *oauth.TokenError
	if errors.As(err, &te) && te.Code == "invalid_client" && cfg.ClientID != "" {
		// The registration was purged: register again, once.
		out(w, "The previous client registration expired; registering again.")
		if clientID, err = oc.Register(ctx); err != nil {
			return err
		}
		tokens, err = authorize(ctx, oc, meta, clientID, noBrowser, cmd)
	}
	if err != nil {
		return err
	}
	if err := store.Save(tokens); err != nil {
		return fmt.Errorf("could not store the credentials: %w", err)
	}
	cfg.ClientID = clientID
	if name != "" {
		cfg.DeviceName = name
	}
	if err := config.Save(cfg); err != nil {
		return err
	}

	// Register the device right away so `link` and the Account page work before the first `run`.
	detected := tools.Detect(ctx)
	source := oauth.NewTokenSource(oc, clientID, store)
	client := api.New(oc.Server, source)
	deviceName := cfg.DeviceName
	if deviceName == "" {
		deviceName, _ = os.Hostname()
		if deviceName == "" {
			deviceName = "my machine"
		}
	}
	device, err := client.RegisterDevice(ctx, api.RegisterDeviceInput{Name: deviceName, Platform: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: version.String(), Tools: detected})
	if err != nil {
		return fmt.Errorf("signed in, but the device could not be registered: %w", err)
	}
	cfg.DeviceID = device.ID
	if err := config.Save(cfg); err != nil {
		return err
	}
	out(w, "Signed in. This machine is registered as %q (%s).", device.Name, device.ID)
	if store.FallbackUsed {
		out(w, "Credentials are in a 0600 file under the config directory because the OS keychain was not reachable.")
	}
	out(w, "Tools: %s", describeTools(detected))
	out(w, "Next: run `productos-agent link` inside a checkout, then `productos-agent run` (or `service install`).")
	return nil
}

func authorize(ctx context.Context, oc *oauth.Client, meta *oauth.Metadata, clientID string, noBrowser bool, cmd *cobra.Command) (secrets.Tokens, error) {
	verifier, err := oauth.NewVerifier()
	if err != nil {
		return secrets.Tokens{}, err
	}
	state, err := oauth.NewState()
	if err != nil {
		return secrets.Tokens{}, err
	}
	lb, err := oauth.Listen(state, meta.Issuer)
	if err != nil {
		return secrets.Tokens{}, err
	}
	defer lb.Close()
	target := oc.AuthorizeURL(meta, clientID, lb.RedirectURI(), state, oauth.Challenge(verifier))
	w := cmd.OutOrStdout()
	if noBrowser {
		out(w, "Open this URL in your browser to approve the agent:\n\n  %s\n", target)
	} else if err := oauth.OpenBrowser(target); err != nil {
		out(w, "Could not open a browser (%v). Open this URL yourself:\n\n  %s\n", err, target)
	} else {
		out(w, "Your browser is open on ProductOS. Approve the ProductOS Agent to continue.\nIf nothing opened, use this URL:\n\n  %s\n", target)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	result, err := lb.Wait(waitCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return secrets.Tokens{}, errors.New("no answer from the browser within 10 minutes")
		}
		return secrets.Tokens{}, err
	}
	if result.Error != "" {
		if result.Error == "access_denied" {
			return secrets.Tokens{}, errors.New("you cancelled the authorization")
		}
		return secrets.Tokens{}, fmt.Errorf("authorization failed: %s %s", result.Error, result.Desc)
	}
	return oc.Exchange(ctx, clientID, result.Code, verifier, lb.RedirectURI())
}

func describeTools(t protocol.Tools) string {
	parts := []string{}
	for _, name := range tools.Known {
		info := t[name]
		switch {
		case info.Available && info.Version != "":
			parts = append(parts, name+" "+info.Version)
		case info.Available:
			parts = append(parts, name)
		default:
			parts = append(parts, name+" (not found)")
		}
	}
	return strings.Join(parts, ", ")
}
