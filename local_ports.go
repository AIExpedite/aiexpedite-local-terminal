// File: local_ports.go
// Per-release-channel defaults for everything the agent binds on the LOCAL
// machine: the ttyd web-terminal port, the browser-identity port and the tmux
// session name.
//
// The four channels (prod / dev / stg / beta) are separate app bundles with
// separate config directories (EnvConfigSuffix, see paths.go), so a developer
// legitimately runs two or more of them side by side on one machine. Every
// machine-local resource therefore has to be keyed by channel, in ONE place —
// the 2026-09-09 outage came from the identity port being per-channel while
// the ttyd port and the tmux session name were not: the prod and dev agents
// relaunched in the same second, dev won the shared `agent` tmux session and
// port 7681, and prod aborted StartAgent before it ever reached Pub/Sub — the
// tray icon stayed up while the backend showed the device Disconnected for
// hours, because the abort happened AFTER the shutdown notice and BEFORE the
// reconnect notice.
package main

import "fmt"

// localChannelDefaults is the set of machine-local resources a channel owns.
type localChannelDefaults struct {
	ttydPort     int
	identityPort int
	tmuxSession  string
}

// localDefaultsByEnv is the single source for per-channel local resources.
// Add a channel here and nowhere else. The ttyd ports are spaced by ten so a
// user who overrides `local_ttyd_port` by hand has room to move within a
// channel without landing on another channel's default, and none of them
// collides with an identity port (asserted by TestLocalChannelDefaultsAreDisjoint).
//
// prod keeps 7681 / `agent`: those are the values every installed prod agent
// already uses and the ones the frontend's Open Terminal links were built for.
var localDefaultsByEnv = map[string]localChannelDefaults{
	"prod": {ttydPort: 7681, identityPort: 7682, tmuxSession: "agent"},
	"dev":  {ttydPort: 7691, identityPort: 7683, tmuxSession: "agent-dev"},
	"stg":  {ttydPort: 7701, identityPort: 7684, tmuxSession: "agent-stg"},
	"beta": {ttydPort: 7711, identityPort: 7685, tmuxSession: "agent-beta"},
}

// legacySharedTtydPort is the ttyd port every channel defaulted to before the
// defaults became per-channel. A non-prod config still pinned to it was
// written by DefaultConfig() of an older build, not chosen by the user, and is
// migrated on load (see migrateLegacyTtydPort).
const legacySharedTtydPort = 7681

// defaultTtydPortFor returns the ttyd port a fresh install of the given
// channel binds. Unknown channels fall back to prod's port so a mis-built
// binary still comes up rather than binding port 0.
func defaultTtydPortFor(environment string) int {
	if defaults, ok := localDefaultsByEnv[environment]; ok {
		return defaults.ttydPort
	}
	return localDefaultsByEnv["prod"].ttydPort
}

// tmuxSessionNameFor returns the tmux session the given channel owns. Unknown
// channels get a suffixed name rather than prod's bare `agent` so an odd build
// can never steal — or, on shutdown, kill — the production session.
func tmuxSessionNameFor(environment string) string {
	if defaults, ok := localDefaultsByEnv[environment]; ok {
		return defaults.tmuxSession
	}
	return fmt.Sprintf("agent-%s", environment)
}

// browserIdentityPort returns the loopback port the given channel's browser
// identity endpoint listens on; ok is false for a channel that has none.
func browserIdentityPort(environment string) (int, bool) {
	defaults, ok := localDefaultsByEnv[environment]
	return defaults.identityPort, ok
}

// resolvedTtydPort maps the on-disk `local_ttyd_port` to the port the running
// channel binds: an explicit value wins, absence means the channel default.
func resolvedTtydPort(configured int) int {
	if configured == 0 {
		return defaultTtydPortFor(EnvName)
	}
	return configured
}

// migrateLegacyTtydPort decides whether a loaded config's ttyd port is the
// pre-per-channel default left behind on a non-prod channel. It returns the
// port to use and whether that differs from what was on disk. Pure so every
// channel is testable from one host.
func migrateLegacyTtydPort(environment string, configured int) (port int, migrated bool) {
	if environment == "prod" || configured != legacySharedTtydPort {
		return configured, false
	}
	return defaultTtydPortFor(environment), true
}
