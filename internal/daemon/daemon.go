// Package daemon is `productos-agent run`: it keeps the device registered and online, holds the AgentChannel
// socket (reconnecting with backoff), heartbeats every 30 s, and hands run.start / run.cancel / run.answer to
// runners. Events and statuses go over the socket when it is up and over REST otherwise.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Hamdan85/productos-agent/internal/api"
	"github.com/Hamdan85/productos-agent/internal/cable"
	"github.com/Hamdan85/productos-agent/internal/config"
	"github.com/Hamdan85/productos-agent/internal/oauth"
	"github.com/Hamdan85/productos-agent/internal/protocol"
	"github.com/Hamdan85/productos-agent/internal/runner"
	"github.com/Hamdan85/productos-agent/internal/tools"
	"github.com/Hamdan85/productos-agent/internal/version"
)

const (
	heartbeatEvery = 30 * time.Second
	redetectEvery  = 5 * time.Minute
	ackTimeout     = 10 * time.Second
	maxBackoff     = 60 * time.Second
)

// ErrDeviceRevoked: the server no longer knows this device; the person must sign in again.
var ErrDeviceRevoked = errors.New("this device was revoked in ProductOS; run `productos-agent login` to register it again")

// Daemon is one running agent process.
type Daemon struct {
	Config *config.Config
	API    *api.Client
	Tokens api.TokenSource
	Log    *slog.Logger
	// Once exits after the first run finishes.
	Once bool
	// ToolPath overrides tool detection for runs (tests).
	ToolPath string
	// Detect overrides tool detection (tests).
	Detect func(context.Context) protocol.Tools

	tools   protocol.Tools
	toolsMu sync.Mutex

	connMu sync.RWMutex
	conn   *cable.Conn

	runsMu sync.Mutex
	runs   map[string]*runner.Run

	acksMu sync.Mutex
	acks   map[string]chan protocol.ServerMessage

	finished chan string
	state    State
	stateMu  sync.Mutex
}

// Run blocks until ctx ends (or, with Once, until the first run completes).
func (d *Daemon) Run(ctx context.Context) error {
	if d.Detect == nil {
		d.Detect = tools.Detect
	}
	d.runs = map[string]*runner.Run{}
	d.acks = map[string]chan protocol.ServerMessage{}
	d.finished = make(chan string, 16)
	d.tools = d.Detect(ctx)
	d.state = State{PID: os.Getpid(), StartedAt: time.Now()}
	d.publish()
	defer clearState()

	if err := d.ensureDevice(ctx); err != nil {
		return err
	}
	d.Log.Info("agent ready", "device_id", d.Config.DeviceID, "server", d.Config.Server, "tools", toolSummary(d.tools))

	backoff := time.Second
	for {
		conn, err := d.connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, cable.ErrRejected) {
				if gone := d.deviceGone(ctx); gone {
					d.setError(ErrDeviceRevoked.Error())
					return ErrDeviceRevoked
				}
			}
			if errors.Is(err, oauth.ErrSignedOut) {
				d.setError(err.Error())
				return err
			}
			d.Log.Warn("cannot connect; retrying", "in", backoff, "error", err)
			d.setError(err.Error())
			if !sleep(ctx, jitter(backoff)) {
				return nil
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		d.setConn(conn)
		d.setConnected(true)
		d.Log.Info("connected")
		d.resumeRuns(ctx)
		done := d.serve(ctx, conn)
		d.setConn(nil)
		d.setConnected(false)
		if done {
			return nil
		}
		d.Log.Warn("socket closed; reconnecting", "error", conn.Err())
		if !sleep(ctx, jitter(backoff)) {
			return nil
		}
	}
}

