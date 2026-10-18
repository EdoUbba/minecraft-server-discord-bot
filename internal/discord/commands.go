package discord

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/EdoUbba/minecraft-server-discord-bot/internal/monitor"
)

// The confirmation button of /stopserver and the menu of /config stop
// working after these times.
const (
	stopConfirmTTL = 30 * time.Second
	configMenuTTL  = 60 * time.Second
)

// Discord limits.
const (
	maxSelectOptions = 25
	maxCustomID      = 100
	maxOptionText    = 100
)

// commands are registered on the guild at every connection. All of them are
// visible to everyone: Discord can hide a command only by permission, not by
// role, which would hide it from operators too. The handlers check the role.
var commands = []*discordgo.ApplicationCommand{
	{Name: "startserver", Description: "Start the Minecraft server (operators only)"},
	{Name: "stopserver", Description: "Stop the Minecraft server (operators only)"},
	{Name: "config", Description: "Choose which Minecraft server is active (operators only)"},
	{Name: "status", Description: "Show the Minecraft server status"},
	{Name: "onlineplayer", Description: "Show the online players"},
	{
		Name:                     "command",
		Description:              "Run a Minecraft console command (administrators only)",
		DefaultMemberPermissions: &adminOnly,
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionString, Name: "command", Description: "For example: op Steve",
				Required: true, MaxLength: maxInput,
			},
		},
	},
	{
		Name:                     "chat",
		Description:              "Write in the Minecraft chat as the server (administrators only)",
		DefaultMemberPermissions: &adminOnly,
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionString, Name: "message", Description: "The message",
				Required: true, MaxLength: maxInput,
			},
		},
	},
	{
		Name:                     "setup",
		Description:              "Choose the operator role and the log channel (administrators only)",
		DefaultMemberPermissions: &adminOnly,
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Members with this role can start, stop and switch the server"},
			{
				Type: discordgo.ApplicationCommandOptionChannel, Name: "channel", Description: "Where the bot posts its log",
				ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText},
			},
		},
	},
}

// adminOnly hides /command, /chat and /setup from everyone but
// administrators; their handlers check again.
var adminOnly = int64(discordgo.PermissionAdministrator)

// maxInput keeps the escaped text of /command and /chat, and the log line
// that repeats it, under the 2000 characters of a Discord message.
const (
	maxInput  = 500
	maxOutput = 900
)

// noMentions keeps user mentions in bot messages from pinging anyone.
var noMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}

const (
	msgDenied    = "You do not have permission to use this command."
	msgBusy      = "An operation is already in progress, try again shortly."
	msgNotYours  = "Only the person who ran the command can use this."
	msgExpired   = "Time is up: run the command again."
	msgStopFirst = "Stop the server first."
	msgFailed    = "Something went wrong. Check the bot logs."

	msgWrongGuild = "My commands are not active on this Discord server."
)

// Custom IDs of the buttons and menus: action|user ID|expiry (unix)|argument.
const (
	actionStop    = "stop"
	actionPick    = "pick"
	actionConfirm = "select"
)

func (b *Bot) handle(i *discordgo.Interaction) {
	user := userOf(i)
	if i.GuildID == "" || i.GuildID != b.activeGuild() {
		slog.Warn("interaction from an inactive guild", "guild_id", i.GuildID, "user_id", user.ID)
		b.reply(i, msgWrongGuild, true)
		return
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		name := i.ApplicationCommandData().Name
		slog.Info("command", "name", name, "user_id", user.ID, "user", user.Username)
		switch name {
		case "startserver":
			b.startServer(i)
		case "stopserver":
			b.stopServer(i)
		case "config":
			b.config(i)
		case "status":
			b.status(i)
		case "onlineplayer":
			b.onlinePlayers(i)
		case "command":
			b.command(i)
		case "chat":
			b.chat(i)
		case "setup":
			b.setup(i)
		default:
			slog.Warn("unknown command", "name", name)
		}
	case discordgo.InteractionMessageComponent:
		action, _, _ := strings.Cut(i.MessageComponentData().CustomID, "|")
		slog.Info("component", "action", action, "user_id", user.ID, "user", user.Username)
		b.component(i)
	}
}

// operator answers with an ephemeral refusal, and records the attempt, if
// the user is not an operator. It runs before any SSH or RCON call.
func (b *Bot) operator(i *discordgo.Interaction, command string) bool {
	if isOperator(i.Member, b.operatorRole()) {
		return true
	}
	user := userOf(i)
	b.audit(user, "denied "+command, fmt.Sprintf("🚫 <@%s> tried to use %s without permission.", user.ID, command))
	b.reply(i, msgDenied, true)
	return false
}

