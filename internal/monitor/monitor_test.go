package monitor

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/EdoUbba/minecraft-server-discord-bot/internal/control"
	"github.com/EdoUbba/minecraft-server-discord-bot/internal/minecraft"
)

var errNoAnswer = errors.New("no answer")

type fakePinger struct {
	players int
	err     error
}

func (p *fakePinger) Ping(context.Context) (minecraft.Status, error) {
	return minecraft.Status{OnlinePlayers: p.players, MaxPlayers: 20}, p.err
}

type fakeRCON struct{ calls []string }

func (r *fakeRCON) Say(context.Context, string) error { r.calls = append(r.calls, "say"); return nil }
func (r *fakeRCON) Stop(context.Context) error        { r.calls = append(r.calls, "stop"); return nil }

type fakeControl struct {
	calls []string
	// active is the ActiveState reported by Status; Stop makes it inactive.
	active string
	since  time.Time
	// stopAfter makes the service go inactive after this many Status calls
	// during a stop; -1 means never on its own.
	stopAfter int
}

func (c *fakeControl) Start(context.Context) error {
	c.calls = append(c.calls, "start")
	c.active = "active"
	return nil
}

func (c *fakeControl) Stop(context.Context) error {
	c.calls = append(c.calls, "stop")
	c.active = "inactive"
	return nil
}

func (c *fakeControl) Status(context.Context) (control.Status, error) {
	c.calls = append(c.calls, "status")
	if c.stopAfter == 0 {
		c.active = "inactive"
	}
	c.stopAfter--
	return control.Status{ActiveState: c.active, Since: c.since, Server: "survival"}, nil
}

type harness struct {
	t      *testing.T
	m      *Monitor
	ping   *fakePinger
	rcon   *fakeRCON
	ctl    *fakeControl
	clock  time.Time
	events []EventKind
}

// newHarness starts with the service stopped.
func newHarness(t *testing.T) *harness {
	return newHarnessWith(t, &fakeControl{active: "inactive", stopAfter: -1})
}