// ensureDevice registers the machine once, or re-registers when the stored id is gone.
func (d *Daemon) ensureDevice(ctx context.Context) error {
	if d.Config.DeviceID != "" {
		_, err := d.API.Heartbeat(ctx, d.Config.DeviceID, api.HeartbeatInput{Tools: d.tools, AgentVersion: version.String()})
		if err == nil {
			return nil
		}
		if !api.IsCode(err, "not_found") {
			return fmt.Errorf("cannot reach the server: %w", err)
		}
		d.Log.Warn("device unknown to the server; registering again")
		d.Config.DeviceID = ""
	}
	device, err := d.API.RegisterDevice(ctx, api.RegisterDeviceInput{
		Name: d.deviceName(), Platform: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: version.String(), Tools: d.tools,
	})
	if err != nil {
		return fmt.Errorf("device registration failed: %w", err)
	}
	d.Config.DeviceID = device.ID
	return config.Save(d.Config)
}

func (d *Daemon) deviceName() string {
	if d.Config.DeviceName != "" {
		return d.Config.DeviceName
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "my machine"
	}
	return host
}

func (d *Daemon) deviceGone(ctx context.Context) bool {
	_, err := d.API.Heartbeat(ctx, d.Config.DeviceID, api.HeartbeatInput{AgentVersion: version.String()})
	return api.IsCode(err, "not_found")
}

func (d *Daemon) connect(ctx context.Context) (*cable.Conn, error) {
	token, err := d.Tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	d.toolsMu.Lock()
	ident := map[string]any{"channel": "AgentChannel", "device_id": d.Config.DeviceID, "tools": d.tools, "agent_version": version.String()}
	d.toolsMu.Unlock()
	conn, err := cable.Dial(ctx, cable.Options{Server: d.Config.Server, Token: token, Identifier: ident, UserAgent: "productos-agent/" + version.String()})
	if err != nil && strings.Contains(err.Error(), "401") {
		// A 401 on the handshake means the token died: refresh once and let the loop retry.
		_ = d.Tokens.Invalidate(ctx, token)
	}
	return conn, err
}

// serve reads the socket until it dies; returns true when the daemon should exit (ctx done or Once satisfied).
func (d *Daemon) serve(ctx context.Context, conn *cable.Conn) bool {
	heartbeat := time.NewTicker(heartbeatEvery)
	defer heartbeat.Stop()
	redetect := time.NewTicker(redetectEvery)
	defer redetect.Stop()
	d.touchHeartbeat()
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return true
		case <-conn.Done():
			return false
		case raw, ok := <-conn.Messages():
			if !ok {
				return false
			}
			d.dispatch(ctx, raw)
		case <-heartbeat.C:
			d.heartbeat(ctx, conn)
			if conn.Stale(45 * time.Second) {
				d.Log.Warn("no server ping for 45 s; reconnecting")
				_ = conn.Close()
				return false
			}
		case <-redetect.C:
			fresh := d.Detect(ctx)
			d.toolsMu.Lock()
			changed := tools.Changed(d.tools, fresh)
			d.tools = fresh
			d.toolsMu.Unlock()
			if changed {
				d.Log.Info("tools changed", "tools", toolSummary(fresh))
				d.heartbeat(ctx, conn)
			}
		case id := <-d.finished:
			d.runsMu.Lock()
			delete(d.runs, id)
			n := len(d.runs)
			d.runsMu.Unlock()
			d.publish()
			if d.Once && n == 0 {
				d.Log.Info("run finished; exiting (--once)")
				_ = conn.Close()
				return true
			}
		}
	}
}

func (d *Daemon) heartbeat(ctx context.Context, conn *cable.Conn) {
	d.toolsMu.Lock()
	t := d.tools
	d.toolsMu.Unlock()
	err := conn.Perform(ctx, "heartbeat", map[string]any{"tools": t, "agent_version": version.String()})
	if err != nil {
		d.Log.Debug("heartbeat over the socket failed; using REST", "error", err)
		if _, rerr := d.API.Heartbeat(ctx, d.Config.DeviceID, api.HeartbeatInput{Tools: t, AgentVersion: version.String()}); rerr != nil {
			d.Log.Warn("heartbeat failed", "error", rerr)
			return
		}
	}
	d.touchHeartbeat()
}

