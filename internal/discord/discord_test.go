package discord

import (
	"context"
	"path/filepath"
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
	sent      []string       // channel IDs of sent messages
	commands  map[string]int // guild ID -> number of registered commands
}

func (f *fakeSession) ApplicationCommandBulkOverwrite(_, guildID string, cmds []*discordgo.ApplicationCommand, _ ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	if f.commands == nil {
		f.commands = map[string]int{}
	}
	f.commands[guildID] = len(cmds)
	return cmds, nil
}

func (f *fakeSession) InteractionRespond(_ *discordgo.Interaction, r *discordgo.InteractionResponse, _ ...discordgo.RequestOption) error {
	f.responses = append(f.responses, r)
	return nil
}
func (f *fakeSession) InteractionResponseEdit(*discordgo.Interaction, *discordgo.WebhookEdit, ...discordgo.RequestOption) (*discordgo.Message, error) {
	return &discordgo.Message{}, nil
}
func (f *fakeSession) ChannelMessageSendComplex(channelID string, _ *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.sent = append(f.sent, channelID)
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
		GuildID:        "guild",
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
		Type:    discordgo.InteractionApplicationCommand,
		Data:    discordgo.ApplicationCommandInteractionData{Name: name},
		Member:  member,
		GuildID: "guild",
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
			Type:    discordgo.InteractionMessageComponent,
			Data:    discordgo.MessageComponentInteractionData{CustomID: id},
			Member:  &discordgo.Member{User: &discordgo.User{ID: user}, Permissions: discordgo.PermissionAdministrator},
			GuildID: "guild",
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

func setupCommand(member *discordgo.Member) *discordgo.Interaction {
	i := command("setup", member)
	i.Data = discordgo.ApplicationCommandInteractionData{Name: "setup", Options: []*discordgo.ApplicationCommandInteractionDataOption{
		{Name: "ruolo", Type: discordgo.ApplicationCommandOptionRole, Value: "newrole"},
		{Name: "canale", Type: discordgo.ApplicationCommandOptionChannel, Value: "newchannel"},
	}}
	return i
}

func TestSetupOnlyForAdministrators(t *testing.T) {
	b, _, s := newTestBot(monitor.Snapshot{})
	b.opts.SettingsFile = filepath.Join(t.TempDir(), "settings.json")
	b.handle(setupCommand(member(0, "op"))) // an operator, not an administrator
	if b.operatorRole() != "op" || b.logChannel() != "logs" {
		t.Fatalf("settings changed by an operator: %+v", b.settings)
	}
	if len(s.responses) != 1 || s.responses[0].Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Fatalf("want one ephemeral refusal, got %+v", s.responses)
	}
}

func TestSetupSavesAndReloads(t *testing.T) {
	b, _, s := newTestBot(monitor.Snapshot{})
	b.opts.SettingsFile = filepath.Join(t.TempDir(), "data", "settings.json")
	b.handle(setupCommand(member(discordgo.PermissionAdministrator)))
	if b.operatorRole() != "newrole" || b.logChannel() != "newchannel" {
		t.Fatalf("settings = %+v", b.settings)
	}
	if len(s.sent) != 1 || s.sent[0] != "newchannel" {
		t.Fatalf("messages sent to %v, want a test message in the new channel", s.sent)
	}
	if !isOperator(member(0, "newrole"), b.operatorRole()) {
		t.Fatal("the new role is not an operator")
	}

	// A restart reads the file instead of config.toml.
	b2, _, _ := newTestBot(monitor.Snapshot{})
	b2.opts.SettingsFile = b.opts.SettingsFile
	if err := b2.loadSettings(); err != nil {
		t.Fatal(err)
	}
	if b2.operatorRole() != "newrole" || b2.logChannel() != "newchannel" {
		t.Fatalf("reloaded settings = %+v", b2.settings)
	}
}