func newHarnessWith(t *testing.T, ctl *fakeControl) *harness {
	h := &harness{
		t:     t,
		ping:  &fakePinger{},
		rcon:  &fakeRCON{},
		ctl:   ctl,
		clock: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	h.m = New(h.ping, h.rcon, h.ctl, Options{
		Interval:     time.Minute,
		StartTimeout: 5 * time.Minute,
		StopTimeout:  90 * time.Second,
		EmptyTimeout: 10 * time.Minute,
		Notify:       func(e Event) { h.events = append(h.events, e.Kind) },
	})
	h.m.now = func() time.Time { return h.clock }
	h.m.sleep = func(_ context.Context, d time.Duration) error { h.clock = h.clock.Add(d); return nil }
	if err := h.m.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

// tick advances the clock by one interval and pings.
func (h *harness) tick() {
	h.clock = h.clock.Add(time.Minute)
	h.m.Tick(context.Background())
}

func (h *harness) wantState(want State) {
	h.t.Helper()
	if got := h.m.Snapshot().State; got != want {
		h.t.Fatalf("state = %v, want %v", got, want)
	}
}

func (h *harness) wantEvents(want ...EventKind) {
	h.t.Helper()
	if !slices.Equal(h.events, want) {
		h.t.Fatalf("events = %v, want %v", h.events, want)
	}
}

// online starts the server and brings it online with one player.
func (h *harness) online() {
	h.t.Helper()
	if err := h.m.Start(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.ping.players = 1
	h.tick()
	h.wantState(Online)
	h.events, h.ctl.calls = nil, nil
}

func TestStartGoesOnlineAtFirstPing(t *testing.T) {
	h := newHarness(t)
	h.wantState(Off)
	if err := h.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wantState(Starting)
	if !slices.Equal(h.ctl.calls, []string{"status", "start"}) {
		t.Fatalf("ssh calls = %v", h.ctl.calls)
	}

	h.ping.err = errNoAnswer
	h.tick()
	h.wantState(Starting)

	h.ping.err, h.ping.players = nil, 2
	h.tick()
	h.wantState(Online)
	h.wantEvents(EventOnline)
	s := h.m.Snapshot()
	if !s.Since.Equal(h.clock) || s.Players != 2 || s.MaxPlayers != 20 || s.Server != "survival" {
		t.Fatalf("snapshot = %+v", s)
	}
	if err := h.m.Start(context.Background()); !errors.Is(err, ErrState) {
		t.Fatalf("second start: error = %v, want ErrState", err)
	}
}

func TestStartTimeout(t *testing.T) {
	h := newHarness(t)
	h.m.Start(context.Background())
	h.ping.err = errNoAnswer
	for range 4 {
		h.tick()
	}
	h.wantState(Starting)
	h.tick() // 5 minutes
	h.wantState(Unresponsive)
	h.wantEvents(EventStartTimeout)

	// A late answer still brings it online.
	h.ping.err = nil
	h.tick()
	h.wantState(Online)
}

func TestStopWaitsForCleanStop(t *testing.T) {
	h := newHarness(t)
	h.online()
	h.ctl.stopAfter = 2 // inactive at the third status check
	if err := h.m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wantState(Off)
	h.wantEvents(EventStopped)
	if !slices.Equal(h.rcon.calls, []string{"say", "stop"}) {
		t.Fatalf("rcon calls = %v", h.rcon.calls)
	}
	if !slices.Equal(h.ctl.calls, []string{"status", "status", "status"}) {
		t.Fatalf("ssh calls = %v, want no ssh stop", h.ctl.calls)
	}
	if err := h.m.Stop(context.Background()); !errors.Is(err, ErrState) {
		t.Fatalf("stop when off: error = %v, want ErrState", err)
	}
}

func TestStopFallsBackToSSH(t *testing.T) {
	h := newHarness(t)
	h.online()
	start := h.clock
	if err := h.m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wantState(Off)
	if last := h.ctl.calls[len(h.ctl.calls)-1]; last != "stop" {
		t.Fatalf("ssh calls = %v, want a final stop", h.ctl.calls)
	}
	if waited := h.clock.Sub(start); waited != 90*time.Second {
		t.Fatalf("waited %v before the ssh stop, want 90s", waited)
	}
}

func TestAutostopAfterTenEmptyMinutes(t *testing.T) {
	h := newHarness(t)
	h.online()
	h.ctl.stopAfter = 0
	h.ping.players = 0
	for range 9 {
		h.tick()
	}
	h.wantState(Online)
	h.tick() // 10 minutes empty
	h.wantState(Off)
	h.wantEvents(EventAutostop, EventStopped)
}

func TestAutostopResetsWhenPlayerJoins(t *testing.T) {
	h := newHarness(t)
	h.online()
	h.ctl.stopAfter = 0
	h.ping.players = 0
	for range 9 {
		h.tick()
	}
	h.ping.players = 1
	h.tick()
	h.ping.players = 0
	for range 9 {
		h.tick()
	}
	h.wantState(Online)
	h.tick()
	h.wantState(Off)
}

func TestAutostopDisabled(t *testing.T) {
	h := newHarness(t)
	h.m.opts.EmptyTimeout = 0
	h.online()
	h.ping.players = 0
	for range 60 {
		h.tick()
	}
	h.wantState(Online)
}

func TestCrash(t *testing.T) {
	h := newHarness(t)
	h.online()
	h.ctl.active = "failed"
	h.ping.err = errNoAnswer
	h.tick()
	h.wantState(Off)
	h.wantEvents(EventCrashed)
	if !slices.Equal(h.ctl.calls, []string{"status"}) {
		t.Fatalf("ssh calls = %v, want only status (no restart)", h.ctl.calls)
	}
}

func TestHangingServerIsUnresponsive(t *testing.T) {
	h := newHarness(t)
	h.online()
	h.ping.err = errNoAnswer
	h.tick()
	h.wantState(Unresponsive)
	h.wantEvents(EventUnresponsive)

	h.ping.err = nil
	h.tick()
	h.wantState(Online)
}

func TestBotRestartKeepsUptime(t *testing.T) {
	h := newHarnessWith(t, &fakeControl{active: "active", since: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC), stopAfter: -1})
	h.wantState(Starting)
	h.ping.players = 1
	h.tick()
	h.wantState(Online)
	if got := h.m.Snapshot().Since; !got.Equal(h.ctl.since) {
		t.Fatalf("since = %v, want %v", got, h.ctl.since)
	}
}
