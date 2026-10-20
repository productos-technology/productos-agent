package secrets

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/productos-technology/productos-agent/internal/paths"
)

const (
	lockFile  = "credentials.lock"
	lockWait  = 15 * time.Second       // how long a process waits for another's refresh to finish
	lockStale = 30 * time.Second       // a lock older than this was left by a crash and is taken over
	lockPoll  = 100 * time.Millisecond // how often a waiting process looks again
)

// ErrLocked: another productos-agent process held the credentials for longer than lockWait.
var ErrLocked = errors.New("another productos-agent process is renewing the credentials; try again")

// Lock serialises token refreshes across the productos-agent processes of this machine (the daemon and a CLI command
// run beside it): a refresh token works once, so whoever holds the lock spends it and saves the new pair, and the
// others wait, then read what was saved. The lock is a file created exclusively under the config directory (portable,
// no OS primitive needed); it is released by the returned function. A lock older than lockStale is a crash's
// leftover and is taken over.
func (s *Store) Lock(ctx context.Context) (release func(), err error) {
	dir, err := paths.ConfigDir()
	if err != nil {
		return nil, err
	}
	if err := paths.Ensure(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, lockFile)
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("cannot lock the credentials: %w", err)
		}
		if info, serr := os.Stat(path); serr == nil && time.Since(info.ModTime()) > lockStale {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, ErrLocked
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}