func (d *Daemon) dispatch(ctx context.Context, raw json.RawMessage) {
	var msg protocol.ServerMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		d.Log.Debug("unreadable message", "error", err)
		return
	}
	switch msg.Type {
	case "hello":
		d.Log.Debug("hello", "protocol_version", msg.ProtocolVersion)
	case "run.start":
		d.startRun(ctx, msg.AsRunStart())
	case "run.cancel":
		if run := d.run(msg.RunID); run != nil {
			d.Log.Info("run cancelled by the server", "run_id", msg.RunID, "reason", msg.Reason)
			run.Cancel(msg.Reason)
		}
	case "run.answer":
		if run := d.run(msg.RunID); run != nil {
			run.Answer(runner.Answer{Seq: msg.Seq, Text: msg.Text})
		}
	case "ack", "error":
		d.acksMu.Lock()
		ch := d.acks[msg.RunID]
		d.acksMu.Unlock()
		if ch != nil {
			select {
			case ch <- msg:
			default:
			}
		} else if msg.Type == "error" {
			d.Log.Warn("server error", "run_id", msg.RunID, "code", msg.Code, "message", msg.Message)
		}
	default:
		d.Log.Debug("ignored message", "type", msg.Type)
	}
}

func (d *Daemon) run(id string) *runner.Run {
	d.runsMu.Lock()
	defer d.runsMu.Unlock()
	return d.runs[id]
}

func (d *Daemon) startRun(ctx context.Context, start protocol.RunStart) {
	d.runsMu.Lock()
	if _, exists := d.runs[start.RunID]; exists {
		d.runsMu.Unlock()
		return
	}
	run := runner.New(start, d, d.Config, d.Log)
	run.ToolPath = d.ToolPath
	d.runs[start.RunID] = run
	d.runsMu.Unlock()
	d.publish()
	d.Log.Info("run started", "run_id", start.RunID, "tool", start.Tool, "subject", start.Subject.Key, "repository", start.Repository.FullName)
	go func() {
		final := run.Execute(ctx)
		d.Log.Info("run ended", "run_id", start.RunID, "status", final.Status, "reason", final.Reason)
		d.finished <- start.RunID
	}()
}

// resumeRuns: after a (re)connect, runs the server still considers acknowledged but this process does not know were
// interrupted by a restart; the tool is gone, so they fail honestly.
func (d *Daemon) resumeRuns(ctx context.Context) {
	runs, err := d.API.ActiveRuns(ctx, d.Config.DeviceID)
	if err != nil {
		d.Log.Debug("cannot list active runs", "error", err)
		return
	}
	for _, r := range runs {
		if d.run(r.ID) != nil || r.Status == protocol.StatusQueued {
			// Ours, or not yet acknowledged by anyone: its run.start is on its way (or the server fails it with no_ack).
			continue
		}
		d.Log.Warn("run was active before the agent restarted; failing it", "run_id", r.ID)
		_, _ = d.API.PostStatus(ctx, r.ID, protocol.StatusReport{Status: protocol.StatusFailed, Reason: "the agent restarted while this run was in progress"})
	}
}

// --- runner.Reporter ---

// Events sends over the socket and waits for the ack; without a socket (or without an ack in time) it uses REST.
func (d *Daemon) Events(ctx context.Context, runID string, events []protocol.Event) (int, error) {
	if conn := d.getConn(); conn != nil {
		ack, err := d.performAndWait(ctx, conn, runID, "run_event", map[string]any{"run_id": runID, "events": events})
		if err == nil {
			if ack.Type == "error" {
				return 0, mapChannelError(ack)
			}
			if ack.LastSeq != nil {
				return *ack.LastSeq, nil
			}
			return 0, nil
		}
		d.Log.Debug("run_event over the socket failed; using REST", "error", err)
	}
	res, err := d.API.PostEvents(ctx, runID, events)
	if err != nil {
		return 0, mapAPIError(err)
	}
	return res.LastSeq, nil
}

