// Package discord serves the slash commands, keeps the bot presence up to
// date and writes to the log channel. Every server action goes through the
// monitor.
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/EdoUbba/minecraft-server-discord-bot/internal/monitor"
)

// Server is the part of the monitor the commands use.
type Server interface {
	Snapshot() monitor.Snapshot
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	List(ctx context.Context) ([]string, error)
	Select(ctx context.Context, name string) error
	Players(ctx context.Context) ([]string, error)
}

// session is the part of *discordgo.Session the bot uses.
type session interface {
	InteractionRespond(i *discordgo.Interaction, r *discordgo.InteractionResponse, options ...discordgo.RequestOption) error
	InteractionResponseEdit(i *discordgo.Interaction, e *discordgo.WebhookEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageSendComplex(channelID string, m *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	UpdateStatusComplex(data discordgo.UpdateStatusData) error
}

type Options struct {
	GuildID        string
	OperatorRoleID string // empty = only administrators are operators
	LogChannelID   string // empty = log channel disabled
	SettingsFile   string // written by /setup; overrides the two IDs above
	Interval       time.Duration
	EmptyTimeout   time.Duration // shown in the automatic stop message
	DisplayName    func(folder string) string
}

type Bot struct {
	srv  Server
	opts Options
	s    session
	ctx  context.Context // ends at shutdown; long operations run on it
	now  func() time.Time

	ops  sync.Mutex    // one start, stop or server change at a time
	logs chan string   // messages for the log channel
	kick chan struct{} // asks for a presence refresh

	presenceMu sync.Mutex
	presence   string // last text sent to Discord

	settingsMu sync.Mutex
	settings   settings
}

func New(srv Server, opts Options) *Bot {
	return &Bot{
		srv:  srv,
		opts: opts,
		ctx:  context.Background(),
		now:  time.Now,
		logs: make(chan string, 32),
		kick: make(chan struct{}, 1),
		settings: settings{
			OperatorRoleID: opts.OperatorRoleID,
			LogChannelID:   opts.LogChannelID,
		},
	}
}

// Run connects to Discord, registers the commands on the guild and serves
// until ctx ends.
func (b *Bot) Run(ctx context.Context, token string) error {
	if err := b.loadSettings(); err != nil {
		return err
	}
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return fmt.Errorf("discord session: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds
	s.StateEnabled = false
	b.s, b.ctx = s, ctx

	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		appID := r.User.ID
		if r.Application != nil {
			appID = r.Application.ID
		}
		if _, err := s.ApplicationCommandBulkOverwrite(appID, b.opts.GuildID, commands); err != nil {
			slog.Error("register slash commands", "error", err)
		}
		// A new gateway session starts with no presence.
		b.presenceMu.Lock()
		b.presence = ""
		b.presenceMu.Unlock()
		b.kickPresence()
		slog.Info("connected to discord", "user", r.User.Username)
	})
	s.AddHandler(func(_ *discordgo.Session, g *discordgo.GuildCreate) {
		b.welcome(g.Guild)
	})
	s.AddHandler(func(_ *discordgo.Session, i *discordgo.InteractionCreate) {
		b.handle(i.Interaction)
	})

	if err := s.Open(); err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}
	defer s.Close()

	t := time.NewTicker(b.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			b.refreshPresence()
		case <-b.kick:
			b.refreshPresence()
		case text := <-b.logs:
			b.sendLog(text)
		}
	}
}

// Notify receives the monitor events. It never blocks.
func (b *Bot) Notify(e monitor.Event) {
	b.queueLog(b.eventText(e))
	b.kickPresence()
}

func (b *Bot) eventText(e monitor.Event) string {
	name := b.opts.DisplayName(e.Server)
	switch e.Kind {
	case monitor.EventOnline:
		return fmt.Sprintf("🟢 **%s** è online.", name)
	case monitor.EventStartTimeout:
		return fmt.Sprintf("⚠️ **%s** non risponde dopo l'avvio.", name)
	case monitor.EventStopped:
		return fmt.Sprintf("🔴 **%s** è spento.", name)
	case monitor.EventCrashed:
		return fmt.Sprintf("💥 **%s** si è fermato da solo (crash). Non verrà riavviato in automatico.", name)
	case monitor.EventUnresponsive:
		return fmt.Sprintf("⚠️ **%s** non risponde.", name)
	case monitor.EventAutostop:
		return fmt.Sprintf("⏹️ **%s** è vuoto da %s: spegnimento automatico.", name, formatDuration(b.opts.EmptyTimeout))
	case monitor.EventHangStop:
		return fmt.Sprintf("⏹️ **%s** continua a non rispondere: spegnimento automatico.", name)
	}
	return fmt.Sprintf("Evento %d su **%s**.", e.Kind, name)
}

// audit records a user action in the logs and in the log channel.
func (b *Bot) audit(user *discordgo.User, action, text string) {
	slog.Info("user action", "action", action, "user_id", user.ID, "user", user.Username)
	b.queueLog(text)
}

func (b *Bot) queueLog(text string) {
	if b.logChannel() == "" {
		return
	}
	select {
	case b.logs <- text:
	default:
		slog.Warn("log channel queue full, message dropped")
	}
}

func (b *Bot) sendLog(text string) {
	_, err := b.s.ChannelMessageSendComplex(b.logChannel(), &discordgo.MessageSend{
		Content:         text,
		AllowedMentions: noMentions,
	})
	if err != nil {
		slog.Warn("send to log channel", "error", err)
	}
}

func (b *Bot) kickPresence() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// refreshPresence sends the presence, shown as "Sta giocando a <text>",
// only when the text changed.
func (b *Bot) refreshPresence() {
	text := presenceText(b.srv.Snapshot(), b.opts.DisplayName, b.now())
	b.presenceMu.Lock()
	defer b.presenceMu.Unlock()
	if text == b.presence {
		return
	}
	err := b.s.UpdateStatusComplex(discordgo.UpdateStatusData{
		Status:     string(discordgo.StatusOnline),
		Activities: []*discordgo.Activity{{Name: text, Type: discordgo.ActivityTypeGame}},
	})
	if err != nil {
		slog.Warn("update presence", "error", err)
		return
	}
	b.presence = text
}

func presenceText(s monitor.Snapshot, displayName func(string) string, now time.Time) string {
	name := displayName(s.Server)
	switch s.State {
	case monitor.Starting:
		return fmt.Sprintf("🟡 %s in avvio", name)
	case monitor.Online:
		return fmt.Sprintf("🟢 %s · da %s · %d/%d", name, formatDuration(now.Sub(s.Since)), s.Players, s.MaxPlayers)
	case monitor.Stopping:
		return fmt.Sprintf("🟠 %s in spegnimento", name)
	case monitor.Unresponsive:
		return fmt.Sprintf("⚠️ %s non risponde", name)
	}
	return fmt.Sprintf("🔴 %s è spento", name)
}

// formatDuration writes d as "2h 15m" or "15m".
func formatDuration(d time.Duration) string {
	d = max(d, 0)
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// isOperator reports whether a member may start, stop or change the server:
// administrators through any role, or members with the operator role.
func isOperator(m *discordgo.Member, operatorRoleID string) bool {
	if m == nil {
		return false
	}
	if m.Permissions&discordgo.PermissionAdministrator != 0 {
		return true
	}
	return operatorRoleID != "" && slices.Contains(m.Roles, operatorRoleID)
}
