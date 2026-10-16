// Package monitor owns the server state. It polls the server with the ping,
// runs the start and stop flows, detects crashes and stops an empty server.
// Every state change goes through a Monitor.
package monitor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/EdoUbba/minecraft-server-discord-bot/internal/control"
	"github.com/EdoUbba/minecraft-server-discord-bot/internal/minecraft"
)

type State int

const (
	Off State = iota
	Starting
	Online
	Stopping
	Unresponsive
)

func (s State) String() string {
	switch s {
	case Off:
		return "off"
	case Starting:
		return "starting"
	case Online:
		return "online"
	case Stopping:
		return "stopping"
	case Unresponsive:
		return "unresponsive"
	}
	return "unknown"
}

// EventKind is something worth a message in the log channel.
type EventKind int

const (
	EventOnline       EventKind = iota // the first successful ping after a start
	EventStartTimeout                  // no ping answer within the start timeout
	EventStopped                       // a stop flow finished
	EventCrashed                       // the service stopped while the server was online
	EventUnresponsive                  // the ping fails but the service is still active
	EventAutostop                      // the server was empty for too long; a stop follows
	EventHangStop                      // the server stayed unresponsive; a stop follows
)

type Event struct {
	Kind   EventKind
	Server string // folder name of the selected server
}

// Snapshot is the state shown to users.
type Snapshot struct {
	State      State
	Server     string    // folder name of the selected server
	Since      time.Time // when the server came online; zero when off
	Players    int
	MaxPlayers int
	Version    string
}

// ErrState means the action is not allowed in the current state.
var ErrState = errors.New("action not allowed in the current state")

// stopPoll is how often the stop flow checks the service with SSH.
const stopPoll = 5 * time.Second

// hangGrace is how long a server that stops answering while online gets
// before it is stopped, so a short lag spike does not stop it.
const hangGrace = 30 * time.Second

// stopMessage is announced to the players before a stop.
const stopMessage = "Il server si sta spegnendo."

type Pinger interface {
	Ping(ctx context.Context) (minecraft.Status, error)
}

type RCON interface {
	ListPlayers(ctx context.Context) ([]string, error)
	Say(ctx context.Context, message string) error
	Stop(ctx context.Context) error
}

type Control interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) (control.Status, error)
	List(ctx context.Context) ([]string, error)
	Select(ctx context.Context, name string) error
}

type Options struct {
	Interval     time.Duration
	StartTimeout time.Duration
	StopTimeout  time.Duration
	EmptyTimeout time.Duration // 0 disables the automatic stop
	Notify       func(Event)   // may be nil; must not block
}

type Monitor struct {
	ping Pinger
	rcon RCON
	ctl  Control
	opts Options

	// Replaced by tests.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	mu        sync.Mutex
	state     State
	server    string
	startedAt time.Time // when the start was requested, for the start timeout
	since     time.Time // when the server came online, for the uptime
	lastSeen  time.Time // last ping with players, or when the server came online
	players   int
	max       int
	version   string
}

func New(ping Pinger, rcon RCON, ctl Control, opts Options) *Monitor {
	return &Monitor{ping: ping, rcon: rcon, ctl: ctl, opts: opts, now: time.Now, sleep: sleep}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Init reads the service state once with SSH, so a bot restart picks up a
// running server with its real uptime. Call it before Run.
func (m *Monitor) Init(ctx context.Context) error {
	st, err := m.ctl.Status(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.server = st.Server
	if !stopped(st.ActiveState) {
		// The next successful ping makes it online, with the uptime counted
		// from when the service started.
		m.state, m.startedAt, m.since = Starting, st.Since, st.Since
	}
	return nil
}

// Run pings the server right away and then every Interval, until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.opts.Interval)
	defer t.Stop()
	for {
		m.Tick(ctx)
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

func (m *Monitor) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{State: m.state, Server: m.server}
	if m.state == Online || m.state == Unresponsive {
		s.Since = m.since
	}
	if m.state == Online {
		s.Players, s.MaxPlayers, s.Version = m.players, m.max, m.version
	}
	return s
}

// Start starts the service. The server becomes online at the first
// successful ping.
func (m *Monitor) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.state != Off {
		m.mu.Unlock()
		return ErrState
	}
	m.state, m.startedAt, m.since = Starting, m.now(), time.Time{}
	m.mu.Unlock()

	if err := m.ctl.Start(ctx); err != nil {
		m.set(Off)
		return err
	}
	return nil
}

// List returns the servers that can be selected.
func (m *Monitor) List(ctx context.Context) ([]string, error) {
	return m.ctl.List(ctx)
}

// Select makes name the active server. It is refused unless the server is
// off; minecraft-ctl checks the service state again.
func (m *Monitor) Select(ctx context.Context, name string) error {
	m.mu.Lock()
	state := m.state
	m.mu.Unlock()
	if state != Off {
		return ErrState
	}
	if err := m.ctl.Select(ctx, name); err != nil {
		return err
	}
	m.mu.Lock()
	m.server = name
	m.mu.Unlock()
	return nil
}

// Players returns the names of the online players, through RCON.
func (m *Monitor) Players(ctx context.Context) ([]string, error) {
	if m.Snapshot().State != Online {
		return nil, ErrState
	}
	return m.rcon.ListPlayers(ctx)
}

// Stop runs the stop flow and returns when the server is off.
func (m *Monitor) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.state == Off || m.state == Stopping {
		m.mu.Unlock()
		return ErrState
	}
	m.state = Stopping
	m.mu.Unlock()
	return m.stop(ctx)
}

