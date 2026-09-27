// cliagent_usage_refresh_ladder.go — the rung arithmetic shared by every
// provider's run-refresh debt schedule.
//
// Antigravity (cliagent_usage_antigravity_refresh_schedule.go) and Codex
// (cliagent_usage_codex_refresh_schedule.go) book their retries off the same
// two rules: index a ladder by the attempts already booked, and back a
// budget-free refusal off with the debt's age. The ladder VALUES differ per
// provider — Codex reads local telemetry first and so starts much shorter —
// but the arithmetic must not, or the two drift on a bug fixed in one.
//
// Pure: no state, no I/O, no clock.
package main

import "time"

// refreshRetryDelayForAttempt is the ladder: the delay before the next attempt
// of a debt that has already booked `attempts` of its `max` budget, or false
// once that budget is spent. Rung 0 and rung 1 are the same entry — the first
// attempt is the settle's own, so the first BOOKED rung is the ladder's head.
func refreshRetryDelayForAttempt(attempts, max int, ladder []time.Duration) (time.Duration, bool) {
	if attempts >= max || len(ladder) == 0 {
		return 0, false
	}
	rung := attempts - 1
	if rung < 0 {
		rung = 0
	}
	if rung >= len(ladder) {
		rung = len(ladder) - 1
	}
	return ladder[rung], true
}

// refreshFreeRetryDelay is the rung after a refusal that spent no outbound
// read, for a debt owed for `owedFor`. Such refusals spend no budget, so the
// budget cannot bound them; the delay grows with the debt's age instead — at
// least `floor`, at most the ladder's longest rung. Without that, a device that
// stays offline would re-check at the floor for the debt's whole age-out
// window, where this costs a couple of dozen checks across hours.
func refreshFreeRetryDelay(owedFor, floor time.Duration, ladder []time.Duration) time.Duration {
	delay := floor
	if owedFor > delay {
		delay = owedFor
	}
	if n := len(ladder); n > 0 && delay > ladder[n-1] {
		delay = ladder[n-1]
	}
	return delay
}
