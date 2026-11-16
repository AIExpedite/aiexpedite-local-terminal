// browser_commands.go — the signed `__browser__` built-in: the AI working in
// the person's own Chrome.
//
//	__browser__ [<params JSON>]   {v, commandId, op, deadlineMs, tabId?, ...op fields}
//
// pubsub.go dispatches it after the signature, staleness and rate checks (stop
// is exempt from the rate limiter and passes a drain). The handler only parses
// and hands the op to a goroutine, so the Pub/Sub callback never waits on
// Chrome; the result goes back over POST /device/:agentId/browser/result, not
// the results topic. Ops on one tab run one at a time. A command id is run at
// most once per process (Pub/Sub redelivers), and a submit request at most
// once. Nothing the page shows and no value the model sent is ever logged.
package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type browserDispatcher struct {
	mu      sync.Mutex
	seen    map[string]time.Time // commandId -> first seen
	submits map[string]time.Time // requestId -> first submit
	active  map[string]*atomic.Bool
	tabs    map[string]*browserTabLock

	poster func() browserPoster
	route  func(ctx context.Context, tabRoute string) (browserRouteChoice, *browserError)
	open   func(ctx context.Context, c browserRouteChoice) (browserBackend, func(), error)
	now    func() time.Time
	// done is called after a command's result was posted (tests wait on it).
	done func(commandID string)
}

type browserTabLock struct {
	ch    chan struct{}
	users int
}

// browserSeenTTL: how long a command id / submit request id is remembered.
const browserSeenTTL = 30 * time.Minute

var globalBrowserDispatcher = newBrowserDispatcher()

func newBrowserDispatcher() *browserDispatcher {
	return &browserDispatcher{
		seen:    map[string]time.Time{},
		submits: map[string]time.Time{},
		active:  map[string]*atomic.Bool{},
		tabs:    map[string]*browserTabLock{},
		route: func(ctx context.Context, tabRoute string) (browserRouteChoice, *browserError) {
			return chooseBrowserRoute(ctx, runtime.GOOS, tabRoute, discoverChromeDevTools)
		},
		open: openBrowserBackend,
		now:  time.Now,
	}
}

// browserPosterFor is set once the agent is configured (StartAgent).
var browserPosterFor func() browserPoster

func openBrowserBackend(ctx context.Context, c browserRouteChoice) (browserBackend, func(), error) {
	if c.Route == browserRouteAppleScript {
		return &asBackend{b: globalASBridge}, func() {}, nil
	}
	conn, release, err := globalCDPManager.acquire(ctx, c.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	return &cdpBackend{conn: conn}, release, nil
}

func logBrowser(commandID, msg string) {
	fmt.Printf("[browser] %s %s\n", commandID, msg)
}

// handleBrowserCommand runs a verified `__browser__` command. It never blocks
// on Chrome; the caller acks the message when it returns.
func handleBrowserCommand(cmd commandMsg) {
	globalBrowserDispatcher.handle(cmd)
}

// ActiveCount is the number of browser ops in flight (drain accounting).
func (d *browserDispatcher) ActiveCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.active)
}

func (d *browserDispatcher) handle(cmd commandMsg) {
	start := d.now()
	p, id, berr := parseBrowserCommand(cmd, start)
	if id == "" {
		logBrowser("-", "refused: "+berr.Code)
		return
	}
	if !d.firstSight(id, start) {
		logBrowser(id, "duplicate delivery ignored")
		return
	}
	if berr != nil {
		logBrowser(id, "refused: "+berr.Code)
		d.finish(id, start, "", nil, berr)
		return
	}
	if p.Op == browserOpStop {
		n := d.stopAll()
		logBrowser(id, fmt.Sprintf("stop: %d op(s) told to stop", n))
		d.finish(id, start, "", map[string]any{"stopped": true}, nil)
		return
	}
	deadline := time.UnixMilli(p.DeadlineMs)
	if max := start.Add(browserMaxOpDuration); deadline.After(max) {
		deadline = max
	}
	if !deadline.After(start.Add(browserResultMargin)) {
		d.finish(id, start, "", nil, browserErr(browserErrCommandTimeout, "the command arrived after its deadline"))
		return
	}
	if p.Op == browserOpSubmit && !d.claimSubmit(p.RequestID, start) {
		logBrowser(id, "submit refused: request already submitted once")
		d.finish(id, start, "", nil, browserErr(browserErrCommandInvalid, "this submit request was already sent to the page once"))
		return
	}
	stop := &atomic.Bool{}
	d.mu.Lock()
	d.active[id] = stop
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.active, id)
			d.mu.Unlock()
		}()
		d.execute(id, start, deadline, p, stop)
	}()
}

// refuseWhileDraining answers a browser command the update drain refused, so
// the person hears why at once instead of waiting for the command to expire.
func (d *browserDispatcher) refuseWhileDraining(cmd commandMsg) {
	start := d.now()
	_, id, _ := parseBrowserCommand(cmd, start)
	if id == "" || !d.firstSight(id, start) {
		return
	}
	d.finish(id, start, "", nil, browserErr(browserErrDeviceOffline, "the desktop app is updating; try again in a minute"))
}

func (d *browserDispatcher) firstSight(id string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, t := range d.seen {
		if now.Sub(t) > browserSeenTTL {
			delete(d.seen, k)
		}
	}
	if _, dup := d.seen[id]; dup {
		return false
	}
	d.seen[id] = now
	return true
}

func (d *browserDispatcher) claimSubmit(requestID string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, t := range d.submits {
		if now.Sub(t) > browserSeenTTL {
			delete(d.submits, k)
		}
	}
	if _, dup := d.submits[requestID]; dup {
		return false
	}
	d.submits[requestID] = now
	return true
}

