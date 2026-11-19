// cli_session_outside_writer.go — the outside-writer watcher of a session AI
// Expedite continues IN PLACE (shared-constants cliSessionDiscovery.js
// CLI_SESSION_DEVICE_RESULT_TYPES.OUTSIDE_WRITER), for every engine.
//
// When AI Expedite resumes a coding-agent session another program may also
// hold (an adopted VS Code tab, a Codex thread a terminal still has open),
// the watcher polls that engine's writer evidence every 5 s and publishes
// claude_native_outside_writer — busy / idle / gone, naming this agent — when
// the state another program holds changes. terminal-service holds a message
// for an adopted session while the outside writer is busy and releases it on
// the next idle or gone frame (adoptedSessionClaims.service.js
// recordOutsideWriterChange), so:
//
//   - a frame that misses the cloud (published while the Pub/Sub link was
//     down) must not strand a held message: every running watcher's last
//     frame is published again on each new Pub/Sub connection, and its later
//     frames go through that connection (resendCliSessionOutsideWriterStates);
//   - AI Expedite's own child is never another writer.
//
// Writer evidence, per engine:
//
//	claudeCode  Claude Code's running registry (<configDir>/sessions/<pid>.json):
//	            pid + status (busy / waiting / idle), entrypoint → editor. Read
//	            exactly as protocol 2 did; this agent's own children excluded.
//	the others  none keeps a status registry, so two signals stand in for it:
//	            a live process of that agent's CLI whose argv names the session
//	            (`codex resume <id>`, `agy --conversation <id>`, …) holds it
//	            open (idle: open elsewhere), and the session's own store
//	            written within writerQuietMs (10 s) by anything but AI
//	            Expedite's own child is a writer mid-turn (busy).
//
// Recorded limits of the second kind: a program that holds a session without
// naming it on its command line (an IDE extension's app-server, a TUI that
// switched sessions inside itself) is seen only while it writes, so between
// its writes it reads gone, not idle; a writer working beside AI Expedite's
// own turn is not told apart from it until that turn's writes stop; and a
// long silent tool call reads idle once the store is quiet for 10 s. Muse
// Code keeps no store this agent can stat (its list is behind `muse serve`),
// so its watcher reads processes only.
//
// READ-ONLY: the process table (names and argv of the agents' own CLIs only,
// matched against the session id and never sent anywhere) and the stores'
// modification times. Nothing is written, locked or signalled.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

const (
	// The watcher's poll interval.
	claudeOutsideWriterPollInterval = 5 * time.Second
	// cliSessionWriterQuiet is shared-constants writerQuietMs: a store written
	// this recently by something else is a writer mid-turn.
	cliSessionWriterQuiet = 10 * time.Second
	// cliSessionOwnWriteSlack: a store write this soon after AI Expedite's own
	// child last read or wrote a frame is that child's (a turn's last record
	// is flushed just after its last frame).
	cliSessionOwnWriteSlack = 5 * time.Second
	// One process scan serves every watcher (and probe) for this long.
	cliSessionProcessSnapshotTTL = 2 * time.Second
	// cliSessionProcessScanBudget bounds one scan of the process table.
	cliSessionProcessScanBudget = 2 * time.Second
	// cliSessionProcessAncestryDepth bounds the parent walk that tells this
	// agent's own descendants (an npm launcher, then the CLI) from the rest.
	cliSessionProcessAncestryDepth = 8
)

// cliOutsideWriterPollNanos is the poll interval watchers start with
// (claudeOutsideWriterPollInterval); tests shorten it.
var cliOutsideWriterPollNanos atomic.Int64

func cliOutsideWriterPollInterval() time.Duration {
	if d := time.Duration(cliOutsideWriterPollNanos.Load()); d > 0 {
		return d
	}
	return claudeOutsideWriterPollInterval
}

// Outside-writer states (shared-constants CLI_SESSION_OUTSIDE_WRITER_STATE).
const (
	claudeOutsideWriterBusy = "busy"
	claudeOutsideWriterIdle = "idle"
	claudeOutsideWriterGone = "gone"
)

// Writer statuses (shared-constants CLI_SESSION_RUN_STATUS).
const (
	cliSessionRunBusy = "busy"
	cliSessionRunIdle = "idle"
)

/* -------------------------------------------------------------------------- */
/* State and watch loop                                                       */
/* -------------------------------------------------------------------------- */

