package gitx

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	nonSlug   = regexp.MustCompile(`[^a-z0-9]+`)
	multiDash = regexp.MustCompile(`-{2,}`)
)

// Slug lowercases, strips accents' marks where possible and keeps a-z0-9 joined by single dashes, at most max chars.
func Slug(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case unicode.IsLetter(r):
			if folded, ok := asciiFold[r]; ok {
				b.WriteString(folded)
			} else {
				b.WriteByte('-')
			}
		default:
			b.WriteByte('-')
		}
	}
	out := multiDash.ReplaceAllString(nonSlug.ReplaceAllString(b.String(), "-"), "-")
	out = strings.Trim(out, "-")
	if max > 0 && len(out) > max {
		out = strings.Trim(out[:max], "-")
	}
	return out
}

// BranchName is the fallback when the server sends no branch: productos/<key>-<slug of title>.
func BranchName(key, title string) string {
	k := Slug(key, 40)
	t := Slug(title, 60)
	if t == "" {
		return "productos/" + k
	}
	return "productos/" + k + "-" + t
}

// ValidBranch rejects names git refuses (a cheap check; git has the final word).
func ValidBranch(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	return true
}

var asciiFold = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'æ': "ae",
	'ç': "c", 'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ì': "i", 'í': "i", 'î': "i", 'ï': "i",
	'ñ': "n", 'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o", 'œ': "oe",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ý': "y", 'ÿ': "y", 'ß': "ss",
}