// stopAll tells every op in flight to stop. A fill stops before its next field
// (or mid-field, leaving that field alone); the other ops are short and finish.
func (d *browserDispatcher) stopAll() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.active {
		s.Store(true)
	}
	return len(d.active)
}

func (d *browserDispatcher) lockTab(ctx context.Context, tabID string) (func(), error) {
	d.mu.Lock()
	l := d.tabs[tabID]
	if l == nil {
		l = &browserTabLock{ch: make(chan struct{}, 1)}
		d.tabs[tabID] = l
	}
	l.users++
	d.mu.Unlock()
	drop := func() {
		d.mu.Lock()
		l.users--
		if l.users == 0 {
			delete(d.tabs, tabID)
		}
		d.mu.Unlock()
	}
	select {
	case l.ch <- struct{}{}:
		return func() { <-l.ch; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

func (d *browserDispatcher) execute(id string, start, deadline time.Time, p browserParams, stop *atomic.Bool) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-browserResultMargin))
	defer cancel()
	if p.TabID != "" {
		unlock, err := d.lockTab(ctx, p.TabID)
		if err != nil {
			d.finish(id, start, "", nil, browserErr(browserErrCommandTimeout, "another browser action on this tab did not finish in time"))
			return
		}
		defer unlock()
	}
	tabRoute, _, _ := parseBrowserTabID(p.TabID)
	choice, berr := d.route(ctx, tabRoute)
	if berr != nil {
		d.finish(id, start, "", nil, berr)
		return
	}
	be, release, err := d.open(ctx, choice)
	if err != nil {
		d.finish(id, start, choice.Route, nil, classifyBrowserOpenError(err, ctx))
		return
	}
	run := &browserOpRun{p: p, be: be, stop: stop}
	result, err := run.run(ctx)
	release()
	if err != nil {
		d.finish(id, start, choice.Route, nil, classifyBrowserError(err, ctx))
		return
	}
	if run.shot != nil {
		pctx, pcancel := context.WithTimeout(context.Background(), browserResultRetryWindow+10*time.Second)
		err := d.posterNow().PostScreenshot(pctx, id, run.shot.Data, run.shot.ContentType)
		pcancel()
		if err != nil {
			logBrowser(id, "screenshot upload failed: "+err.Error())
			d.finish(id, start, choice.Route, nil, browserErr(browserErrScreenshotNotFound, "the screenshot could not be uploaded"))
			return
		}
	}
	d.finish(id, start, choice.Route, result, nil)
}

func (d *browserDispatcher) posterNow() browserPoster {
	if d.poster != nil {
		return d.poster()
	}
	if browserPosterFor != nil {
		return browserPosterFor()
	}
	return nil
}

// finish posts the command's result (contract 2a), on its own goroutine when
// called from the Pub/Sub callback.
func (d *browserDispatcher) finish(id string, start time.Time, route string, result any, berr *browserError) {
	body := browserResultBody{V: browserParamsVersion, CommandID: id, OK: berr == nil, Route: route, Ms: d.now().Sub(start).Milliseconds()}
	if berr != nil {
		body.Error = berr
	} else {
		body.Result = result
	}
	body = fitBrowserResult(body)
	post := func() {
		code := ""
		if body.Error != nil {
			code = " code=" + body.Error.Code
		}
		poster := d.posterNow()
		if poster == nil {
			logBrowser(id, "no result poster (agent not registered)")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), browserResultRetryWindow+10*time.Second)
			err := poster.PostResult(ctx, body)
			cancel()
			if err != nil {
				logBrowser(id, "result post failed: "+err.Error())
			}
		}
		logBrowser(id, fmt.Sprintf("route=%s ok=%v%s ms=%d", orDash(route), body.OK, code, body.Ms))
		if d.done != nil {
			d.done(id)
		}
	}
	go post()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// classifyBrowserOpenError: failures reaching Chrome at all.
func classifyBrowserOpenError(err error, ctx context.Context) *browserError {
	var be *browserError
	switch {
	case errors.As(err, &be):
		return be
	case errors.Is(err, errCDPDenied):
		return browserErr(browserErrChromeDenied, "Chrome's \"Allow remote debugging?\" was declined")
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		return browserErr(browserErrChromeDenied, "Chrome's \"Allow remote debugging?\" was not answered in time")
	}
	return browserErr(browserErrChromeNotReachable, "could not connect to Chrome's remote debugging")
}

// classifyBrowserError maps what went wrong during an op to a BROWSER_* code.
func classifyBrowserError(err error, ctx context.Context) *browserError {
	var be *browserError
	switch {
	case errors.As(err, &be):
		return be
	case isNotResponding(err), errors.Is(err, errGaveUp):
		return browserErr(browserErrPageNotResponding, "the page stopped responding (its scripts may be stuck)")
	case errors.Is(err, errCDPDenied):
		return browserErr(browserErrChromeDenied, "Chrome's \"Allow remote debugging?\" was declined")
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return browserErr(browserErrCommandTimeout, "the browser action did not finish before its deadline")
	case isKitError(err):
		return browserErr(browserErrPageNotResponding, "the page did not answer as expected")
	}
	return browserErr(browserErrChromeNotReachable, "lost the connection to Chrome")
}

func isKitError(err error) bool {
	s := err.Error()
	return len(s) >= 4 && (s[:4] == "kit " || s == "unreadable reply from the page" || s == "the kit raised an exception in the page")
}

// shutdownBrowserControl closes the DevTools connection and the AppleScript
// helper (agent exit).
func shutdownBrowserControl() {
	globalCDPManager.closeAll()
	globalASBridge.close()
}