// claudeOutsideWriterState is the state another process holds sessionID in:
// busy when any writer is mid-turn (an unknown status counts as busy), idle
// when every writer is idle, gone when none holds it.
func claudeOutsideWriterState(writers []cliSessionWriter) (state, entrypoint, editor string) {
	if len(writers) == 0 {
		return claudeOutsideWriterGone, "", ""
	}
	state = claudeOutsideWriterIdle
	for _, w := range writers {
		if w.Status != "idle" {
			state = claudeOutsideWriterBusy
		}
		if entrypoint == "" {
			entrypoint, editor = w.Entrypoint, w.Editor
		}
	}
	return state, entrypoint, editor
}

// claudeOutsideWriterWatch polls `poll` every interval until stop closes and
// calls emit whenever the outside-writer state changes. The first observation
// is emitted only when someone else already holds the session (a resumed
// session that starts alone has nothing to report).
func claudeOutsideWriterWatch(stop <-chan struct{}, interval time.Duration, poll func() []cliSessionWriter, emit func(cliSessionOutsideWriter)) {
	cliOutsideWriterWatchLoop(stop, nil, interval, poll, emit)
}

// cliOutsideWriterWatchLoop is claudeOutsideWriterWatch for a session that
// may have no stop channel: it also ends, without a frame, on the first poll
// at which alive (when set) reports the session gone. Ending says nothing
// about the outside writer, so no state is published for it.
func cliOutsideWriterWatchLoop(stop <-chan struct{}, alive func() bool, interval time.Duration, poll func() []cliSessionWriter, emit func(cliSessionOutsideWriter)) {
	last := claudeOutsideWriterGone
	check := func() {
		state, entrypoint, editor := claudeOutsideWriterState(poll())
		if state == last {
			return
		}
		last = state
		emit(cliSessionOutsideWriter{State: state, Entrypoint: entrypoint, Editor: editor, At: time.Now().UnixMilli()})
	}
	if alive != nil && !alive() {
		return
	}
	check()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if alive != nil && !alive() {
				return
			}
			check()
		}
	}
}

/* -------------------------------------------------------------------------- */
/* The frame                                                                  */
/* -------------------------------------------------------------------------- */

// cliOutsideWriterOwner is AI Expedite's own session the frame is about: the
// terminal session id terminal-service records the outside writer on.
type cliOutsideWriterOwner struct {
	ID          string
	WorkspaceID string
	UID         string
}

// cliSessionOutsideWriterFrame is the claude_native_outside_writer result for
// one observed change, whatever the engine. terminal-service records it only
// when `agentId` and `uid` name the session's own computer and owner
// (adoptedSessionClaims.service.js recordOutsideWriter) and drops it
// otherwise, so the frame must name this agent; `bootId` says which agent
// process saw it. The wire shape is pinned by
// testdata/claude_native_outside_writer_frame.json, the fixture
// terminal-service's tests read too.
func cliSessionOutsideWriterFrame(owner cliOutsideWriterOwner, agentID string, w cliSessionOutsideWriter, ts int64) resultMsg {
	return resultMsg{
		ID:            owner.ID,
		WorkspaceID:   owner.WorkspaceID,
		UID:           owner.UID,
		AgentID:       agentID,
		Status:        "success",
		Ts:            ts,
		Version:       Version,
		Type:          claudeOutsideWriterResultType,
		SessionID:     owner.ID,
		OutsideWriter: &w,
		BootID:        agentBootID,
	}
}

// claudeOutsideWriterFrame is cliSessionOutsideWriterFrame for a
// claude_native session.
func claudeOutsideWriterFrame(session *ClaudeNativeSession, agentID string, w cliSessionOutsideWriter, ts int64) resultMsg {
	return cliSessionOutsideWriterFrame(cliOutsideWriterOwner{ID: session.ID, WorkspaceID: session.WorkspaceID, UID: session.UID}, agentID, w, ts)
}

/* -------------------------------------------------------------------------- */
/* Running watchers: re-sent on reconnect                                     */
/* -------------------------------------------------------------------------- */

// cliOutsideWriterWatch is one running watcher: the publisher its frames go
// through and the last frame it published.
type cliOutsideWriterWatch struct {
	key   string
	stop  <-chan struct{}
	alive func() bool

	mu      sync.Mutex
	publish PublishFunc
	last    *resultMsg
}

