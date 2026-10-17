package discord

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/bwmarrin/discordgo"
)

// settings are the values chosen with /setup. They override the operator
// role and the log channel of config.toml. Role and channel IDs exist only
// in the guild they were chosen on, so on any other guild they are ignored.
type settings struct {
	GuildID        string `json:"guild_id"`
	OperatorRoleID string `json:"operator_role_id"`
	LogChannelID   string `json:"log_channel_id"`
}

// current returns the settings if they belong to the active guild, or
// empty ones: only administrators are operators and the log is off.
func (b *Bot) current() settings {
	active := b.activeGuild()
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	if b.settings.GuildID != "" && b.settings.GuildID != active {
		return settings{}
	}
	return b.settings
}

func (b *Bot) operatorRole() string { return b.current().OperatorRoleID }

func (b *Bot) logChannel() string { return b.current().LogChannelID }

// loadSettings reads the settings file, if /setup has written one.
func (b *Bot) loadSettings() error {
	data, err := os.ReadFile(b.opts.SettingsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read settings: %w", err)
	}
	var s settings
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("parse settings file %s: %w", b.opts.SettingsFile, err)
	}
	b.settingsMu.Lock()
	b.settings = s
	b.settingsMu.Unlock()
	return nil
}

// saveSettings writes the file atomically, so a crash never leaves it half
// written.
func saveSettings(path string, s settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// setup changes the operator role and the log channel. Only administrators
// may use it: an operator must not be able to choose who is an operator.
func (b *Bot) setup(i *discordgo.Interaction) {
	user := userOf(i)
	if i.Member == nil || i.Member.Permissions&discordgo.PermissionAdministrator == 0 {
		b.audit(user, "denied /setup", fmt.Sprintf("🚫 <@%s> tried to use /setup without permission.", user.ID))
		b.reply(i, "Only administrators can use /setup.", true)
		return
	}

	s := b.current()
	s.GuildID = b.activeGuild()
	changed := false
	for _, o := range i.ApplicationCommandData().Options {
		switch o.Name {
		case "role":
			s.OperatorRoleID, changed = o.RoleValue(nil, "").ID, true
		case "channel":
			s.LogChannelID, changed = o.ChannelValue(nil).ID, true
		}
	}

	if changed {
		if err := saveSettings(b.opts.SettingsFile, s); err != nil {
			slog.Error("save settings", "error", err)
			b.reply(i, msgFailed, true)
			return
		}
		b.settingsMu.Lock()
		b.settings = s
		b.settingsMu.Unlock()
		b.audit(user, "setup", fmt.Sprintf("⚙️ <@%s> changed the configuration: %s", user.ID, describe(s)))
	}

	text := "Current configuration: " + describe(s)
	if !changed {
		text += "\nTo change it, use the `role` and `channel` options."
	}
	if changed && s.LogChannelID != "" {
		// Check right away that the bot can write in the chosen channel.
		_, err := b.s.ChannelMessageSendComplex(s.LogChannelID, &discordgo.MessageSend{
			Content: "✅ From now on I will post the Minecraft server log here.", AllowedMentions: noMentions,
		})
		if err != nil {
			slog.Warn("write to the new log channel", "error", err)
			text += "\n⚠️ I cannot write in the log channel: check the bot's permissions in that channel."
		}
	}
	b.reply(i, text, true)
}

func describe(s settings) string {
	role, channel := "administrators only", "disabled"
	if s.OperatorRoleID != "" {
		role = "<@&" + s.OperatorRoleID + ">"
	}
	if s.LogChannelID != "" {
		channel = "<#" + s.LogChannelID + ">"
	}
	return fmt.Sprintf("operator role %s, log channel %s.", role, channel)
}

// welcome greets a guild the bot has just joined, in its system channel.
// GuildCreate also fires for every guild at each connection, so only a
// recent join counts.
func (b *Bot) welcome(g *discordgo.Guild) {
	if g.Unavailable || b.now().Sub(g.JoinedAt) > time.Minute {
		return
	}
	slog.Info("joined a guild", "guild_id", g.ID, "guild", g.Name)
	if g.SystemChannelID == "" {
		return
	}
	text := "👋 Hi! I control the Minecraft server.\n" +
		"An administrator can use `/setup` to choose the operator role and the log channel."
	switch {
	case g.ID == b.activeGuild():
	case b.opts.GuildID != "":
		text = "👋 Hi! My commands only work on the Discord server set as `guild_id` " +
			"in the configuration file, and this is not it."
		slog.Warn("joined a guild that is not the configured one", "guild_id", g.ID)
	default:
		text = "👋 Hi! I am already on another Discord server, and I work on one server at a time.\n" +
			"Remove me from there and my commands will move here; then an administrator can use `/setup`."
	}
	if _, err := b.s.ChannelMessageSendComplex(g.SystemChannelID, &discordgo.MessageSend{Content: text, AllowedMentions: noMentions}); err != nil {
		slog.Warn("send welcome message", "error", err)
	}
}
