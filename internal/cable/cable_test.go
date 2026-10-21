package cable

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Action Cable transmits what a channel sends inside `subscribed` (the server's hello) BEFORE the confirmation frame.
// The client must deliver those messages, not drop them while it waits for `confirm_subscription`.
func TestDialDeliversMessagesTransmittedBeforeTheConfirmation(t *testing.T) {
	ident := map[string]any{"channel": "AgentChannel", "device_id": "dev-1"}
	identJSON, _ := json.Marshal(ident)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"actioncable-v1-json"}})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		write := func(v any) {
			b, _ := json.Marshal(v)
			if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
				t.Errorf("write: %v", err)
			}
		}
		write(map[string]any{"type": "welcome"})
		// The client subscribes; read its frame before answering, like the server does.
		if _, _, err := ws.Read(ctx); err != nil {
			t.Errorf("read subscribe: %v", err)
			return
		}
		write(map[string]any{"type": "ping", "message": time.Now().Unix()})
		write(map[string]any{"identifier": string(identJSON), "message": map[string]any{"type": "hello", "features": []string{"outcome", "continue"}}})
		write(map[string]any{"type": "confirm_subscription", "identifier": string(identJSON)})
		write(map[string]any{"identifier": string(identJSON), "message": map[string]any{"type": "run.cancel", "run_id": "r1"}})
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, Options{Server: srv.URL, Token: "poa_test", Identifier: ident})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var got []string
	for len(got) < 2 {
		select {
		case raw, ok := <-conn.Messages():
			if !ok {
				t.Fatalf("messages closed after %v", got)
			}
			var m struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &m)
			got = append(got, m.Type)
		case <-ctx.Done():
			t.Fatalf("timed out with %v", got)
		}
	}
	if got[0] != "hello" || got[1] != "run.cancel" {
		t.Fatalf("messages arrived as %v, want hello then run.cancel", got)
	}
}