// ended reports whether the session this watcher watches for has ended (its
// loop returns at its next tick).
func (w *cliOutsideWriterWatch) ended() bool {
	if w.stop != nil {
		select {
		case <-w.stop:
			return true
		default:
		}
	}
	return w.alive != nil && !w.alive()
}

// emit records frame as this watcher's current state and publishes it.
func (w *cliOutsideWriterWatch) emit(frame resultMsg) {
	w.mu.Lock()
	kept := frame
	w.last = &kept
	publish := w.publish
	w.mu.Unlock()
	if publish != nil {
		publish(frame)
	}
}

// cliOutsideWriterWatches are the running watchers, by engine | AI Expedite
// session | the engine's session id: a redelivered resume never starts a
// second watcher for the same pair.
var cliOutsideWriterWatches = struct {
	sync.Mutex
	byKey map[string]*cliOutsideWriterWatch
}{byKey: map[string]*cliOutsideWriterWatch{}}

// registerCliOutsideWriterWatch adds t's watcher, or reports false when one
// is already running for the same pair in a session still running. A
// watcher whose session has ended (it unwinds at its next tick) is replaced,
// so a new session under a reused id is never left unwatched.
func registerCliOutsideWriterWatch(key string, t cliOutsideWriterTarget, publish PublishFunc) (*cliOutsideWriterWatch, bool) {
	cliOutsideWriterWatches.Lock()
	defer cliOutsideWriterWatches.Unlock()
	if running, ok := cliOutsideWriterWatches.byKey[key]; ok && !running.ended() {
		return nil, false
	}
	w := &cliOutsideWriterWatch{key: key, stop: t.stop, alive: t.alive, publish: publish}
	cliOutsideWriterWatches.byKey[key] = w
	return w, true
}

func unregisterCliOutsideWriterWatch(w *cliOutsideWriterWatch) {
	cliOutsideWriterWatches.Lock()
	defer cliOutsideWriterWatches.Unlock()
	if cliOutsideWriterWatches.byKey[w.key] == w {
		delete(cliOutsideWriterWatches.byKey, w.key)
	}
}

// resendCliSessionOutsideWriterStates runs on every new Pub/Sub connection
// (runPubSubConnection) with that connection's results publisher. Each
// running watcher publishes its frames through it from now on — the
// session's own publisher belonged to the connection that just closed — and
// the last state it reported is published again. A frame published while the
// link was down (the `idle` that would release a held message) is never
// lost for good: the cloud reads a repeat as the same report (the same
// `outsideWriter.at`), and a newer one as newer. A watcher that has reported
// nothing has nothing the cloud could be holding for. Returns how many
// frames went out.
func resendCliSessionOutsideWriterStates(publish PublishFunc) int {
	if publish == nil {
		return 0
	}
	cliOutsideWriterWatches.Lock()
	watches := make([]*cliOutsideWriterWatch, 0, len(cliOutsideWriterWatches.byKey))
	for _, w := range cliOutsideWriterWatches.byKey {
		watches = append(watches, w)
	}
	cliOutsideWriterWatches.Unlock()
	sent := 0
	for _, w := range watches {
		w.mu.Lock()
		w.publish = publish
		var frame resultMsg
		have := w.last != nil
		if have {
			frame = *w.last
		}
		w.mu.Unlock()
		if !have {
			continue
		}
		frame.Ts = time.Now().UnixMilli()
		publish(frame)
		sent++
	}
	return sent
}

/* -------------------------------------------------------------------------- */
/* Starting a watcher                                                         */
/* -------------------------------------------------------------------------- */

// cliOutsideWriterTarget is one session AI Expedite continues in place that
// another program may also hold.
type cliOutsideWriterTarget struct {
	// engine is shared-constants CLI_SESSION_ENGINES; sessionID the engine's
	// own id (Claude session id, Codex thread id, ACP session id, …).
	engine    string
	sessionID string
	// owner is AI Expedite's session the frames name; agentID this agent.
	owner   cliOutsideWriterOwner
	agentID string
	// stop closes when AI Expedite's session ends; alive, when set, reports
	// it still running (a session with no channel of its own).
	stop  <-chan struct{}
	alive func() bool
	// ownActiveAt is the last time AI Expedite's own child of this session
	// read or wrote a frame (now while a turn of it runs), so its own store
	// writes are never another writer. nil: AI Expedite never writes it.
	ownActiveAt func(now time.Time) time.Time
}