func (b *Bot) startServer(i *discordgo.Interaction) {
	if !b.operator(i, "/startserver") {
		return
	}
	if !b.ops.TryLock() {
		b.reply(i, msgBusy, true)
		return
	}
	defer b.ops.Unlock()
	if !b.deferReply(i, false) {
		return
	}

	user, name := userOf(i), b.serverName()
	err := b.srv.Start(b.ctx)
	switch {
	case errors.Is(err, monitor.ErrState):
		b.edit(i, "The server is not off.", nil)
	case err != nil:
		slog.Error("start", "error", err)
		b.edit(i, msgFailed, nil)
	default:
		b.audit(user, "start", fmt.Sprintf("▶️ <@%s> started **%s**.", user.ID, name))
		b.edit(i, fmt.Sprintf("🟡 Starting **%s**…", name), nil)
		b.kickPresence()
	}
}

func (b *Bot) stopServer(i *discordgo.Interaction) {
	if !b.operator(i, "/stopserver") {
		return
	}
	snap := b.srv.Snapshot()
	switch {
	case snap.State == monitor.Off || snap.State == monitor.Stopping:
		b.reply(i, "The server is already off or stopping.", true)
	case snap.Players > 0:
		id := customID(actionStop, userOf(i).ID, b.now().Add(stopConfirmTTL), "")
		b.respond(i, discordgo.InteractionResponseChannelMessageWithSource, &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("There are %d players online. Confirm the shutdown within %d seconds?",
				snap.Players, int(stopConfirmTTL.Seconds())),
			Components: buttonRow(discordgo.Button{Label: "Stop", Style: discordgo.DangerButton, CustomID: id}),
			Flags:      discordgo.MessageFlagsEphemeral,
		})
	default:
		b.runStop(i, discordgo.InteractionResponseDeferredChannelMessageWithSource)
	}
}

// runStop takes the lock and runs the stop flow. ack is the deferred
// response type: a new message for a command, an update for a button.
func (b *Bot) runStop(i *discordgo.Interaction, ack discordgo.InteractionResponseType) {
	if !b.ops.TryLock() {
		b.reply(i, msgBusy, true)
		return
	}
	defer b.ops.Unlock()
	if !b.respond(i, ack, nil) {
		return
	}

	user, name := userOf(i), b.serverName()
	b.audit(user, "stop", fmt.Sprintf("⏹️ <@%s> asked to stop **%s**.", user.ID, name))
	b.edit(i, fmt.Sprintf("🟠 Stopping **%s**…", name), &[]discordgo.MessageComponent{})
	b.kickPresence()

	err := b.srv.Stop(b.ctx)
	switch {
	case errors.Is(err, monitor.ErrState):
		b.edit(i, "The server is already off or stopping.", nil)
	case err != nil:
		slog.Error("stop", "error", err)
		b.edit(i, msgFailed, nil)
	default:
		b.edit(i, fmt.Sprintf("🔴 **%s** stopped.", name), nil)
	}
}

func (b *Bot) config(i *discordgo.Interaction) {
	if !b.operator(i, "/config") {
		return
	}
	snap := b.srv.Snapshot()
	if snap.State != monitor.Off {
		b.reply(i, msgStopFirst, true)
		return
	}
	if !b.deferReply(i, true) {
		return
	}
	names, err := b.srv.List(b.ctx)
	if err != nil {
		slog.Error("list servers", "error", err)
		b.edit(i, msgFailed, nil)
		return
	}
	if len(names) == 0 {
		b.edit(i, "No servers available.", nil)
		return
	}
	if len(names) > maxSelectOptions {
		slog.Warn("too many servers for one menu, showing the first ones", "servers", len(names))
		names = names[:maxSelectOptions]
	}

	user := userOf(i)
	expiry := b.now().Add(configMenuTTL)
	options := make([]discordgo.SelectMenuOption, 0, len(names))
	for _, n := range names {
		// The confirmation button carries the folder name in its custom ID.
		if len(customID(actionConfirm, user.ID, expiry, n)) > maxCustomID {
			slog.Warn("server folder name too long for the menu", "server", n)
			continue
		}
		opt := discordgo.SelectMenuOption{Label: truncate(b.opts.DisplayName(n)), Value: n, Default: n == snap.Server}
		if opt.Label != n {
			opt.Description = truncate(n)
		}
		options = append(options, opt)
	}
	menu := discordgo.SelectMenu{
		CustomID:    customID(actionPick, user.ID, expiry, ""),
		Placeholder: "Choose a server",
		Options:     options,
	}
	b.edit(i, fmt.Sprintf("Active server: **%s**. Which one do you want to activate?", b.opts.DisplayName(snap.Server)),
		&[]discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{menu}}})
}

