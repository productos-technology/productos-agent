// Package gitx wraps the git command line for the checkout work of a run, and normalises remotes the way the server
// does (AgentDeviceRepository.normalize_remote).
package gitx

import (
	"regexp"
	"strings"
)

// Same expression as the server's GITHUB_REMOTE: optional scheme, optional user@, github.com, : or /, owner/name,
// optional .git, optional trailing slash.
var githubRemote = regexp.MustCompile(`(?i)^(?:(?:https?|ssh|git)://)?(?:[^@/\s]+@)?github\.com[:/]([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// NormalizeRemote returns `github.com/owner/name` (lowercase) for a GitHub remote, and "" with false otherwise.
func NormalizeRemote(raw string) (string, bool) {
	m := githubRemote.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", false
	}
	return "github.com/" + strings.ToLower(m[1]) + "/" + strings.ToLower(m[2]), true
}