// startClaudeOutsideWriterWatcher watches a claude_native session resumed in
// place for another process holding the same Claude session id, until it
// exits. agentID is this agent's id (ClaudeNativeManager.frameAgentID).
func startClaudeOutsideWriterWatcher(session *ClaudeNativeSession, claudeSessionID, agentID string, publishFn PublishFunc) {
	if session == nil || publishFn == nil || !isClaudeSessionID(claudeSessionID) {
		return
	}
	startCliSessionOutsideWriterWatcher(cliOutsideWriterTarget{
		engine:    cliSessionEngineClaudeCode,
		sessionID: claudeSessionID,
		owner:     cliOutsideWriterOwner{ID: session.ID, WorkspaceID: session.WorkspaceID, UID: session.UID},
		agentID:   agentID,
		stop:      session.processExited,
	}, publishFn)
}

// startCliSessionOutsideWriterWatcher starts t's watcher, publishing through
// publishFn, and reports whether it did (false for an engine or id this build
// cannot watch, or one already watched).
func startCliSessionOutsideWriterWatcher(t cliOutsideWriterTarget, publishFn PublishFunc) bool {
	if publishFn == nil || t.owner.ID == "" || (t.stop == nil && t.alive == nil) {
		return false
	}
	poll := cliSessionOutsideWriterPoll(t)
	if poll == nil {
		return false
	}
	w, ok := registerCliOutsideWriterWatch(t.engine+"|"+t.owner.ID+"|"+t.sessionID, t, publishFn)
	if !ok {
		return false
	}
	go func() {
		defer unregisterCliOutsideWriterWatch(w)
		cliOutsideWriterWatchLoop(t.stop, t.alive, cliOutsideWriterPollInterval(), poll, func(state cliSessionOutsideWriter) {
			w.emit(cliSessionOutsideWriterFrame(t.owner, t.agentID, state, time.Now().UnixMilli()))
		})
	}()
	return true
}

// cliSessionOutsideWriterPoll reads t's engine's writer evidence, or nil when
// there is none to read for it.
func cliSessionOutsideWriterPoll(t cliOutsideWriterTarget) func() []cliSessionWriter {
	if t.engine == cliSessionEngineClaudeCode {
		if !isClaudeSessionID(t.sessionID) {
			return nil
		}
		configDir := claudeConfigDir(userHomeDir())
		return func() []cliSessionWriter {
			entries, _ := readClaudeRegistryEntries(configDir)
			return claudeOtherWriters(entries, t.sessionID, isClaudeOwnPID)
		}
	}
	ev, ok := cliSessionWriterEvidenceFor(t.engine)
	if !ok || !ev.validID(t.sessionID) {
		return nil
	}
	var clock func() int64
	if ev.storeClock != nil {
		clock = lastGoodStoreClock(ev.storeClock(t.sessionID))
	}
	return func() []cliSessionWriter {
		return cliSessionOutsideWriters(ev, t.sessionID, clock, t.ownActiveAt, time.Now())
	}
}

// lastGoodStoreClock keeps the newest store time a read returned: a read that
// fails (0, unknown) answers the last good one, so one failed poll never turns
// a busy session quiet — which, with the cloud's writer hold on, would release
// a held message beside a writer still mid-turn. The session still goes quiet
// once writerQuietMs passes after that last good write, as a quiet store does.
func lastGoodStoreClock(read func() int64) func() int64 {
	var last atomic.Int64
	return func() int64 {
		if ms := read(); ms > 0 {
			last.Store(ms)
			return ms
		}
		return last.Load()
	}
}

/* -------------------------------------------------------------------------- */
/* Writer evidence of the engines without a status registry                   */
/* -------------------------------------------------------------------------- */

// cliSessionWriterEvidence is how one engine's outside writer shows itself on
// this computer.
type cliSessionWriterEvidence struct {
	// validID accepts an id of this engine's shape only (never one that could
	// name a path or start with a flag).
	validID func(id string) bool
	// isAgentProcess reports whether a process name (cliSessionProcessName)
	// is this engine's CLI.
	isAgentProcess func(name string) bool
	// storeClock returns, for one session, what reports when its own store was
	// last written (unix ms, 0 unknown): resolved once, then cheap to call.
	// nil: no store this agent can stat (Muse Code).
	storeClock func(sessionID string) func() int64
}

