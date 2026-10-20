package worktree

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/productos-technology/productos-agent/internal/gitx"
)

// IncludeFile names the gitignored files a fresh worktree needs from the person's checkout (`.env`, local
// settings), Claude Code's convention: one path or glob per line, relative to the checkout's root, `#` comments. Only
// paths git ignores are copied: tracked files are in the worktree already.
const IncludeFile = ".worktreeinclude"

// CopyIncluded copies the gitignored paths `.worktreeinclude` names from the checkout src into the worktree dst
// (files, whole directories, symlinks as symlinks). With onlyMissing, files already in dst stay as they are (a
// worktree reused by a later round keeps what it has). It returns how many files it copied.
func CopyIncluded(ctx context.Context, src *gitx.Repo, dst string, onlyMissing bool) (int, error) {
	patterns, err := readInclude(filepath.Join(src.Dir, IncludeFile))
	if err != nil || len(patterns) == 0 {
		return 0, err
	}
	copied := 0
	seen := map[string]bool{}
	var firstErr error
	for _, pattern := range patterns {
		matches := []string{filepath.Join(src.Dir, pattern)}
		if strings.ContainsAny(pattern, "*?[") {
			if matches, err = filepath.Glob(filepath.Join(src.Dir, pattern)); err != nil {
				continue
			}
		}
		for _, abs := range matches {
			rel, err := filepath.Rel(src.Dir, abs)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") || seen[rel] || isGitDir(rel) {
				continue
			}
			seen[rel] = true
			if _, err := os.Lstat(abs); err != nil || !src.IsIgnored(ctx, filepath.ToSlash(rel)) {
				continue
			}
			n, err := copyTree(abs, filepath.Join(dst, rel), onlyMissing)
			copied += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return copied, firstErr
}

// readInclude reads the patterns: blank lines, comments and negations (`!x`, not supported) are skipped; a leading
// `/` anchors to the root anyway; paths that climb out of the checkout are refused.
func readInclude(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		line = strings.TrimSuffix(strings.TrimLeft(line, "/"), "/")
		if line == "" || filepath.IsAbs(line) {
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(line))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			continue
		}
		out = append(out, clean)
	}
	return out, sc.Err()
}

func isGitDir(rel string) bool {
	first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	return first == ".git"
}

// copyTree copies a file, a symlink or a directory (recursively), creating parents; mode bits are kept.
func copyTree(src, dst string, onlyMissing bool) (int, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return copyEntry(src, dst, info, onlyMissing)
	}
	n := 0
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		c, err := copyEntry(path, target, info, onlyMissing)
		n += c
		return err
	})
	return n, err
}

func copyEntry(src, dst string, info fs.FileInfo, onlyMissing bool) (int, error) {
	if onlyMissing {
		if _, err := os.Lstat(dst); err == nil {
			return 0, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return 0, err
		}
		_ = os.Remove(dst)
		return 1, os.Symlink(target, dst)
	}
	if !info.Mode().IsRegular() {
		return 0, nil // sockets, devices: nothing a worktree needs
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return 0, err
	}
	return 1, out.Close()
}
