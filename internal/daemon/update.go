package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/productos-technology/productos-agent/internal/protocol"
	"github.com/productos-technology/productos-agent/internal/runner"
	"github.com/productos-technology/productos-agent/internal/update"
)

// Self-update (docs/reference/agent-protocol.md, Agent versions and self-update). The server is the authority on
// versions: its `agent` block arrives with hello and run.start. Updates happen before a run (with a handoff of the
// acknowledged runs) or when idle, never in the middle of a run, and a failed update never fails a run.

const (
	// versionCheckEvery: once an hour the heartbeat goes over REST, whose device view carries latest_version (the
	// socket heartbeat answers nothing), so a long-lived connection still learns about a release.
	versionCheckEvery = time.Hour
	// retryUpdateAfter: a version that failed to install is not tried again before this.
	retryUpdateAfter = time.Hour
)

type staged struct {
	path, version string
	err           error
}

func (d *Daemon) setAgent(a *protocol.AgentVersions) {
	if a == nil {
		return
	}
	cp := *a
	d.stateMu.Lock()
	d.agent = &cp
	d.stateMu.Unlock()
	d.publish()
}

func (d *Daemon) getAgent() *protocol.AgentVersions {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.agent == nil {
		return nil
	}
	cp := *d.agent
	return &cp
}

func (d *Daemon) installMethod() update.Method { return update.DetectMethod(d.exeInvoked, d.Exe) }

// learnLatest takes a newer latest_version from the REST heartbeat into the block the server sent last (without a
// block, the server never offered updates: nothing to do).
func (d *Daemon) learnLatest(ctx context.Context, latest *string) {
	if latest == nil || !update.Valid(*latest) {
		return
	}
	d.stateMu.Lock()
	newer := d.agent != nil && update.Compare(*latest, d.agent.LatestVersion) > 0
	if newer {
		d.agent.LatestVersion = *latest
	}
	d.stateMu.Unlock()
	if newer {
		d.publish()
		d.maybeUpdateIdle(ctx)
	}
}

// updateCheck says whether this agent should update now (target), or why it will not although it is behind (held,
// for an update_available note). Development builds never update themselves and say nothing.
func (d *Daemon) updateCheck(a *protocol.AgentVersions) (target, held string) {
	if a == nil || !update.Behind(d.Version, a.LatestVersion) || update.IsDev(d.Version) {
		return "", ""
	}
	available := fmt.Sprintf("v%s is available", update.Number(a.LatestVersion))
	switch method := d.installMethod(); {
	case !a.UpdateEnabled:
		return "", available + " (updates are paused by the server)"
	case !d.Config.AutoUpdateEnabled():
		return "", available + ": run `productos-agent update`"
	case method != update.MethodSelf:
		return "", available + ": run `" + method.Command() + "`"
	}
	d.updateMu.Lock()
	failedAt, failed := d.failedUpdates[a.LatestVersion]
	d.updateMu.Unlock()
	if failed && time.Since(failedAt) < retryUpdateAfter {
		return "", available + " (the last attempt to install it failed)"
	}
	return a.LatestVersion, ""
}

// beforePrepare is every run's gate between its `starting` ack and the checkout. When an update is due and no other
// run is past the gate, it updates and re-executes with a handoff of the runs waiting here (it does not return then).
// Runs that arrive meanwhile wait on gateMu and travel in the handoff. Any failure is a warn log and an
// update_failed note: the run continues on this version.
func (d *Daemon) beforePrepare(ctx context.Context, run *runner.Run) []protocol.Note {
	d.gateMu.Lock()
	defer d.gateMu.Unlock()
	defer d.passGate(run.Start.RunID)

	a := run.Start.Agent
	if a == nil {
		a = d.getAgent()
	}
	target, held := d.updateCheck(a)
	if target == "" {
		if held != "" {
			return []protocol.Note{{Code: runner.NoteUpdateAvailable, Text: held}}
		}
		return nil
	}
	if d.staging.Load() || d.runningCount() > 0 {
		return []protocol.Note{{Code: runner.NoteUpdateAvailable, Text: fmt.Sprintf("v%s is available; the agent updates when no run is in progress", update.Number(target))}}
	}
	run.Emit(protocol.Log("info", fmt.Sprintf("Updating the agent from v%s to v%s.", update.Number(d.Version), update.Number(target))))
	_ = run.Flush(ctx)
	err := d.applyUpdate(ctx, target, a.DownloadBase, true)
	d.Log.Warn("update failed; the run continues", "to", target, "error", err)
	run.Emit(protocol.Log("warn", fmt.Sprintf("The update to v%s failed; this run continues on v%s: %s", update.Number(target), update.Number(d.Version), oneLine(err.Error()))))
	return []protocol.Note{{Code: runner.NoteUpdateFailed, Text: oneLine(err.Error())}}
}

// maybeUpdateIdle starts staging the latest release in the background when nothing runs; serve installs it when it
// is ready and still nothing runs.
func (d *Daemon) maybeUpdateIdle(ctx context.Context) {
	if d.Once {
		return // a one-shot run does not restart itself
	}
	a := d.getAgent()
	target, _ := d.updateCheck(a)
	if target == "" || d.activeCount() > 0 || !d.staging.CompareAndSwap(false, true) {
		return
	}
	d.Log.Info("updating the agent while idle", "from", d.Version, "to", target)
	go func() {
		path, err := d.updater(a.DownloadBase).Stage(ctx, target)
		select {
		case d.stagedCh <- staged{path: path, version: target, err: err}:
		case <-ctx.Done():
			if path != "" {
				_ = os.Remove(path)
			}
		}
	}()
}