// cliSessionWriterEvidenceFor is the engine's evidence (not Claude Code's,
// whose registry says more).
func cliSessionWriterEvidenceFor(engine string) (cliSessionWriterEvidence, bool) {
	switch engine {
	case cliSessionEngineCodex:
		return codexWriterEvidence, true
	case cliSessionEngineGrok:
		return grokWriterEvidence, true
	case cliSessionEngineAntigravity:
		return antigravityWriterEvidence, true
	case cliSessionEngineOpenCode:
		return openCodeWriterEvidence, true
	case cliSessionEngineMuseCode:
		return museCodeWriterEvidence, true
	}
	return cliSessionWriterEvidence{}, false
}

// cliSessionOutsideWriters is what the watcher reads for one poll: every live
// process of the engine's CLI naming sessionID that is not this agent's own
// (idle: open elsewhere), and — when the store was written within
// writerQuietMs by anything but AI Expedite's own child — a writer mid-turn
// (busy; pid 0 when no process names the session, an IDE's app-server say).
func cliSessionOutsideWriters(ev cliSessionWriterEvidence, sessionID string, clock func() int64, ownActiveAt func(time.Time) time.Time, now time.Time) []cliSessionWriter {
	writers := cliSessionProcessWriters(ev, sessionID)
	if clock == nil || !cliSessionStoreWrittenByOther(clock(), now, ownActiveAt) {
		return writers
	}
	if len(writers) == 0 {
		return []cliSessionWriter{{Status: cliSessionRunBusy}}
	}
	for i := range writers {
		writers[i].Status = cliSessionRunBusy
	}
	return writers
}

// cliSessionStoreWrittenByOther reports whether a store written at writtenMs
// was written by another program mid-turn: within writerQuietMs of now, and
// later than AI Expedite's own child could have written it.
func cliSessionStoreWrittenByOther(writtenMs int64, now time.Time, ownActiveAt func(time.Time) time.Time) bool {
	if writtenMs <= 0 {
		return false
	}
	written := time.UnixMilli(writtenMs)
	if now.Sub(written) >= cliSessionWriterQuiet {
		return false
	}
	if ownActiveAt != nil {
		if own := ownActiveAt(now); !own.IsZero() && !written.After(own.Add(cliSessionOwnWriteSlack)) {
			return false
		}
	}
	return true
}

// cliSessionProcessWriters are the live processes of the engine's CLI whose
// argv names sessionID, this agent's own descendants excluded, as writers
// holding it open (status idle: the process table cannot say more). The probe
// reports these; its transcriptWrittenAt tells terminal-service the rest.
func cliSessionProcessWriters(ev cliSessionWriterEvidence, sessionID string) []cliSessionWriter {
	writers := []cliSessionWriter{}
	procs, _ := cliSessionAgentProcesses()
	for _, p := range procs {
		if p.Own || !ev.isAgentProcess(p.Name) || !cliSessionArgvNames(p.Args, sessionID) {
			continue
		}
		writers = append(writers, cliSessionWriter{PID: p.PID, Status: cliSessionRunIdle})
	}
	return writers
}

// cliSessionArgvNames reports whether argv names id as a whole argument
// (`resume <id>`) or as a flag's value (`--session=<id>`). argv[0] is the
// program, never a session.
func cliSessionArgvNames(argv []string, id string) bool {
	if id == "" {
		return false
	}
	for i, a := range argv {
		if i == 0 {
			continue
		}
		if a == id || strings.HasSuffix(a, "="+id) {
			return true
		}
	}
	return false
}

// cliSessionOwnIO records when AI Expedite's own child of a session last read
// or wrote a frame: a store write right after it is that child's own.
type cliSessionOwnIO struct{ at atomic.Int64 }

func (o *cliSessionOwnIO) note() { o.at.Store(time.Now().UnixMilli()) }

func (o *cliSessionOwnIO) lastAt(time.Time) time.Time {
	ms := o.at.Load()
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// newestFileMs is the newest modification time among paths (unix ms, 0 when
// none exists).
func newestFileMs(paths ...string) int64 {
	var newest int64
	for _, p := range paths {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil {
			if ms := fi.ModTime().UnixMilli(); ms > newest {
				newest = ms
			}
		}
	}
	return newest
}

/* -------------------------------------------------------------------------- */
/* The process table                                                          */
/* -------------------------------------------------------------------------- */

// cliSessionProcess is one live process of a coding agent's CLI.
type cliSessionProcess struct {
	PID  int
	Name string
	Args []string
	// Own: this agent process or one of its descendants (a session AI
	// Expedite runs), never another writer.
	Own bool
}

// cliSessionIsAgentProcessName reports whether a process name is any
// engine's CLI: the union the one shared scan keeps.
func cliSessionIsAgentProcessName(name string) bool {
	for _, ev := range []cliSessionWriterEvidence{codexWriterEvidence, grokWriterEvidence, antigravityWriterEvidence, openCodeWriterEvidence, museCodeWriterEvidence} {
		if ev.isAgentProcess(name) {
			return true
		}
	}
	return false
}

// cliSessionProcessName is a process name as the evidence compares it: base
// name, lower case, without a Windows `.exe`.
func cliSessionProcessName(name string) string {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(name)))
	return strings.TrimSuffix(base, ".exe")
}

