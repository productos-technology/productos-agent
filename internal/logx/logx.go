// Package logx sets up structured logging: JSON lines to the state directory's agent.log (rotated at 10 MB), and a
// human text handler on stderr when --verbose. Callers must never log tokens or the change definition body.
package logx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/productos/productos-agent/internal/paths"
)

const maxSize = 10 << 20

// Setup returns the logger and a closer for the file.
func Setup(verbose bool) (*slog.Logger, func(), error) {
	handlers := []slog.Handler{}
	closer := func() {}
	if path, err := paths.LogFile(); err == nil {
		if err := paths.Ensure(filepath.Dir(path)); err == nil {
			rotate(path)
			f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err == nil {
				handlers = append(handlers, slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
				closer = func() { _ = f.Close() }
			}
		}
	}
	level := slog.LevelWarn
	if verbose {
		level = slog.LevelDebug
	}
	handlers = append(handlers, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	return slog.New(multi(handlers)), closer, nil
}

func rotate(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < maxSize {
		return
	}
	_ = os.Rename(path, path+".1")
}

// Discard is a logger that drops everything (tests).
func Discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type multi []slog.Handler

func (m multi) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m multi) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (m multi) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(multi, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (m multi) WithGroup(name string) slog.Handler {
	out := make(multi, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}