// installStaged is serve's side of an idle update: swap and re-execute, unless a run started meanwhile.
func (d *Daemon) installStaged(s staged) {
	defer d.staging.Store(false)
	if s.err != nil {
		d.markFailed(s.version)
		d.Log.Warn("idle update failed", "to", s.version, "error", s.err)
		return
	}
	if d.activeCount() > 0 {
		_ = os.Remove(s.path)
		return // the next idle moment tries again
	}
	if err := d.install(s.path, s.version, false); err != nil {
		d.Log.Warn("idle update failed", "to", s.version, "error", err)
	}
}

// applyUpdate stages, swaps and re-executes; it returns only on failure.
func (d *Daemon) applyUpdate(ctx context.Context, target, base string, handoff bool) error {
	path, err := d.updater(base).Stage(ctx, target)
	if err != nil {
		d.markFailed(target)
		return err
	}
	return d.install(path, target, handoff)
}

// install swaps the staged binary in and re-executes (with the handoff of the runs at the gate, when asked). It
// returns only on failure.
func (d *Daemon) install(path, target string, handoff bool) error {
	if handoff {
		d.stateMu.Lock()
		features := d.features
		d.stateMu.Unlock()
		h := Handoff{From: update.Number(d.Version), To: update.Number(target), PID: os.Getpid(), WrittenAt: time.Now(), Runs: d.pendingMessages(), Features: features}
		if err := writeHandoff(h); err != nil {
			_ = os.Remove(path)
			d.markFailed(target)
			return fmt.Errorf("cannot write the handoff: %w", err)
		}
	}
	if err := update.Replace(path, d.Exe); err != nil {
		_ = os.Remove(path)
		removeHandoff()
		d.markFailed(target)
		return fmt.Errorf("cannot replace %s: %w", d.Exe, err)
	}
	d.Log.Info("agent updated; restarting", "from", d.Version, "to", target, "binary", d.Exe)
	// Leave the channel before the new process subscribes, so the server does not mark the device offline after it.
	if conn := d.getConn(); conn != nil {
		_ = conn.Close()
	}
	reexec := d.Reexec
	if reexec == nil {
		reexec = update.Reexec
	}
	err := reexec(d.Exe)
	removeHandoff()
	d.markFailed(target)
	return fmt.Errorf("v%s is installed but could not be started (%v); restart the agent", update.Number(target), err)
}

func (d *Daemon) updater(base string) *update.Updater {
	u := update.New(base, d.Exe)
	u.HTTP = d.HTTP
	return u
}

func (d *Daemon) markFailed(version string) {
	d.updateMu.Lock()
	d.failedUpdates[version] = time.Now()
	d.updateMu.Unlock()
}

// passGate: the run is past the update gate (it now counts as running).
func (d *Daemon) passGate(id string) {
	d.runsMu.Lock()
	delete(d.pending, id)
	d.runsMu.Unlock()
}

func (d *Daemon) pendingMessages() []protocol.ServerMessage {
	d.runsMu.Lock()
	defer d.runsMu.Unlock()
	out := make([]protocol.ServerMessage, 0, len(d.pending))
	for _, m := range d.pending {
		out = append(out, m)
	}
	return out
}

// runningCount is the runs past the gate (touching a checkout); activeCount is every run this process holds.
func (d *Daemon) runningCount() int {
	d.runsMu.Lock()
	defer d.runsMu.Unlock()
	return len(d.runs) - len(d.pending)
}

func (d *Daemon) activeCount() int {
	d.runsMu.Lock()
	defer d.runsMu.Unlock()
	return len(d.runs)
}

// resumeHandoff starts the runs the previous process handed over, before connecting (so the reconnect does not fail
// them as interrupted). They skip `starting`; their report says the agent updated.
func (d *Daemon) resumeHandoff(ctx context.Context) {
	h, err := takeHandoff()
	if err != nil {
		d.Log.Warn("unreadable handoff; its runs will fail as interrupted", "error", err)
		return
	}
	if h == nil || len(h.Runs) == 0 {
		return
	}
	if time.Since(h.WrittenAt) > handoffMaxAge {
		d.Log.Warn("stale handoff ignored", "written_at", h.WrittenAt)
		return
	}
	d.stateMu.Lock()
	if d.features == nil {
		d.features = h.Features
	}
	d.stateMu.Unlock()
	note := protocol.Note{Code: runner.NoteUpdated, Text: fmt.Sprintf("the agent updated itself from v%s to v%s before this run", h.From, h.To)}
	if update.Compare(d.Version, h.To) != 0 {
		note = protocol.Note{Code: runner.NoteUpdateFailed, Text: fmt.Sprintf("the agent restarted to update to v%s but runs v%s", h.To, update.Number(d.Version))}
	}
	for _, msg := range h.Runs {
		d.Log.Info("resuming a handed-over run", "run_id", msg.RunID)
		d.startRun(ctx, msg, true, note)
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 280 {
		return s
	}
	n := 280
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