// cliSessionNameIn reports whether name is one of names.
func cliSessionNameIn(names ...string) func(string) bool {
	return func(name string) bool {
		for _, n := range names {
			if name == n {
				return true
			}
		}
		return false
	}
}

// scanCliSessionProcesses lists the live processes whose name is an agent's
// CLI, with their argv. ok is false when the table could not be read in full.
func scanCliSessionProcesses(ctx context.Context) ([]cliSessionProcess, bool) {
	cliSessionProcessScannerMu.RLock()
	scan := cliSessionProcessScanner
	cliSessionProcessScannerMu.RUnlock()
	return scan(ctx)
}

// cliSessionProcessScanner is scanCliSessionProcesses' source, swappable by a
// test under its lock (watchers read it from their own goroutines).
var (
	cliSessionProcessScannerMu sync.RWMutex
	cliSessionProcessScanner   = defaultScanCliSessionProcesses
)

func defaultScanCliSessionProcesses(ctx context.Context) ([]cliSessionProcess, bool) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, false
	}
	self := int32(os.Getpid())
	out := []cliSessionProcess{}
	for _, p := range procs {
		if ctx.Err() != nil {
			return out, false
		}
		if p.Pid == self {
			continue
		}
		name, err := p.NameWithContext(ctx)
		if err != nil {
			continue
		}
		base := cliSessionProcessName(name)
		if !cliSessionIsAgentProcessName(base) {
			continue
		}
		args, err := p.CmdlineSliceWithContext(ctx)
		if err != nil || len(args) == 0 {
			continue
		}
		out = append(out, cliSessionProcess{PID: int(p.Pid), Name: base, Args: args, Own: cliSessionDescendsFrom(ctx, p, self)})
	}
	return out, true
}

// cliSessionDescendsFrom reports whether p is a descendant of pid ancestor,
// walking at most cliSessionProcessAncestryDepth parents.
func cliSessionDescendsFrom(ctx context.Context, p *process.Process, ancestor int32) bool {
	cur := p
	for i := 0; i < cliSessionProcessAncestryDepth && cur != nil; i++ {
		ppid, err := cur.PpidWithContext(ctx)
		if err != nil || ppid <= 1 {
			return false
		}
		if ppid == ancestor {
			return true
		}
		next, err := process.NewProcessWithContext(ctx, ppid)
		if err != nil {
			return false
		}
		cur = next
	}
	return false
}

// cliSessionProcessSnapshot is the last scan, shared by every watcher and
// probe for cliSessionProcessSnapshotTTL.
var cliSessionProcessSnapshot struct {
	sync.Mutex
	at    time.Time
	procs []cliSessionProcess
	ok    bool
}

// cliSessionAgentProcesses is the agents' live processes, scanned at most
// once per cliSessionProcessSnapshotTTL.
func cliSessionAgentProcesses() ([]cliSessionProcess, bool) {
	cliSessionProcessSnapshot.Lock()
	defer cliSessionProcessSnapshot.Unlock()
	if !cliSessionProcessSnapshot.at.IsZero() && time.Since(cliSessionProcessSnapshot.at) < cliSessionProcessSnapshotTTL {
		return cliSessionProcessSnapshot.procs, cliSessionProcessSnapshot.ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliSessionProcessScanBudget)
	defer cancel()
	procs, ok := scanCliSessionProcesses(ctx)
	cliSessionProcessSnapshot.at, cliSessionProcessSnapshot.procs, cliSessionProcessSnapshot.ok = time.Now(), procs, ok
	return procs, ok
}

// resetCliSessionProcessSnapshot forgets the shared scan (tests).
func resetCliSessionProcessSnapshot() {
	cliSessionProcessSnapshot.Lock()
	cliSessionProcessSnapshot.at, cliSessionProcessSnapshot.procs, cliSessionProcessSnapshot.ok = time.Time{}, nil, false
	cliSessionProcessSnapshot.Unlock()
}

