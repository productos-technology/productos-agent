// Package cable is a minimal Action Cable client (the `actioncable-v1-json` subprotocol) over github.com/coder/websocket:
// connect with the bearer token, subscribe to one channel, perform actions, and deliver the channel's messages. It
// does not reconnect by itself: the daemon owns that loop.
package cable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ErrRejected: the server refused the subscription (unknown device, revoked, another person's).
var ErrRejected = errors.New("subscription rejected")

// ErrClosed: the socket went away.
var ErrClosed = errors.New("socket closed")

// Conn is one socket with one subscription.
type Conn struct {
	ws         *websocket.Conn
	identifier string
	messages   chan json.RawMessage
	done       chan struct{}
	err        error
	once       sync.Once
	writeMu    sync.Mutex
	lastPing   time.Time
	pingMu     sync.Mutex
}

// Options of Dial.
type Options struct {
	// Server is the HTTP origin, https://productos.technology; the socket URL is derived (wss://…/cable).
	Server string
	Token  string
	// Channel params, including "channel": the identifier of the subscription.
	Identifier map[string]any
	// HandshakeTimeout defaults to 15 s.
	HandshakeTimeout time.Duration
	// UserAgent header.
	UserAgent string
}

// CableURL turns https://host into wss://host/cable?token=….
func CableURL(server, token string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/cable"
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Dial connects, waits for `welcome`, subscribes and waits for the confirmation. The identifier's JSON encoding is
// fixed here and reused verbatim on every perform, as Action Cable requires.
func Dial(ctx context.Context, opts Options) (*Conn, error) {
	target, err := CableURL(opts.Server, opts.Token)
	if err != nil {
		return nil, err
	}
	timeout := opts.HandshakeTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	headers := http.Header{}
	headers.Set("Origin", strings.TrimRight(opts.Server, "/"))
	headers.Set("Authorization", "Bearer "+opts.Token)
	if opts.UserAgent != "" {
		headers.Set("User-Agent", opts.UserAgent)
	}
	ws, _, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{
		Subprotocols: []string{"actioncable-v1-json"},
		HTTPHeader:   headers,
	})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(16 << 20)

	ident, err := json.Marshal(opts.Identifier)
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "identifier")
		return nil, err
	}
	c := &Conn{ws: ws, identifier: string(ident), messages: make(chan json.RawMessage, 256), done: make(chan struct{})}

	// welcome
	if err := c.expect(dialCtx, "welcome"); err != nil {
		c.closeWith(err)
		return nil, err
	}
	if err := c.write(dialCtx, map[string]any{"command": "subscribe", "identifier": c.identifier}); err != nil {
		c.closeWith(err)
		return nil, err
	}
	if err := c.expect(dialCtx, "confirm_subscription"); err != nil {
		c.closeWith(err)
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

// expect reads frames until the given type arrives (pings are skipped; reject_subscription is ErrRejected).
func (c *Conn) expect(ctx context.Context, want string) error {
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		var frame struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &frame) != nil {
			continue
		}
		switch frame.Type {
		case want:
			return nil
		case "ping":
			continue
		case "reject_subscription":
			return ErrRejected
		case "disconnect":
			var d struct {
				Reason    string `json:"reason"`
				Reconnect bool   `json:"reconnect"`
			}
			_ = json.Unmarshal(data, &d)
			return fmt.Errorf("server disconnected: %s", d.Reason)
		}
	}
}

func (c *Conn) readLoop() {
	defer close(c.messages)
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.closeWith(err)
			return
		}
		var frame struct {
			Type       string          `json:"type"`
			Identifier string          `json:"identifier"`
			Message    json.RawMessage `json:"message"`
		}
		if json.Unmarshal(data, &frame) != nil {
			continue
		}
		switch frame.Type {
		case "ping":
			c.pingMu.Lock()
			c.lastPing = time.Now()
			c.pingMu.Unlock()
			continue
		case "disconnect":
			c.closeWith(ErrClosed)
			return
		case "":
			if frame.Identifier == c.identifier && len(frame.Message) > 0 {
				select {
				case c.messages <- frame.Message:
				default:
					// A full buffer means the daemon stalled; drop the oldest.
					select {
					case <-c.messages:
					default:
					}
					c.messages <- frame.Message
				}
			}
		}
	}
}

// Messages are the channel's broadcasts and transmissions, as raw JSON. Closed when the socket dies.
func (c *Conn) Messages() <-chan json.RawMessage { return c.messages }

// Done is closed when the socket dies; Err says why.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Err() error {
	<-c.done
	return c.err
}

// Perform sends {"command":"message","identifier":…,"data":"{\"action\":…}"}.
func (c *Conn) Perform(ctx context.Context, action string, data map[string]any) error {
	payload := map[string]any{"action": action}
	for k, v := range data {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.write(ctx, map[string]any{"command": "message", "identifier": c.identifier, "data": string(raw)})
}

func (c *Conn) write(ctx context.Context, frame any) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, raw)
}

// Close ends the socket politely.
func (c *Conn) Close() error {
	c.closeWith(ErrClosed)
	return nil
}

func (c *Conn) closeWith(err error) {
	c.once.Do(func() {
		c.err = err
		close(c.done)
		_ = c.ws.Close(websocket.StatusNormalClosure, "bye")
	})
}

// Stale reports whether no server ping arrived for the given duration (Action Cable pings every 3 s).
func (c *Conn) Stale(d time.Duration) bool {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	return !c.lastPing.IsZero() && time.Since(c.lastPing) > d
}
