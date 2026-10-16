package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func fakeGHBin(t *testing.T, authExit string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\nif [ \"$1\" = \"auth\" ]; then exit " + authExit + "; fi\necho 'gh version 2.60.1 (2024-10-01)'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRODUCTOS_AGENT_GH_BIN", bin)
}

func TestGHIsAvailableOnlyWhenSignedIn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	fakeGHBin(t, "0")
	if got := Detect(context.Background())[GH]; !got.Available || got.Version != "2.60.1" {
		t.Errorf("signed in: %+v", got)
	}
	fakeGHBin(t, "1")
	if got := Detect(context.Background())[GH]; got.Available || got.Path == "" {
		t.Errorf("signed out: %+v", got)
	}
}
