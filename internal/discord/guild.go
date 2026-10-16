package discord

import (
	"log/slog"
	"maps"
	"slices"

	"github.com/bwmarrin/discordgo"
)

// activeGuild is the guild where the commands work: the configured one, or
// the only guild the bot is in. Empty means none.
func (b *Bot) activeGuild() string {
	b.guildMu.Lock()
	defer b.guildMu.Unlock()
	return b.active
}

// ready runs at every gateway connection: it learns the guilds the bot is in
// and registers the commands again.
func (b *Bot) ready(appID string, guilds []*discordgo.Guild) {
	b.guildMu.Lock()
	defer b.guildMu.Unlock()
	b.appID = appID
	clear(b.guilds)
	for _, g := range guilds {
		b.guilds[g.ID] = true
	}
	b.sync(true)
}

func (b *Bot) joined(id string) {
	b.guildMu.Lock()
	defer b.guildMu.Unlock()
	if !b.guilds[id] {
		b.guilds[id] = true
		b.sync(false)
	}
}

func (b *Bot) left(id string) {
	b.guildMu.Lock()
	defer b.guildMu.Unlock()
	if b.guilds[id] {
		delete(b.guilds, id)
		b.sync(false)
	}
}

// sync picks the active guild and moves the commands there. With no
// guild_id the bot never chooses among several guilds: it waits until it is
// in only one. The caller holds guildMu.
func (b *Bot) sync(force bool) {
	if b.appID == "" {
		return // not connected yet; ready will sync
	}
	active := b.opts.GuildID
	if active == "" && len(b.guilds) == 1 {
		active = slices.Collect(maps.Keys(b.guilds))[0]
	}
	if active == "" && len(b.guilds) > 1 {
		slog.Warn("the bot is in more than one guild and guild_id is empty: commands disabled until it is removed from all but one",
			"guilds", len(b.guilds))
	}
	old := b.active
	if active == old && !force {
		return
	}
	b.active = active

	if old != "" && old != active && b.guilds[old] {
		if _, err := b.s.ApplicationCommandBulkOverwrite(b.appID, old, nil); err != nil {
			slog.Error("remove slash commands", "guild_id", old, "error", err)
		}
	}
	if active == "" {
		return
	}
	if _, err := b.s.ApplicationCommandBulkOverwrite(b.appID, active, commands); err != nil {
		slog.Error("register slash commands", "guild_id", active, "error", err)
	}
	if active != old {
		slog.Info("commands active on guild", "guild_id", active)
	}

	// Settings saved before they carried a guild belong to the first active one.
	b.settingsMu.Lock()
	if b.settings.GuildID == "" {
		b.settings.GuildID = active
	}
	b.settingsMu.Unlock()
}
