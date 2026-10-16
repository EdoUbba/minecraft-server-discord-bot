package discord

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/EdoUbba/minecraft-server-discord-bot/internal/monitor"
)

// fakeServer records every call that would reach the Minecraft LXC.
type fakeServer struct {
	snap  monitor.Snapshot
	calls []string
}

func (f *fakeServer) Snapshot() monitor.Snapshot { return f.snap }
func (f *fakeServer) Start(context.Context) error {
	f.calls = append(f.calls, "start")
	return nil
}
func (f *fakeServer) Stop(context.Context) error {
	f.calls = append(f.calls, "stop")
	return nil
}
func (f *fakeServer) List(context.Context) ([]string, error) {
	f.calls = append(f.calls, "list")
	return []string{"survival"}, nil
}
func (f *fakeServer) Select(context.Context, string) error {
	f.calls = append(f.calls, "select")
	return nil
}
func (f *fakeServer) Players(context.Context) ([]string, error) {
	f.calls = append(f.calls, "players")
	return nil, nil
}

type fakeSession struct {
	responses []*discordgo.InteractionResponse
	presences []string
}

func (f *fakeSession) InteractionRespond(_ *discordgo.Interaction, r *discordgo.InteractionResponse, _ ...discordgo.RequestOption) error {
	f.responses = append(f.responses, r)
	return nil
}
func (f *fakeSession) InteractionResponseEdit(*discordgo.Interaction, *discordgo.WebhookEdit, ...discordgo.RequestOption) (*discordgo.Message, error) {
	return &discordgo.Message{}, nil
}
func (f *fakeSession) ChannelMessageSendComplex(string, *discordgo.MessageSend, ...discordgo.RequestOption) (*discordgo.Message, error) {
	return &discordgo.Message{}, nil
}
func (f *fakeSession) UpdateStatusComplex(d discordgo.UpdateStatusData) error {
	f.presences = append(f.presences, d.Activities[0].Name)
	return nil
}

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newTestBot(snap monitor.Snapshot) (*Bot, *fakeServer, *fakeSession) {
	srv, s := &fakeServer{snap: snap}, &fakeSession{}
	b := New(srv, Options{
		OperatorRoleID: "op",
		LogChannelID:   "logs",
		Interval:       time.Minute,
		DisplayName:    func(string) string { return "Survival Friends" },
	})
	b.s, b.now = s, func() time.Time { return now }
	return b, srv, s
}

func command(name string, member *discordgo.Member) *discordgo.Interaction {
	return &discordgo.Interaction{
		Type:   discordgo.InteractionApplicationCommand,
		Data:   discordgo.ApplicationCommandInteractionData{Name: name},
		Member: member,
	}
}

func member(perms int64, roles ...string) *discordgo.Member {
	return &discordgo.Member{User: &discordgo.User{ID: "42", Username: "steve"}, Permissions: perms, Roles: roles}
}

