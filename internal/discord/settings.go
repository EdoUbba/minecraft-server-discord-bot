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
// role and the log channel of config.toml.
type settings struct {
	OperatorRoleID string `json:"operator_role_id"`
	LogChannelID   string `json:"log_channel_id"`
}

func (b *Bot) operatorRole() string {
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	return b.settings.OperatorRoleID
}

func (b *Bot) logChannel() string {
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	return b.settings.LogChannelID
}

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
		b.audit(user, "denied /setup", fmt.Sprintf("🚫 <@%s> ha provato a usare /setup senza permessi.", user.ID))
		b.reply(i, "Solo gli amministratori possono usare /setup.", true)
		return
	}

	b.settingsMu.Lock()
	s := b.settings
	b.settingsMu.Unlock()
	changed := false
	for _, o := range i.ApplicationCommandData().Options {
		switch o.Name {
		case "ruolo":
			s.OperatorRoleID, changed = o.RoleValue(nil, "").ID, true
		case "canale":
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
		b.audit(user, "setup", fmt.Sprintf("⚙️ <@%s> ha cambiato la configurazione: %s", user.ID, describe(s)))
	}

	text := "Configurazione attuale: " + describe(s)
	if !changed {
		text += "\nPer cambiarla usa le opzioni `ruolo` e `canale`."
	}
	if changed && s.LogChannelID != "" {
		// Check right away that the bot can write in the chosen channel.
		_, err := b.s.ChannelMessageSendComplex(s.LogChannelID, &discordgo.MessageSend{
			Content: "✅ Da ora scrivo qui il log del server Minecraft.", AllowedMentions: noMentions,
		})
		if err != nil {
			slog.Warn("write to the new log channel", "error", err)
			text += "\n⚠️ Non riesco a scrivere nel canale di log: controlla i permessi del bot in quel canale."
		}
	}
	b.reply(i, text, true)
}

func describe(s settings) string {
	role, channel := "solo amministratori", "disattivato"
	if s.OperatorRoleID != "" {
		role = "<@&" + s.OperatorRoleID + ">"
	}
	if s.LogChannelID != "" {
		channel = "<#" + s.LogChannelID + ">"
	}
	return fmt.Sprintf("ruolo operatore %s, canale di log %s.", role, channel)
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
	text := "👋 Ciao! Controllo il server Minecraft.\n" +
		"Un amministratore può usare `/setup` per scegliere il ruolo operatore e il canale di log."
	if g.ID != b.opts.GuildID {
		text = "👋 Ciao! I miei comandi sono attivi solo sul server Discord indicato in `guild_id` " +
			"nel file di configurazione, e non è questo."
		slog.Warn("joined a guild that is not the configured one", "guild_id", g.ID)
	}
	if _, err := b.s.ChannelMessageSendComplex(g.SystemChannelID, &discordgo.MessageSend{Content: text, AllowedMentions: noMentions}); err != nil {
		slog.Warn("send welcome message", "error", err)
	}
}
