package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"
)

// CallbackResult is what the browser brought back to the loopback listener.
type CallbackResult struct {
	Code  string
	Error string
	Desc  string
}

// Loopback listens on 127.0.0.1 at a random port for the authorization response (RFC 8252 section 7.3).
type Loopback struct {
	listener net.Listener
	server   *http.Server
	state    string
	issuer   string
	results  chan CallbackResult
}

// Listen opens the port. RedirectURI() is what to send in the authorization request.
func Listen(state, issuer string) (*Loopback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("cannot open a loopback port: %w", err)
	}
	lb := &Loopback{listener: ln, state: state, issuer: issuer, results: make(chan CallbackResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", lb.handle)
	lb.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = lb.server.Serve(ln) }()
	return lb, nil
}

func (l *Loopback) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d/callback", l.listener.Addr().(*net.TCPAddr).Port)
}

func (l *Loopback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if q.Get("state") != l.state {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		l.deliver(CallbackResult{Error: "state_mismatch", Desc: "the state of the response does not match the request"})
		return
	}
	if iss := q.Get("iss"); iss != "" && l.issuer != "" && !sameIssuer(iss, l.issuer) {
		http.Error(w, "issuer mismatch", http.StatusBadRequest)
		l.deliver(CallbackResult{Error: "issuer_mismatch", Desc: "the response came from another issuer"})
		return
	}
	if e := q.Get("error"); e != "" {
		fmt.Fprint(w, page("Sign-in was not completed", "You can close this window and return to the terminal."))
		l.deliver(CallbackResult{Error: e, Desc: q.Get("error_description")})
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		l.deliver(CallbackResult{Error: "invalid_response", Desc: "the response has no code"})
		return
	}
	fmt.Fprint(w, page("ProductOS Agent is signed in", "You can close this window and return to the terminal."))
	l.deliver(CallbackResult{Code: code})
}

func (l *Loopback) deliver(r CallbackResult) {
	select {
	case l.results <- r:
	default:
	}
}

// Wait blocks until the browser comes back or ctx ends.
func (l *Loopback) Wait(ctx context.Context) (CallbackResult, error) {
	select {
	case r := <-l.results:
		return r, nil
	case <-ctx.Done():
		return CallbackResult{}, ctx.Err()
	}
}

// Close stops listening.
func (l *Loopback) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return l.server.Shutdown(ctx)
}

func sameIssuer(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ua.Scheme == ub.Scheme && ua.Host == ub.Host && trimSlash(ua.Path) == trimSlash(ub.Path)
}

func trimSlash(p string) string {
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}

func page(title, body string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>` + title + `</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#1b1b1b}h1{font-size:1.25rem}</style>
</head><body><h1>` + title + `</h1><p>` + body + `</p></body></html>`
}

// OpenBrowser tries the platform's opener; callers print the URL as well in case it fails.
func OpenBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return errors.New("xdg-open is not installed")
		}
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