// stop asks Minecraft to stop through RCON, waits up to StopTimeout for the
// service to end and then stops it with SSH. The state must be Stopping.
func (m *Monitor) stop(ctx context.Context) error {
	// RCON errors are not fatal: the SSH stop below still saves the worlds.
	if err := m.rcon.Say(ctx, stopMessage); err != nil {
		slog.Warn("rcon say failed", "error", err)
	}
	if err := m.rcon.Stop(ctx); err != nil {
		slog.Warn("rcon stop failed", "error", err)
	}

	deadline := m.now().Add(m.opts.StopTimeout)
	for {
		st, err := m.ctl.Status(ctx)
		if err == nil && stopped(st.ActiveState) {
			m.finishStop()
			return nil
		}
		if err != nil {
			slog.Warn("service status failed during stop", "error", err)
		}
		if !m.now().Before(deadline) {
			break
		}
		if err := m.sleep(ctx, stopPoll); err != nil {
			m.set(Unresponsive)
			return err
		}
	}

	if err := m.ctl.Stop(ctx); err != nil {
		m.set(Unresponsive)
		return err
	}
	m.finishStop()
	return nil
}

func (m *Monitor) finishStop() {
	m.set(Off)
	m.notify(EventStopped)
}

// Tick pings the server once and updates the state.
func (m *Monitor) Tick(ctx context.Context) {
	m.mu.Lock()
	state := m.state
	m.mu.Unlock()
	// Off and Stopping change only through Start and the stop flow.
	if state == Off || state == Stopping {
		return
	}

	st, err := m.ping.Ping(ctx)
	if err != nil {
		m.pingFailed(ctx, state, err)
		return
	}
	m.pingOK(ctx, state, st)
}

func (m *Monitor) pingOK(ctx context.Context, state State, st minecraft.Status) {
	now := m.now()
	m.mu.Lock()
	if m.state != state { // a Stop ran while the ping was in flight
		m.mu.Unlock()
		return
	}
	m.players, m.max, m.version = st.OnlinePlayers, st.MaxPlayers, st.Version
	cameOnline := state != Online
	if cameOnline {
		m.state = Online
		if m.since.IsZero() {
			m.since = now
		}
		m.lastSeen = now
	}
	if st.OnlinePlayers > 0 {
		m.lastSeen = now
	}
	autostop := m.opts.EmptyTimeout > 0 && st.OnlinePlayers == 0 &&
		now.Sub(m.lastSeen) >= m.opts.EmptyTimeout
	if autostop {
		m.state = Stopping
	}
	m.mu.Unlock()

	if cameOnline {
		m.notify(EventOnline)
	}
	if autostop {
		m.notify(EventAutostop)
		if err := m.stop(ctx); err != nil {
			slog.Error("automatic stop failed", "error", err)
		}
	}
}

func (m *Monitor) pingFailed(ctx context.Context, state State, pingErr error) {
	switch state {
	case Starting:
		m.mu.Lock()
		timedOut := m.state == Starting && m.now().Sub(m.startedAt) >= m.opts.StartTimeout
		if timedOut {
			m.state = Unresponsive
		}
		m.mu.Unlock()
		if timedOut {
			slog.Warn("server did not answer within the start timeout", "error", pingErr)
			m.notify(EventStartTimeout)
		}

	case Online:
		// One SSH call tells a crash from a server that hangs.
		st, err := m.ctl.Status(ctx)
		m.mu.Lock()
		if m.state != Online {
			m.mu.Unlock()
			return
		}
		crashed := err == nil && stopped(st.ActiveState)
		if crashed {
			m.state, m.since = Off, time.Time{}
		} else {
			m.state = Unresponsive
		}
		m.mu.Unlock()

		if crashed {
			slog.Error("server crashed", "ping_error", pingErr, "service", st.ActiveState)
			m.notify(EventCrashed)
		} else {
			slog.Warn("server not responding", "ping_error", pingErr, "status_error", err)
			m.notify(EventUnresponsive)
			m.stopIfStillHanging(ctx)
		}
	}
}

// stopIfStillHanging pings again after hangGrace and runs the stop flow if
// the server still does not answer. A hung server only gets a clean stop
// this way; it is never restarted.
func (m *Monitor) stopIfStillHanging(ctx context.Context) {
	if err := m.sleep(ctx, hangGrace); err != nil {
		return
	}
	if _, err := m.ping.Ping(ctx); err == nil {
		return // the next tick brings it back online
	}
	m.mu.Lock()
	if m.state != Unresponsive { // a Stop ran during the grace period
		m.mu.Unlock()
		return
	}
	m.state = Stopping
	m.mu.Unlock()

	m.notify(EventHangStop)
	if err := m.stop(ctx); err != nil {
		slog.Error("stop of the unresponsive server failed", "error", err)
	}
}

func (m *Monitor) set(s State) {
	m.mu.Lock()
	m.state = s
	if s == Off {
		m.since = time.Time{}
	}
	m.mu.Unlock()
}

func (m *Monitor) notify(kind EventKind) {
	if m.opts.Notify == nil {
		return
	}
	m.mu.Lock()
	server := m.server
	m.mu.Unlock()
	m.opts.Notify(Event{Kind: kind, Server: server})
}

// stopped reports whether a systemd ActiveState means the service is not
// running: "inactive" after a clean stop, "failed" after a crash.
func stopped(activeState string) bool {
	return activeState == "inactive" || activeState == "failed"
}
