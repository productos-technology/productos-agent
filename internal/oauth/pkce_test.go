package oauth

import (
	"regexp"
	"testing"
)

func TestVerifierAndChallenge(t *testing.T) {
	v, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`).MatchString(v) {
		t.Errorf("verifier %q is not RFC 7636 shaped", v)
	}
	// RFC 7636 appendix B example.
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	if got := Challenge(verifier); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("Challenge = %q", got)
	}
	if Challenge(v) == Challenge(v+"x") {
		t.Error("challenge must depend on the verifier")
	}
	s1, _ := NewState()
	s2, _ := NewState()
	if s1 == s2 || len(s1) < 16 {
		t.Error("states must be random")
	}
}

func TestNormalizeServer(t *testing.T) {
	cases := map[string]string{
		"productos.technology":          "https://productos.technology",
		"https://productos.technology/": "https://productos.technology",
		" http://localhost:3000/ ":      "http://localhost:3000",
	}
	for in, want := range cases {
		if got := NormalizeServer(in); got != want {
			t.Errorf("NormalizeServer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthorizeURL(t *testing.T) {
	c := New("https://productos.technology")
	meta := &Metadata{AuthorizationEndpoint: "https://productos.technology/oauth/authorize"}
	u := c.AuthorizeURL(meta, "pcl_1", "http://127.0.0.1:4242/callback", "st", "ch")
	for _, want := range []string{"response_type=code", "client_id=pcl_1", "code_challenge=ch", "code_challenge_method=S256", "scope=agent", "resource=https%3A%2F%2Fproductos.technology%2Fmcp", "state=st", "redirect_uri=http%3A%2F%2F127.0.0.1%3A4242%2Fcallback"} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(u) {
			t.Errorf("%q lacks %q", u, want)
		}
	}
}