func (b *Bot) status(i *discordgo.Interaction) {
	s := b.srv.Snapshot()
	name := b.opts.DisplayName(s.Server)
	var text string
	switch s.State {
	case monitor.Off:
		text = fmt.Sprintf("🔴 **%s** · off", name)
	case monitor.Starting:
		text = fmt.Sprintf("🟡 **%s** · starting", name)
	case monitor.Online:
		text = fmt.Sprintf("🟢 **%s** · online for %s\nPlayers: %d/%d · Version: %s",
			name, formatDuration(b.now().Sub(s.Since)), s.Players, s.MaxPlayers, escape(s.Version))
	case monitor.Stopping:
		text = fmt.Sprintf("🟠 **%s** · stopping", name)
	case monitor.Unresponsive:
		text = fmt.Sprintf("⚠️ **%s** · not responding", name)
	}
	b.reply(i, text, false)
}

func (b *Bot) onlinePlayers(i *discordgo.Interaction) {
	if b.srv.Snapshot().State != monitor.Online {
		b.reply(i, "The server is not online.", true)
		return
	}
	if !b.deferReply(i, false) {
		return
	}
	players, err := b.srv.Players(b.ctx)
	switch {
	case errors.Is(err, monitor.ErrState):
		b.edit(i, "The server is not online.", nil)
	case err != nil:
		slog.Error("list players", "error", err)
		b.edit(i, msgFailed, nil)
	case len(players) == 0:
		b.edit(i, "No players online.", nil)
	default:
		// shortcut: no split over 2000 characters, about 100 players; split if servers get that big.
		for n, p := range players {
			players[n] = escape(p)
		}
		b.edit(i, fmt.Sprintf("Players online (%d): %s", len(players), strings.Join(players, ", ")), nil)
	}
}

// administrator answers with an ephemeral refusal, and records the attempt,
// if the user is not an administrator.
func (b *Bot) administrator(i *discordgo.Interaction, command string) bool {
	if i.Member != nil && i.Member.Permissions&discordgo.PermissionAdministrator != 0 {
		return true
	}
	user := userOf(i)
	b.audit(user, "denied "+command, fmt.Sprintf("🚫 <@%s> tried to use %s without permission.", user.ID, command))
	b.reply(i, "Only administrators can use "+command+".", true)
	return false
}

// command runs any console command. Only administrators may use it: it can
// do everything the server console can, operators included.
func (b *Bot) command(i *discordgo.Interaction) {
	if !b.administrator(i, "/command") {
		return
	}
	if b.srv.Snapshot().State != monitor.Online {
		b.reply(i, "The server is not online.", true)
		return
	}
	if !b.deferReply(i, true) {
		return
	}
	user, command := userOf(i), i.ApplicationCommandData().Options[0].StringValue()
	out, err := b.srv.Command(b.ctx, command)
	switch {
	case errors.Is(err, monitor.ErrState):
		b.edit(i, "The server is not online.", nil)
	case err != nil:
		slog.Error("command", "error", err)
		b.edit(i, msgFailed, nil)
	default:
		b.audit(user, "command", fmt.Sprintf("⌨️ <@%s> ran: %s", user.ID, escape(command)))
		if r := []rune(out); len(r) > maxOutput {
			out = string(r[:maxOutput]) + "…"
		}
		if strings.TrimSpace(out) == "" {
			out = "Done, no output."
		}
		b.edit(i, escape(out), nil)
	}
}

// chat writes a message in the game chat with the say command.
func (b *Bot) chat(i *discordgo.Interaction) {
	if !b.administrator(i, "/chat") {
		return
	}
	if b.srv.Snapshot().State != monitor.Online {
		b.reply(i, "The server is not online.", true)
		return
	}
	if !b.deferReply(i, true) {
		return
	}
	user, message := userOf(i), i.ApplicationCommandData().Options[0].StringValue()
	err := b.srv.Chat(b.ctx, message)
	switch {
	case errors.Is(err, monitor.ErrState):
		b.edit(i, "The server is not online.", nil)
	case err != nil:
		slog.Error("chat", "error", err)
		b.edit(i, msgFailed, nil)
	default:
		b.audit(user, "chat", fmt.Sprintf("💬 <@%s> wrote in the game chat: %s", user.ID, escape(message)))
		b.edit(i, "✅ Sent.", nil)
	}
}

