package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `
[discord]
token = "file-token"
guild_id = "123456789012345678"
operator_role_id = "223456789012345678"
log_channel_id = ""

[ssh]
host = "192.168.1.50"
port = 22
user = "mcbot"
key_path = "/app/secrets/id_ed25519"
known_hosts = "/app/secrets/known_hosts"

[server]
host = "192.168.1.50"
port = 25565
rcon_port = 25575
rcon_password = "file-password"
start_timeout = "5m"
stop_timeout = "90s"

[server.names]
survival = "Survival Friends"

[status]
interval = "60s"

[autostop]
enabled = true
empty_minutes = 10
`

func noEnv(string) (string, bool) { return "", false }

func envOf(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

// replaceLine replaces the first line of data that sets key.
func replaceLine(t *testing.T, data, key, replacement string) string {
	t.Helper()
	lines := strings.Split(data, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, key+" ") {
			lines[i] = replacement
			return strings.Join(lines, "\n")
		}
	}
	t.Fatalf("key %q not found", key)
	return ""
}

func TestParseValidConfig(t *testing.T) {
	cfg, err := parse([]byte(validConfig), noEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := cfg.Discord.Token.Reveal(); got != "file-token" {
		t.Errorf("token = %q, want file-token", got)
	}
	if cfg.Server.StartTimeout.Std() != 5*time.Minute {
		t.Errorf("start_timeout = %s, want 5m", cfg.Server.StartTimeout)
	}
	if cfg.Server.StopTimeout.Std() != 90*time.Second {
		t.Errorf("stop_timeout = %s, want 90s", cfg.Server.StopTimeout)
	}
	if cfg.Status.Interval.Std() != time.Minute {
		t.Errorf("interval = %s, want 1m", cfg.Status.Interval)
	}
	if cfg.Autostop.EmptyTimeout() != 10*time.Minute {
		t.Errorf("empty timeout = %s, want 10m", cfg.Autostop.EmptyTimeout())
	}
	if got := cfg.Server.DisplayName("survival"); got != "Survival Friends" {
		t.Errorf("DisplayName(survival) = %q", got)
	}
	if got := cfg.Server.DisplayName("creative"); got != "creative" {
		t.Errorf("DisplayName(creative) = %q, want folder name fallback", got)
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	minimal := `
[discord]
token = "t"
guild_id = "123456789012345678"

[ssh]
host = "10.0.0.2"
user = "mcbot"
key_path = "/k"
known_hosts = "/kh"

[server]
host = "10.0.0.2"
rcon_password = "p"
`
	cfg, err := parse([]byte(minimal), noEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SSH.Port != 22 || cfg.Server.Port != 25565 || cfg.Server.RCONPort != 25575 {
		t.Errorf("default ports = %d/%d/%d", cfg.SSH.Port, cfg.Server.Port, cfg.Server.RCONPort)
	}
	if cfg.Server.StartTimeout.Std() != 5*time.Minute || cfg.Server.StopTimeout.Std() != 90*time.Second {
		t.Errorf("default timeouts = %s/%s", cfg.Server.StartTimeout, cfg.Server.StopTimeout)
	}
	if cfg.Status.Interval.Std() != 60*time.Second {
		t.Errorf("default interval = %s", cfg.Status.Interval)
	}
	if !cfg.Autostop.Enabled || cfg.Autostop.EmptyMinutes != 10 {
		t.Errorf("default autostop = %+v", cfg.Autostop)
	}
	if cfg.Discord.SettingsFile != "data/settings.json" {
		t.Errorf("default settings file = %q", cfg.Discord.SettingsFile)
	}
	if cfg.Discord.OperatorRoleID != "" || cfg.Discord.LogChannelID != "" {
		t.Errorf("optional IDs should stay empty")
	}
}

func TestParseEnvOverridesFile(t *testing.T) {
	env := envOf(map[string]string{
		EnvDiscordToken: "env-token",
		EnvRCONPassword: "env-password",
	})
	cfg, err := parse([]byte(validConfig), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Discord.Token.Reveal(); got != "env-token" {
		t.Errorf("token = %q, want env-token", got)
	}
	if got := cfg.Server.RCONPassword.Reveal(); got != "env-password" {
		t.Errorf("rcon password = %q, want env-password", got)
	}
}

func TestParseEnvFillsMissingSecrets(t *testing.T) {
	data := replaceLine(t, validConfig, "token", `token = ""`)
	data = replaceLine(t, data, "rcon_password", `rcon_password = ""`)
	env := envOf(map[string]string{EnvDiscordToken: "env-token", EnvRCONPassword: "env-password"})
	if _, err := parse([]byte(data), env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseEmptyEnvDoesNotOverride(t *testing.T) {
	cfg, err := parse([]byte(validConfig), envOf(map[string]string{EnvDiscordToken: ""}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Discord.Token.Reveal(); got != "file-token" {
		t.Errorf("token = %q, want file-token", got)
	}
}

func TestParseMissingRequiredFields(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"token", "discord.token"},
		{"guild_id", "discord.guild_id"},
		{"user", "ssh.user"},
		{"key_path", "ssh.key_path"},
		{"known_hosts", "ssh.known_hosts"},
		{"rcon_password", "server.rcon_password"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			data := replaceLine(t, validConfig, tt.key, tt.key+` = ""`)
			_, err := parse([]byte(data), noEnv)
			if err == nil || !strings.Contains(err.Error(), tt.want+" ") {
				t.Fatalf("error = %v, want mention of %s", err, tt.want)
			}
		})
	}
}

func TestParseMissingHosts(t *testing.T) {
	data := strings.ReplaceAll(validConfig, `host = "192.168.1.50"`, `host = ""`)
	_, err := parse([]byte(data), noEnv)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"ssh.host", "server.host"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention %s", err, key)
		}
	}
}

func TestParseReportsAllErrors(t *testing.T) {
	data := replaceLine(t, validConfig, "token", `token = ""`)
	data = replaceLine(t, data, "guild_id", `guild_id = ""`)
	_, err := parse([]byte(data), noEnv)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "discord.token") || !strings.Contains(err.Error(), "discord.guild_id") {
		t.Errorf("error should list every problem, got %q", err)
	}
}

func TestParseLimits(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		line    string
		wantErr string
	}{
		{"interval below minimum", "interval", `interval = "29s"`, "status.interval must be at least 30s"},
		{"interval at minimum", "interval", `interval = "30s"`, ""},
		{"interval invalid", "interval", `interval = "soon"`, "invalid duration"},
		{"ssh port zero", "port", `port = 0`, "ssh.port must be between 1 and 65535"},
		{"rcon port too high", "rcon_port", `rcon_port = 70000`, "server.rcon_port must be between 1 and 65535"},
		{"start timeout zero", "start_timeout", `start_timeout = "0s"`, "server.start_timeout must be greater than zero"},
		{"stop timeout negative", "stop_timeout", `stop_timeout = "-5s"`, "server.stop_timeout must be greater than zero"},
		{"empty minutes zero", "empty_minutes", `empty_minutes = 0`, "autostop.empty_minutes must be at least 1"},
		{"guild id not numeric", "guild_id", `guild_id = "my-guild"`, "discord.guild_id must be a numeric Discord ID"},
		{"role id not numeric", "operator_role_id", `operator_role_id = "admins"`, "discord.operator_role_id must be a numeric Discord ID"},
		{"empty display name", "survival", `survival = ""`, "server.names.survival must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse([]byte(replaceLine(t, validConfig, tt.key, tt.line)), noEnv)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseAutostopDisabledSkipsMinutesCheck(t *testing.T) {
	data := replaceLine(t, validConfig, "enabled", `enabled = false`)
	data = replaceLine(t, data, "empty_minutes", `empty_minutes = 0`)
	if _, err := parse([]byte(data), noEnv); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	data := replaceLine(t, validConfig, "interval", "interval = \"60s\"\nintervall = \"60s\"")
	_, err := parse([]byte(data), noEnv)
	if err == nil || !strings.Contains(err.Error(), "intervall") {
		t.Fatalf("error = %v, want unknown key intervall", err)
	}
}

func TestParseIgnoresByteOrderMark(t *testing.T) {
	if _, err := parse([]byte("\xef\xbb\xbf"+validConfig), noEnv); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseInvalidTOML(t *testing.T) {
	if _, err := parse([]byte("[discord\ntoken = "), noEnv); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	data := replaceLine(t, validConfig, "interval", `interval = "1s"`)
	env := envOf(map[string]string{EnvDiscordToken: "super-secret-token", EnvRCONPassword: "super-secret-password"})
	_, err := parse([]byte(data), env)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, secret := range []string{"super-secret-token", "super-secret-password", "file-token", "file-password", "/app/secrets"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %v", secret, err)
		}
	}
}

func TestSecretIsRedacted(t *testing.T) {
	s := Secret("hunter2")
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %s %#v", s, s, s), s.LogValue().String()} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("secret leaked in %q", out)
		}
	}
	cfg := Config{Discord: Discord{Token: s}}
	if out := fmt.Sprintf("%+v", cfg); strings.Contains(out, "hunter2") {
		t.Errorf("secret leaked when printing config: %q", out)
	}
}

func TestLoadFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDiscordToken, "")
	t.Setenv(EnvRCONPassword, "")
	if _, err := Load(path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// The example ships with empty secrets and IDs; fill them in and check
	// that everything else in the file is valid.
	text := replaceLine(t, string(data), "guild_id", `guild_id = "123456789012345678"`)
	env := envOf(map[string]string{EnvDiscordToken: "t", EnvRCONPassword: "p"})
	if _, err := parse([]byte(text), env); err != nil {
		t.Fatalf("config.example.toml is not valid: %v", err)
	}
}