func TestLoadSettingsWithoutFile(t *testing.T) {
	b, _, _ := newTestBot(monitor.Snapshot{})
	b.opts.SettingsFile = filepath.Join(t.TempDir(), "missing.json")
	if err := b.loadSettings(); err != nil {
		t.Fatal(err)
	}
	if b.operatorRole() != "op" || b.logChannel() != "logs" {
		t.Fatalf("settings = %+v, want the config.toml values", b.settings)
	}
}

func TestWelcomeOnlyOnNewJoin(t *testing.T) {
	b, _, s := newTestBot(monitor.Snapshot{})
	b.welcome(&discordgo.Guild{ID: "guild", SystemChannelID: "general", JoinedAt: now.Add(-24 * time.Hour)})
	if len(s.sent) != 0 {
		t.Fatalf("welcomed a guild joined yesterday: %v", s.sent)
	}
	b.welcome(&discordgo.Guild{ID: "guild", SystemChannelID: "general", JoinedAt: now.Add(-5 * time.Second)})
	if len(s.sent) != 1 || s.sent[0] != "general" {
		t.Fatalf("messages sent to %v, want a welcome in general", s.sent)
	}
}

func guilds(ids ...string) []*discordgo.Guild {
	var gs []*discordgo.Guild
	for _, id := range ids {
		gs = append(gs, &discordgo.Guild{ID: id})
	}
	return gs
}

func TestAutoGuildFollowsTheOnlyGuild(t *testing.T) {
	b, _, s := newTestBot(monitor.Snapshot{})
	b.opts.GuildID, b.active = "", ""

	b.ready("app", guilds("old"))
	if b.activeGuild() != "old" || s.commands["old"] != len(commands) {
		t.Fatalf("one guild: active = %q, commands = %v", b.activeGuild(), s.commands)
	}

	b.joined("new")
	if b.activeGuild() != "" || s.commands["old"] != 0 || s.commands["new"] != 0 {
		t.Fatalf("two guilds: active = %q, commands = %v, want none anywhere", b.activeGuild(), s.commands)
	}

	b.left("old")
	if b.activeGuild() != "new" || s.commands["new"] != len(commands) {
		t.Fatalf("old guild left: active = %q, commands = %v", b.activeGuild(), s.commands)
	}
}

func TestConfiguredGuildIgnoresOthers(t *testing.T) {
	b, _, s := newTestBot(monitor.Snapshot{})
	b.ready("app", guilds("guild", "other"))
	b.joined("third")
	b.left("guild")
	if b.activeGuild() != "guild" || s.commands["guild"] != len(commands) || len(s.commands) != 1 {
		t.Fatalf("active = %q, commands = %v, want only the configured guild", b.activeGuild(), s.commands)
	}
}

func TestInteractionFromInactiveGuildIsRefused(t *testing.T) {
	b, srv, s := newTestBot(monitor.Snapshot{State: monitor.Off, Server: "survival"})
	i := command("startserver", member(discordgo.PermissionAdministrator))
	i.GuildID = "other"
	b.handle(i)
	if len(srv.calls) != 0 {
		t.Fatalf("server calls = %v, want none", srv.calls)
	}
	if len(s.responses) != 1 || s.responses[0].Data.Content != msgWrongGuild {
		t.Fatalf("responses = %+v, want the wrong guild refusal", s.responses)
	}
}

func TestSettingsOfAnotherGuildAreIgnored(t *testing.T) {
	b, _, _ := newTestBot(monitor.Snapshot{})
	b.opts.SettingsFile = filepath.Join(t.TempDir(), "settings.json")
	b.handle(setupCommand(member(discordgo.PermissionAdministrator)))

	b2, _, _ := newTestBot(monitor.Snapshot{})
	b2.opts.SettingsFile, b2.opts.GuildID, b2.active = b.opts.SettingsFile, "", ""
	if err := b2.loadSettings(); err != nil {
		t.Fatal(err)
	}
	b2.ready("app", guilds("elsewhere"))
	if b2.operatorRole() != "" || b2.logChannel() != "" {
		t.Fatalf("settings of another guild used: role %q, channel %q", b2.operatorRole(), b2.logChannel())
	}
}