// component handles the stop button, the server menu and its confirmation.
func (b *Bot) component(i *discordgo.Interaction) {
	data := i.MessageComponentData()
	parts := strings.SplitN(data.CustomID, "|", 4)
	if len(parts) != 4 {
		slog.Warn("unknown component", "custom_id", data.CustomID)
		return
	}
	action, owner, arg := parts[0], parts[1], parts[3]
	expiry, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		slog.Warn("unknown component", "custom_id", data.CustomID)
		return
	}
	if userOf(i).ID != owner {
		b.reply(i, msgNotYours, true)
		return
	}
	if b.now().Unix() > expiry {
		b.respond(i, discordgo.InteractionResponseUpdateMessage, &discordgo.InteractionResponseData{
			Content: msgExpired, Components: []discordgo.MessageComponent{},
		})
		return
	}
	command := "/config"
	if action == actionStop {
		command = "/stopserver"
	}
	if !b.operator(i, command) {
		return
	}

	switch action {
	case actionStop:
		b.runStop(i, discordgo.InteractionResponseDeferredMessageUpdate)
	case actionPick:
		if len(data.Values) != 1 {
			return
		}
		folder := data.Values[0]
		b.respond(i, discordgo.InteractionResponseUpdateMessage, &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("Activate **%s**?", b.opts.DisplayName(folder)),
			Components: buttonRow(discordgo.Button{
				Label: "Confirm", Style: discordgo.PrimaryButton,
				CustomID: customID(actionConfirm, owner, time.Unix(expiry, 0), folder),
			}),
		})
	case actionConfirm:
		b.selectServer(i, arg)
	}
}

func (b *Bot) selectServer(i *discordgo.Interaction, folder string) {
	if !b.ops.TryLock() {
		b.reply(i, msgBusy, true)
		return
	}
	defer b.ops.Unlock()
	if !b.respond(i, discordgo.InteractionResponseDeferredMessageUpdate, nil) {
		return
	}

	user, prev := userOf(i), b.serverName()
	err := b.srv.Select(b.ctx, folder)
	switch {
	case errors.Is(err, monitor.ErrState):
		b.edit(i, msgStopFirst, &[]discordgo.MessageComponent{})
	case err != nil:
		slog.Error("select server", "error", err)
		b.edit(i, msgFailed, &[]discordgo.MessageComponent{})
	default:
		next := b.opts.DisplayName(folder)
		b.audit(user, "select", fmt.Sprintf("🔁 <@%s> switched server: **%s** → **%s**.", user.ID, prev, next))
		b.edit(i, fmt.Sprintf("✅ Active server: **%s**.", next), &[]discordgo.MessageComponent{})
	}
}

func (b *Bot) serverName() string {
	return b.opts.DisplayName(b.srv.Snapshot().Server)
}

func (b *Bot) respond(i *discordgo.Interaction, typ discordgo.InteractionResponseType, data *discordgo.InteractionResponseData) bool {
	if data != nil {
		data.AllowedMentions = noMentions
	}
	if err := b.s.InteractionRespond(i, &discordgo.InteractionResponse{Type: typ, Data: data}); err != nil {
		slog.Warn("respond to interaction", "error", err)
		return false
	}
	return true
}

func (b *Bot) reply(i *discordgo.Interaction, text string, ephemeral bool) bool {
	data := &discordgo.InteractionResponseData{Content: text}
	if ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	return b.respond(i, discordgo.InteractionResponseChannelMessageWithSource, data)
}

// deferReply acknowledges a command that needs SSH or RCON, which may take
// longer than the 3 seconds Discord waits for an answer.
func (b *Bot) deferReply(i *discordgo.Interaction, ephemeral bool) bool {
	data := &discordgo.InteractionResponseData{}
	if ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	return b.respond(i, discordgo.InteractionResponseDeferredChannelMessageWithSource, data)
}

// edit replaces the deferred answer. components nil keeps the current ones.
func (b *Bot) edit(i *discordgo.Interaction, text string, components *[]discordgo.MessageComponent) {
	_, err := b.s.InteractionResponseEdit(i, &discordgo.WebhookEdit{
		Content: &text, Components: components, AllowedMentions: noMentions,
	})
	if err != nil {
		slog.Warn("edit interaction response", "error", err)
	}
}

func userOf(i *discordgo.Interaction) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	if i.User != nil {
		return i.User
	}
	return &discordgo.User{}
}

func customID(action, userID string, expiry time.Time, arg string) string {
	return fmt.Sprintf("%s|%s|%d|%s", action, userID, expiry.Unix(), arg)
}

func buttonRow(button discordgo.Button) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{button}}}
}

func truncate(s string) string {
	if r := []rune(s); len(r) > maxOptionText {
		return string(r[:maxOptionText-1]) + "…"
	}
	return s
}

var escape = strings.NewReplacer(`\`, `\\`, "_", `\_`, "*", `\*`, "~", `\~`, "`", "\\`", "|", `\|`).Replace
