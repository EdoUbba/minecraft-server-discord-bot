// Package config loads and validates the bot configuration from config.toml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Environment variables that take precedence over the values in the file.
const (
	EnvDiscordToken = "DISCORD_TOKEN"
	EnvRCONPassword = "RCON_PASSWORD"
)

// MinStatusInterval is the shortest allowed status polling interval.
const MinStatusInterval = 30 * time.Second

type Config struct {
	Discord  Discord  `toml:"discord"`
	SSH      SSH      `toml:"ssh"`
	Server   Server   `toml:"server"`
	Status   Status   `toml:"status"`
	Autostop Autostop `toml:"autostop"`
}

type Discord struct {
	Token          Secret `toml:"token"`
	GuildID        string `toml:"guild_id"`
	OperatorRoleID string `toml:"operator_role_id"` // empty = only administrators are operators
	LogChannelID   string `toml:"log_channel_id"`   // empty = log channel disabled
}

type SSH struct {
	Host       string `toml:"host"`
	Port       int    `toml:"port"`
	User       string `toml:"user"`
	KeyPath    string `toml:"key_path"`
	KnownHosts string `toml:"known_hosts"`
}

type Server struct {
	Host         string   `toml:"host"` // never shown to users
	Port         int      `toml:"port"`
	RCONPort     int      `toml:"rcon_port"`
	RCONPassword Secret   `toml:"rcon_password"`
	StartTimeout Duration `toml:"start_timeout"`
	StopTimeout  Duration `toml:"stop_timeout"`
	// Names maps a server folder name to the name shown to users.
	Names map[string]string `toml:"names"`
}

type Status struct {
	Interval Duration `toml:"interval"`
}

type Autostop struct {
	Enabled      bool `toml:"enabled"`
	EmptyMinutes int  `toml:"empty_minutes"`
}

// DisplayName returns the name shown to users for the given server folder.
func (s Server) DisplayName(folder string) string {
	if name, ok := s.Names[folder]; ok {
		return name
	}
	return folder
}

// EmptyTimeout is how long the server may stay online with no players before
// it is stopped automatically.
func (a Autostop) EmptyTimeout() time.Duration {
	return time.Duration(a.EmptyMinutes) * time.Minute
}

func defaults() Config {
	return Config{
		SSH: SSH{Port: 22},
		Server: Server{
			Port:         25565,
			RCONPort:     25575,
			StartTimeout: Duration(5 * time.Minute),
			StopTimeout:  Duration(90 * time.Second),
		},
		Status:   Status{Interval: Duration(60 * time.Second)},
		Autostop: Autostop{Enabled: true, EmptyMinutes: 10},
	}
}

// Load reads the configuration file at path, applies the environment
// overrides and validates the result.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read configuration file: %w", err)
	}
	return parse(data, os.LookupEnv)
}

func parse(data []byte, lookupEnv func(string) (string, bool)) (*Config, error) {
	// Editors on Windows often save UTF-8 files with a byte order mark.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	cfg := defaults()
	dec := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var strictErr *toml.StrictMissingError
		if errors.As(err, &strictErr) {
			return nil, fmt.Errorf("parse configuration file: unknown keys:\n%s", strictErr.String())
		}
		return nil, fmt.Errorf("parse configuration file: %w", err)
	}

	if v, ok := lookupEnv(EnvDiscordToken); ok && v != "" {
		cfg.Discord.Token = Secret(v)
	}
	if v, ok := lookupEnv(EnvRCONPassword); ok && v != "" {
		cfg.Server.RCONPassword = Secret(v)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	required := func(key, value string) {
		if value == "" {
			fail("%s is required", key)
		}
	}

	required("discord.token (or "+EnvDiscordToken+")", c.Discord.Token.Reveal())
	required("discord.guild_id", c.Discord.GuildID)
	checkSnowflake(fail, "discord.guild_id", c.Discord.GuildID)
	checkSnowflake(fail, "discord.operator_role_id", c.Discord.OperatorRoleID)
	checkSnowflake(fail, "discord.log_channel_id", c.Discord.LogChannelID)

	required("ssh.host", c.SSH.Host)
	checkPort(fail, "ssh.port", c.SSH.Port)
	required("ssh.user", c.SSH.User)
	required("ssh.key_path", c.SSH.KeyPath)
	required("ssh.known_hosts", c.SSH.KnownHosts)

	required("server.host", c.Server.Host)
	checkPort(fail, "server.port", c.Server.Port)
	checkPort(fail, "server.rcon_port", c.Server.RCONPort)
	required("server.rcon_password (or "+EnvRCONPassword+")", c.Server.RCONPassword.Reveal())
	if c.Server.StartTimeout <= 0 {
		fail("server.start_timeout must be greater than zero")
	}
	if c.Server.StopTimeout <= 0 {
		fail("server.stop_timeout must be greater than zero")
	}
	for folder, name := range c.Server.Names {
		if name == "" {
			fail("server.names.%s must not be empty", folder)
		}
	}

	if c.Status.Interval.Std() < MinStatusInterval {
		fail("status.interval must be at least %s, got %s", MinStatusInterval, c.Status.Interval)
	}

	if c.Autostop.Enabled && c.Autostop.EmptyMinutes < 1 {
		fail("autostop.empty_minutes must be at least 1, got %d", c.Autostop.EmptyMinutes)
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}

// checkSnowflake reports an error if a non-empty value is not a Discord ID.
func checkSnowflake(fail func(string, ...any), key, value string) {
	if value == "" {
		return
	}
	if _, err := strconv.ParseUint(value, 10, 64); err != nil {
		fail("%s must be a numeric Discord ID", key)
	}
}

func checkPort(fail func(string, ...any), key string, port int) {
	if port < 1 || port > 65535 {
		fail("%s must be between 1 and 65535, got %d", key, port)
	}
}