func TestIsOperator(t *testing.T) {
	tests := []struct {
		name   string
		member *discordgo.Member
		roleID string
		want   bool
	}{
		{"administrator", member(discordgo.PermissionAdministrator), "op", true},
		{"operator role", member(0, "other", "op"), "op", true},
		{"no role", member(discordgo.PermissionManageMessages, "other"), "op", false},
		{"role not configured", member(0, ""), "", false},
		{"not in a guild", nil, "op", false},
	}
	for _, tt := range tests {
		if got := isOperator(tt.member, tt.roleID); got != tt.want {
			t.Errorf("%s: isOperator = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestNonOperatorIsRefusedWithoutSSH(t *testing.T) {
	for _, name := range []string{"startserver", "stopserver", "config"} {
		b, srv, s := newTestBot(monitor.Snapshot{State: monitor.Off, Server: "survival"})
		if name == "stopserver" {
			srv.snap.State = monitor.Online
		}
		b.handle(command(name, member(0, "other")))

		if len(srv.calls) != 0 {
			t.Errorf("/%s: server calls = %v, want none", name, srv.calls)
		}
		if len(s.responses) != 1 || s.responses[0].Data.Flags&discordgo.MessageFlagsEphemeral == 0 ||
			s.responses[0].Data.Content != msgDenied {
			t.Errorf("/%s: want one ephemeral refusal, got %+v", name, s.responses)
		}
		if len(b.logs) != 1 {
			t.Errorf("/%s: the attempt was not sent to the log channel", name)
		}
	}
}

func TestOperatorCanStart(t *testing.T) {
	b, srv, _ := newTestBot(monitor.Snapshot{State: monitor.Off})
	b.handle(command("startserver", member(0, "op")))
	if len(srv.calls) != 1 || srv.calls[0] != "start" {
		t.Fatalf("server calls = %v, want start", srv.calls)
	}
}

func TestConfigRefusedWhenServerIsOn(t *testing.T) {
	for _, state := range []monitor.State{monitor.Starting, monitor.Online, monitor.Stopping, monitor.Unresponsive} {
		b, srv, s := newTestBot(monitor.Snapshot{State: state})
		b.handle(command("config", member(discordgo.PermissionAdministrator)))
		if len(srv.calls) != 0 {
			t.Errorf("%v: server calls = %v, want none", state, srv.calls)
		}
		if len(s.responses) != 1 || s.responses[0].Data.Content != msgStopFirst {
			t.Errorf("%v: want the stop-first refusal, got %+v", state, s.responses)
		}
	}
}

func TestStopAsksConfirmationWithPlayers(t *testing.T) {
	b, srv, s := newTestBot(monitor.Snapshot{State: monitor.Online, Players: 2})
	b.handle(command("stopserver", member(discordgo.PermissionAdministrator)))
	if len(srv.calls) != 0 {
		t.Fatalf("server calls = %v, want none before the confirmation", srv.calls)
	}
	if len(s.responses) != 1 || len(s.responses[0].Data.Components) != 1 {
		t.Fatalf("want a confirmation button, got %+v", s.responses)
	}
	id := s.responses[0].Data.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.Button).CustomID

	press := func(user string, at time.Time) {
		b.now = func() time.Time { return at }
		b.handle(&discordgo.Interaction{
			Type:   discordgo.InteractionMessageComponent,
			Data:   discordgo.MessageComponentInteractionData{CustomID: id},
			Member: &discordgo.Member{User: &discordgo.User{ID: user}, Permissions: discordgo.PermissionAdministrator},
		})
	}
	press("7", now) // someone else
	press("42", now.Add(stopConfirmTTL+time.Second))
	if len(srv.calls) != 0 {
		t.Fatalf("server calls = %v, want none for another user or after expiry", srv.calls)
	}
	press("42", now.Add(10*time.Second))
	if len(srv.calls) != 1 || srv.calls[0] != "stop" {
		t.Fatalf("server calls = %v, want stop", srv.calls)
	}
}

func TestPresenceOnlyWhenTextChanges(t *testing.T) {
	b, srv, s := newTestBot(monitor.Snapshot{State: monitor.Online, Since: now.Add(-135 * time.Minute), Players: 3, MaxPlayers: 20})
	b.refreshPresence()
	b.refreshPresence()
	srv.snap.Players = 4
	b.refreshPresence()
	srv.snap = monitor.Snapshot{State: monitor.Off}
	b.refreshPresence()
	b.refreshPresence()

	want := []string{"🟢 Survival Friends · da 2h 15m · 3/20", "🟢 Survival Friends · da 2h 15m · 4/20", "🔴 Survival Friends è spento"}
	if len(s.presences) != len(want) {
		t.Fatalf("presences = %q, want %q", s.presences, want)
	}
	for n := range want {
		if s.presences[n] != want[n] {
			t.Fatalf("presences = %q, want %q", s.presences, want)
		}
	}
}
