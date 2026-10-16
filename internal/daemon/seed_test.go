package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/productos-technology/productos-agent/internal/api"
	"github.com/productos-technology/productos-agent/internal/config"
	"github.com/productos-technology/productos-agent/internal/logx"
	"github.com/productos-technology/productos-agent/internal/protocol"
)

// seed.plant over the socket: the daemon fetches the seed, answers planting, plants it and reports planted.
func TestSeedPlantIsDispatchedAndReported(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	isolate(t)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	dir := filepath.Join(t.TempDir(), "acme")

	f := newFakeServer(t)
	f.features = []string{"seed"}
	f.seedReports = make(chan protocol.SeedStatusReport, 8)
	seed := &protocol.Seed{ID: "seed-1", Kind: "prompt", LocalPath: dir, Status: "pending", Files: []protocol.SeedFile{{Path: "README.md", Content: "# Acme\n"}}}
	seed.Commit.Message = "Start Acme"
	seed.Commit.Trailer = "ProductOS: MOB"
	f.seed = seed

	client := api.New(f.srv.URL, staticTokens{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	device, err := client.RegisterDevice(ctx, api.RegisterDeviceInput{Name: "test-box", Platform: "linux", Arch: "amd64", AgentVersion: "0.0.0-test"})
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Config: &config.Config{Server: f.srv.URL, DeviceID: device.ID}, API: client, Tokens: staticTokens{}, Log: logx.Discard(),
		Detect: func(context.Context) protocol.Tools { return protocol.Tools{} }}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	var got []protocol.SeedStatusReport
	for len(got) < 2 {
		select {
		case r := <-f.seedReports:
			got = append(got, r)
		case <-ctx.Done():
			t.Fatalf("reports so far: %+v", got)
		}
	}
	cancel()
	<-done
	if got[0].Status != protocol.SeedPlanting {
		t.Errorf("first report = %+v, want planting", got[0])
	}
	final := got[1]
	if final.Status != protocol.SeedPlanted || final.DefaultBranch != "main" || len(final.HeadCommit) < 7 {
		t.Fatalf("final = %+v", final)
	}
	if real, _ := filepath.EvalSymlinks(final.LocalPath); real == "" {
		t.Errorf("local_path %q not absolute/existing", final.LocalPath)
	}
	msg := git(t, dir, "log", "-1", "--format=%B")
	if !strings.HasSuffix(strings.TrimSpace(msg), "ProductOS: MOB") {
		t.Errorf("commit message = %q", msg)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Error(err)
	}
}
