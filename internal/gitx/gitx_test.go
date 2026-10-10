package gitx

import "testing"

func TestNormalizeRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:Acme/Mobile.git":        "github.com/acme/mobile",
		"https://github.com/Acme/Mobile.git/":   "github.com/acme/mobile",
		"https://github.com/acme/mobile":        "github.com/acme/mobile",
		"ssh://git@github.com/Acme/Mobile":      "github.com/acme/mobile",
		"git://github.com/acme/mobile.git":      "github.com/acme/mobile",
		"github.com/Acme/Mobile":                "github.com/acme/mobile",
		"  https://GitHub.com/acme/my.repo.git": "github.com/acme/my.repo",
		"https://user:token@github.com/a/b.git": "github.com/a/b",
	}
	for in, want := range cases {
		got, ok := NormalizeRemote(in)
		if !ok || got != want {
			t.Errorf("NormalizeRemote(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "git@gitlab.com:acme/mobile.git", "https://github.com/acme", "/Users/pat/origin.git", "https://github.com/acme/mobile/tree/main"} {
		if got, ok := NormalizeRemote(in); ok {
			t.Errorf("NormalizeRemote(%q) = %q, want no match", in, got)
		}
	}
}

func TestBranchName(t *testing.T) {
	cases := [][3]string{
		{"MOB-P7", "Offline drafts are lost", "productos/mob-p7-offline-drafts-are-lost"},
		{"MOB-P7", "", "productos/mob-p7"},
		{"WEB-P12", "Ação rápida: exportação (CSV) — v2", "productos/web-p12-acao-rapida-exportacao-csv-v2"},
		{"X-P1", "   many    spaces   ", "productos/x-p1-many-spaces"},
	}
	for _, c := range cases {
		if got := BranchName(c[0], c[1]); got != c[2] {
			t.Errorf("BranchName(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	long := BranchName("MOB-P7", "a very long title that keeps going and going well past the sixty character limit of the slug")
	if len(long) > len("productos/mob-p7-")+60 {
		t.Errorf("too long: %q", long)
	}
}

func TestValidBranch(t *testing.T) {
	for _, ok := range []string{"productos/mob-p7-x", "main", "feature/a.b-c_d"} {
		if !ValidBranch(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-x", "a..b", "a b", "a//b", "x.lock", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b", "trailing/"} {
		if ValidBranch(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestSplitRename(t *testing.T) {
	from, to, ok := splitRename("src/{old => new}/file.ts")
	if !ok || from != "src/old/file.ts" || to != "src/new/file.ts" {
		t.Errorf("got %q %q %v", from, to, ok)
	}
	from, to, ok = splitRename("a.txt => b.txt")
	if !ok || from != "a.txt" || to != "b.txt" {
		t.Errorf("got %q %q %v", from, to, ok)
	}
	if _, _, ok := splitRename("plain.txt"); ok {
		t.Error("plain path is not a rename")
	}
}