func (d *Daemon) Status(ctx context.Context, runID string, report protocol.StatusReport) error {
	data := map[string]any{"run_id": runID, "status": report.Status}
	if report.Reason != "" {
		data["reason"] = report.Reason
	}
	if report.Branch != "" {
		data["branch"] = report.Branch
	}
	if report.HeadCommit != "" {
		data["head_commit"] = report.HeadCommit
	}
	if report.PRURL != "" {
		data["pr_url"] = report.PRURL
	}
	if report.Summary != "" {
		data["summary"] = report.Summary
	}
	if conn := d.getConn(); conn != nil {
		ack, err := d.performAndWait(ctx, conn, runID, "run_status", data)
		if err == nil {
			if ack.Type == "error" {
				return mapChannelError(ack)
			}
			return nil
		}
		d.Log.Debug("run_status over the socket failed; using REST", "error", err)
	}
	_, err := d.API.PostStatus(ctx, runID, report)
	return mapAPIError(err)
}

func (d *Daemon) performAndWait(ctx context.Context, conn *cable.Conn, runID, action string, data map[string]any) (protocol.ServerMessage, error) {
	ch := make(chan protocol.ServerMessage, 1)
	d.acksMu.Lock()
	d.acks[runID] = ch
	d.acksMu.Unlock()
	defer func() {
		d.acksMu.Lock()
		if d.acks[runID] == ch {
			delete(d.acks, runID)
		}
		d.acksMu.Unlock()
	}()
	if err := conn.Perform(ctx, action, data); err != nil {
		return protocol.ServerMessage{}, err
	}
	select {
	case ack := <-ch:
		return ack, nil
	case <-conn.Done():
		return protocol.ServerMessage{}, cable.ErrClosed
	case <-time.After(ackTimeout):
		return protocol.ServerMessage{}, errors.New("no ack within 10 s")
	case <-ctx.Done():
		return protocol.ServerMessage{}, ctx.Err()
	}
}

func mapChannelError(msg protocol.ServerMessage) error {
	err := fmt.Errorf("%s: %s", msg.Code, msg.Message)
	switch msg.Code {
	case "run_finished", "not_found", "invalid_transition":
		return fmt.Errorf("%w (%s)", runner.ErrRunFinished, msg.Code)
	case "invalid_event", "validation_failed":
		return &runner.PermanentError{Err: err}
	}
	return err
}

func mapAPIError(err error) error {
	if err == nil {
		return nil
	}
	var e *api.Error
	if errors.As(err, &e) {
		switch e.Code {
		case "run_finished", "not_found", "invalid_transition":
			return fmt.Errorf("%w (%s)", runner.ErrRunFinished, e.Code)
		case "invalid_event", "validation_failed":
			return &runner.PermanentError{Err: err}
		}
	}
	return err
}

// --- state ---

func (d *Daemon) getConn() *cable.Conn {
	d.connMu.RLock()
	defer d.connMu.RUnlock()
	return d.conn
}

func (d *Daemon) setConn(c *cable.Conn) {
	d.connMu.Lock()
	d.conn = c
	d.connMu.Unlock()
}

func (d *Daemon) setConnected(v bool) {
	d.stateMu.Lock()
	d.state.Connected = v
	if v {
		d.state.Error = ""
	}
	d.stateMu.Unlock()
	d.publish()
}

func (d *Daemon) setError(msg string) {
	d.stateMu.Lock()
	d.state.Error = msg
	d.stateMu.Unlock()
	d.publish()
}

func (d *Daemon) touchHeartbeat() {
	d.stateMu.Lock()
	d.state.LastHeartbeat = time.Now()
	d.stateMu.Unlock()
	d.publish()
}

func (d *Daemon) publish() {
	d.runsMu.Lock()
	ids := make([]string, 0, len(d.runs))
	for id := range d.runs {
		ids = append(ids, id)
	}
	d.runsMu.Unlock()
	d.stateMu.Lock()
	s := d.state
	s.ActiveRuns = ids
	d.stateMu.Unlock()
	writeState(s)
}

func toolSummary(t protocol.Tools) string {
	out := ""
	for _, name := range tools.Known {
		info := t[name]
		if !info.Available {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += name
		if info.Version != "" {
			out += " " + info.Version
		}
	}
	if out == "" {
		return "none"
	}
	return out
}

func jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int63n(int64(d)/4+1))
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
