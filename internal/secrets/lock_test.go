package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockIsExclusiveUntilReleased(t *testing.T) {
	t.Setenv("PRODUCTOS_AGENT_CONFIG_DIR", t.TempDir())
	store := New("https://example.test")
	release, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := store.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Lock() = %v, want to wait until the context ends", err)
	}

	release()
	release2, err := store.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock() after release = %v", err)
	}
	release2()
}

func TestLockTakesOverAStaleOne(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PRODUCTOS_AGENT_CONFIG_DIR", dir)
	path := filepath.Join(dir, lockFile)
	if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	release, err := New("https://example.test").Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock() over a stale lock = %v", err)
	}
	release()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file still there after release: %v", err)
	}
}