// cliSessionRunningByID maps every session id a live process of the engine's
// CLI names (any argv token of the id's shape) to that process, for a
// discovery row's `running`.
func cliSessionRunningByID(ev cliSessionWriterEvidence) map[string]*cliSessionRunning {
	out := map[string]*cliSessionRunning{}
	procs, _ := cliSessionAgentProcesses()
	for _, p := range procs {
		if p.Own || !ev.isAgentProcess(p.Name) {
			continue
		}
		for i, a := range p.Args {
			if i == 0 {
				continue
			}
			if _, v, found := strings.Cut(a, "="); found && strings.HasPrefix(a, "-") {
				a = v
			}
			if ev.validID(a) {
				out[a] = &cliSessionRunning{Status: cliSessionRunIdle, PID: p.PID}
			}
		}
	}
	return out
}

/* -------------------------------------------------------------------------- */
/* Where a session is resumed in place                                        */
/* -------------------------------------------------------------------------- */

// configAgentID is this agent's registered id, else fallback (the id a
// command was addressed to).
func configAgentID(cfg *Config, fallback string) string {
	if cfg != nil {
		id := ""
		cfg.WithPersistenceLock(func() { id = cfg.AgentID })
		if id != "" {
			return id
		}
	}
	return fallback
}

// cliSessionResumedID is the session a client frame resumes on an ACP or MSP
// child or a Codex app-server, or "": ACP `session/load` / `session/resume`,
// MSP `session/resume`, Codex `thread/resume` — each names the session it
// continues in its params. Only the envelope and that one param are read.
func cliSessionResumedID(frame string, methods ...string) string {
	named := false
	for _, m := range methods {
		named = named || strings.Contains(frame, `"`+m+`"`)
	}
	if !named {
		return "" // most frames (a prompt, an answer) are never parsed again
	}
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			ThreadID  string `json:"threadId"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(frame), &rpc) != nil || !containsString(methods, rpc.Method) {
		return ""
	}
	if rpc.Method == codexMethodThreadResume {
		return rpc.Params.ThreadID
	}
	return rpc.Params.SessionID
}

// codexMethodThreadResume is the Codex app-server request that continues an
// existing thread (agent-orchestrator-service codexConversation.service.js
// sends it with `params.threadId`).
const codexMethodThreadResume = "thread/resume"

// acpProfileEngine is the discovery engine an ACP-family profile drives.
func acpProfileEngine(p *acpAgentProfile) string {
	if p == nil {
		return ""
	}
	switch p.cliAgentID {
	case grokCliAgentID:
		return cliSessionEngineGrok
	case opencodeCliAgentID:
		return cliSessionEngineOpenCode
	case antigravityCliAgentID:
		return cliSessionEngineAntigravity
	case museCodeCliAgentID:
		return cliSessionEngineMuseCode
	}
	return ""
}

// acpResumeMethods are the frames that continue an existing session on the
// profile's protocol.
func acpResumeMethods(p *acpAgentProfile) []string {
	if p != nil && p.protocol == acpProtocolMSP {
		return []string{mspMethodSessionResume}
	}
	return []string{acpMethodSessionLoad, acpMethodSessionResume}
}

// prepareACPResumeFrame runs before a client frame is written to an ACP child:
// a Grok `session/load` of a session AI Expedite's isolated Grok home cannot
// see links that one session's folder in (linkAdoptedGrokSession). A link
// that cannot be made is logged and the frame still goes: Grok then answers
// the load as it always did, and adoption reports it unsupported.
func prepareACPResumeFrame(p *acpAgentProfile, frame string) {
	if p == nil || p.cliAgentID != grokCliAgentID || !strings.Contains(frame, `"`+acpMethodSessionLoad+`"`) {
		return
	}
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(frame), &rpc) != nil || rpc.Method != acpMethodSessionLoad {
		return
	}
	linked, err := linkAdoptedGrokSession(rpc.Params.SessionID, rpc.Params.Cwd)
	switch {
	case err != nil:
		logCliSessionOutsideWriter("[grok-acp] adopted session folder not linked: %v", err)
	case linked:
		logCliSessionOutsideWriter("[grok-acp] linked one adopted session folder into the conversation store")
	}
}

// noteACPResumeWritten runs once a client frame reached an ACP or MSP child:
// a frame that resumes a session starts that session's watcher, for as long
// as the child runs.
func (m *ACPManager) noteACPResumeWritten(session *ACPSession, frame string) {
	if m == nil || session == nil {
		return
	}
	engine := acpProfileEngine(m.profile)
	id := cliSessionResumedID(frame, acpResumeMethods(m.profile)...)
	if engine == "" || id == "" {
		return
	}
	startCliSessionOutsideWriterWatcher(cliOutsideWriterTarget{
		engine:      engine,
		sessionID:   id,
		owner:       cliOutsideWriterOwner{ID: session.ID, WorkspaceID: session.WorkspaceID, UID: session.UID},
		agentID:     configAgentID(m.Config, ""),
		stop:        session.processExited,
		ownActiveAt: session.ownIO.lastAt,
	}, session.publish)
}

// noteCodexResumeWritten runs once a client frame reached a Codex
// app-server: `thread/resume` starts that thread's watcher.
func (m *CodexAppServerManager) noteCodexResumeWritten(session *CodexAppServerSession, frame string) {
	if m == nil || session == nil {
		return
	}
	id := cliSessionResumedID(frame, codexMethodThreadResume)
	if id == "" {
		return
	}
	startCliSessionOutsideWriterWatcher(cliOutsideWriterTarget{
		engine:      cliSessionEngineCodex,
		sessionID:   id,
		owner:       cliOutsideWriterOwner{ID: session.ID, WorkspaceID: session.WorkspaceID, UID: session.UID},
		agentID:     configAgentID(m.Config, ""),
		stop:        session.processExited,
		ownActiveAt: session.ownIO.lastAt,
	}, session.publish)
}

// oneShotTurnState is a one-shot manager's view of a session: still
// registered, and whether a turn of it runs now.
type oneShotTurnState interface {
	TurnState(id string) (alive, running bool)
}

func (m *oneShotNativeManager) TurnState(id string) (alive, running bool) {
	s := m.Get(id)
	if s == nil {
		return false, false
	}
	st := s.Status()
	return st != "ended", st == "running"
}

func (m *OpenCodeNativeManager) TurnState(id string) (alive, running bool) {
	s := m.Get(id)
	if s == nil {
		return false, false
	}
	st := s.Status()
	return st != "ended", st == "running"
}

func (m *AntigravityNativeManager) TurnState(id string) (alive, running bool) {
	s := m.Get(id)
	if s == nil {
		return false, false
	}
	st := s.Status()
	return st != "ended", st == "running"
}

// watchOneShotResumedSession starts the watcher of a one-shot session a START
// seeded with the conversation it continues (the signed `conversationId`):
// between turns no process runs, so the session's own manager says when it
// ended and when a turn of AI Expedite's own runs.
func watchOneShotResumedSession(engine string, cmd commandMsg, mgr oneShotTurnState, publishFn PublishFunc) bool {
	if mgr == nil || cmd.ConversationID == "" || cmd.SessionID == "" {
		return false
	}
	var lastRunning atomic.Int64
	return startCliSessionOutsideWriterWatcher(cliOutsideWriterTarget{
		engine:    engine,
		sessionID: cmd.ConversationID,
		owner:     cliOutsideWriterOwner{ID: cmd.SessionID, WorkspaceID: cmd.WorkspaceID, UID: cmd.UID},
		agentID:   cmd.AgentID,
		alive: func() bool {
			alive, _ := mgr.TurnState(cmd.SessionID)
			return alive
		},
		ownActiveAt: func(now time.Time) time.Time {
			if _, running := mgr.TurnState(cmd.SessionID); running {
				lastRunning.Store(now.UnixMilli())
				return now
			}
			if ms := lastRunning.Load(); ms > 0 {
				return time.UnixMilli(ms)
			}
			return time.Time{}
		},
	}, publishFn)
}

// oneShotEngineFor is the discovery engine of a one-shot native kind's frame
// prefix ("" for one without).
func oneShotEngineFor(framePrefix string) string {
	switch framePrefix {
	case "opencode_native":
		return cliSessionEngineOpenCode
	case "musecode_native":
		return cliSessionEngineMuseCode
	case "antigravity_native":
		return cliSessionEngineAntigravity
	}
	return ""
}

// logCliSessionOutsideWriter prints a fixed line (its errors name no path,
// title or id).
func logCliSessionOutsideWriter(format string, args ...any) {
	fmt.Printf(colorYellow+format+colorReset+"\n", args...)
}
