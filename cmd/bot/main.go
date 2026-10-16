// Command bot runs the Discord bot that controls the Minecraft server.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/EdoUbba/minecraft-server-discord-bot/internal/config"
	"github.com/EdoUbba/minecraft-server-discord-bot/internal/control"
	"github.com/EdoUbba/minecraft-server-discord-bot/internal/discord"
	"github.com/EdoUbba/minecraft-server-discord-bot/internal/minecraft"
	"github.com/EdoUbba/minecraft-server-discord-bot/internal/monitor"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "config.toml", "path to the configuration file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load configuration", "error", err)
		return 1
	}
	guild := cfg.Discord.GuildID
	if guild == "" {
		guild = "auto"
	}
	logger.Info("configuration loaded", "guild_id", guild)

	ctl, err := control.New(cfg.SSH.Host, cfg.SSH.Port, cfg.SSH.User, cfg.SSH.KeyPath, cfg.SSH.KnownHosts)
	if err != nil {
		logger.Error("failed to set up ssh", "error", err)
		return 1
	}
	var emptyTimeout time.Duration
	if cfg.Autostop.Enabled {
		emptyTimeout = cfg.Autostop.EmptyTimeout()
	}

	var bot *discord.Bot
	mon := monitor.New(
		minecraft.NewPinger(cfg.Server.Host, cfg.Server.Port),
		minecraft.NewRCON(cfg.Server.Host, cfg.Server.RCONPort, cfg.Server.RCONPassword.Reveal()),
		ctl,
		monitor.Options{
			Interval:     cfg.Status.Interval.Std(),
			StartTimeout: cfg.Server.StartTimeout.Std(),
			StopTimeout:  cfg.Server.StopTimeout.Std(),
			EmptyTimeout: emptyTimeout,
			// Events only fire from Run and the commands, after bot is set.
			Notify: func(e monitor.Event) { bot.Notify(e) },
		},
	)
	bot = discord.New(mon, discord.Options{
		GuildID:        cfg.Discord.GuildID,
		OperatorRoleID: cfg.Discord.OperatorRoleID,
		LogChannelID:   cfg.Discord.LogChannelID,
		SettingsFile:   cfg.Discord.SettingsFile,
		Interval:       cfg.Status.Interval.Std(),
		EmptyTimeout:   emptyTimeout,
		DisplayName:    cfg.Server.DisplayName,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := mon.Init(ctx); err != nil {
		logger.Error("failed to read the server state over ssh", "error", err)
		return 1
	}
	snap := mon.Snapshot()
	logger.Info("server state", "state", snap.State, "server", snap.Server)
	go mon.Run(ctx)

	if err := bot.Run(ctx, cfg.Discord.Token.Reveal()); err != nil {
		logger.Error("discord", "error", err)
		return 1
	}
	logger.Info("shutting down")
	return 0
}
