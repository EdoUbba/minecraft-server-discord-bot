// Command bot runs the Discord bot that controls the Minecraft server.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/EdoUbba/minecraft-server-discord-bot/internal/config"
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
	logger.Info("configuration loaded", "guild_id", cfg.Discord.GuildID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	logger.Info("shutting down")
	return 0
}
