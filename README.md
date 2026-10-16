# Minecraft server Discord bot

A Discord bot, written in Go, that starts and stops a Minecraft Java server,
shows who is online and keeps the server state in its own profile.

- `/startserver`, `/stopserver` and `/config` (choose which server to run)
  for operators; `/status` and `/onlineplayer` for everyone.
- The presence shows the server, its uptime and the players, for example
  *Playing 🟢 Survival · da 2h 15m · 3/20*.
- An empty server stops by itself after a configurable time; a server that
  stops answering is stopped cleanly. A crashed server is never restarted.
- Every start, stop, server change and refused attempt goes to a log channel.

The bot talks Italian to its users; the code and this guide are in English.

## How it works

```
┌──────────── bot host ────────────┐         ┌───────── Minecraft host ─────────┐
│ Docker: the bot (non-root)       │── SSH ─▶│ minecraft-ctl (forced command)   │
│                                  │         │   └─ sudo systemctl start|stop   │
│                                  │         │        minecraft.service         │
│                                  │── ping, RCON (LAN) ─▶ Minecraft server     │
└──────────────────────────────────┘         └──────────────────────────────────┘
```

The bot never gets a shell on the Minecraft host. Its SSH key is bound to
`minecraft-ctl`, which accepts five commands only: `start`, `stop`, `status`,
`list` and `select <name>`. The server state comes from the Minecraft ping;
SSH is used only for actions, once at bot startup and to confirm a crash.

The two hosts can be any Linux machines; this guide uses two Proxmox LXCs.

## 1. Minecraft host

Everything in this section runs as root on the Minecraft host. It assumes a
Debian-like system with systemd, Java and `sudo` installed.

### Server folders

Put each server in its own folder under one base folder, for example
`/opt/mc/survival` and `/opt/mc/creative`. A folder is a server if it contains
`start.sh` or `startserver.sh`. The scripts must run the server in the
foreground and exit when it stops: no `while true` restart loops and no final
`pause`.

### Users

```sh
# Runs the server and owns the server folders. If the folders already belong
# to a user (for example Crafty's), skip this and use that user below.
useradd --system --create-home --shell /usr/sbin/nologin minecraft
chown -R minecraft:minecraft /opt/mc

# The bot's user: no password, key login only. Its shell must be a real shell,
# because sshd runs the forced command through it.
useradd --create-home --shell /bin/bash mcbot
passwd -l mcbot

# minecraft-ctl keeps the active server link here.
install -d -o mcbot -g mcbot -m 755 /var/lib/minecraft-ctl
```

### minecraft-ctl

Copy [`deploy/minecraft-ctl`](deploy/minecraft-ctl) to the host and set `BASE`
at the top of the file to your servers folder (default `/opt/mc`). Then:

```sh
install -o root -g root -m 755 minecraft-ctl /usr/local/bin/minecraft-ctl
```

Point the active server link at one of your servers:

```sh
sudo -u mcbot ln -s /opt/mc/survival /var/lib/minecraft-ctl/current
```

### minecraft.service

Copy [`deploy/minecraft.service`](deploy/minecraft.service) to
`/etc/systemd/system/` and change `User=` if the folders belong to another
user. Do **not** enable it: the bot starts and stops it.

```sh
systemctl daemon-reload
```

The unit stops the server with SIGINT, like Ctrl+C, so Minecraft saves the
worlds; it treats exit code 130 as a clean stop and anything else as a crash.

If `java` is not in `/usr/bin` (for example installed with SDKMAN under
`/root`, which the server user cannot read), copy the JDK to a shared folder
such as `/opt/java/<version>` and add a drop-in,
`/etc/systemd/system/minecraft.service.d/java.conf`:

```ini
[Service]
Environment=PATH=/opt/java/<version>/bin:/usr/local/bin:/usr/bin:/bin
```

Check that the service starts and stops before going on:

```sh
systemctl start minecraft && journalctl -fu minecraft    # Ctrl+C when ready
systemctl stop minecraft && systemctl show -p ActiveState minecraft   # inactive
```

### sudoers

```sh
visudo -f /etc/sudoers.d/minecraft-ctl
```

and paste the line from [`deploy/sudoers.example`](deploy/sudoers.example). It
lets `mcbot` run `systemctl start`, `stop` and `show` on `minecraft.service`
and nothing else.

### RCON

In each server's `server.properties`:

```properties
enable-rcon=true
rcon.port=25575
rcon.password=<a long random password, for example from: openssl rand -base64 24>
```

All the servers the bot can select must use the same RCON port and password.

### Firewall

Open port 22 (SSH) and 25575 (RCON) only to the bot host, and 25565 to the
players. RCON is not encrypted: it must not be reachable from anywhere else.

## 2. Discord application

1. In the [Developer Portal](https://discord.com/developers/applications)
   create an application, open **Bot** and copy the token
   (**Reset Token**). No privileged intent is needed.
2. Invite the bot, with your Application ID from **General Information**:

   ```
   https://discord.com/oauth2/authorize?client_id=<APPLICATION_ID>&scope=bot+applications.commands&permissions=3072
   ```

   3072 lets it read and write messages, which it needs for the log channel.
3. In Discord turn on **User Settings → Advanced → Developer Mode**, then
   right-click your server and **Copy Server ID**: that is the `guild_id`.

The bot registers its commands on that server by itself at every start.

## 3. Bot host

Any Linux host with Docker and the Compose plugin (v2.24 or later). On Proxmox, an
unprivileged LXC needs **Options → Features → nesting** (and **keyctl**) for
Docker to run.

On Debian, install Docker and the Compose plugin from Docker's repository, following
[docs.docker.com/engine/install/debian](https://docs.docker.com/engine/install/debian/),
plus `git`. Check with `docker compose version`.

If the repository is private, give the host a read-only deploy key: create it
with `ssh-keygen -t ed25519 -N "" -f ~/.ssh/github_deploy`, add the `.pub`
under **Settings → Deploy keys** on GitHub (no write access), point
`github.com` to it in `~/.ssh/config` (`IdentityFile ~/.ssh/github_deploy`)
and clone with `git@github.com:EdoUbba/minecraft-server-discord-bot.git`.

```sh
git clone https://github.com/EdoUbba/minecraft-server-discord-bot.git
cd minecraft-server-discord-bot
mkdir -p secrets data
```

### SSH key and host key

```sh
ssh-keygen -t ed25519 -N "" -C minecraft-bot -f secrets/id_ed25519
ssh-keyscan -t ed25519 <minecraft-host> > secrets/known_hosts
ssh-keygen -lf secrets/known_hosts
```

Compare the fingerprint printed by the last command with the one on the
Minecraft host (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`): if they
differ, stop and find out why.

On the Minecraft host, put the public key (`secrets/id_ed25519.pub`) in
`~mcbot/.ssh/authorized_keys`, bound to `minecraft-ctl`:

```sh
install -d -o mcbot -g mcbot -m 700 ~mcbot/.ssh
echo 'command="/usr/local/bin/minecraft-ctl",no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding ssh-ed25519 AAAA... minecraft-bot' \
  >> ~mcbot/.ssh/authorized_keys
chown mcbot:mcbot ~mcbot/.ssh/authorized_keys && chmod 600 ~mcbot/.ssh/authorized_keys
```

Test it from the bot host: it must print the service state and nothing else.

```sh
ssh -i secrets/id_ed25519 -o UserKnownHostsFile=secrets/known_hosts mcbot@<minecraft-host> status
```

### Configuration

```sh
cp config.example.toml config.toml
```

Fill in `guild_id`, the two `host` values and the server names you want to
show (`[server.names]`). Leave the paths as they are: they are the paths
inside the container. Every option is described in
[`config.example.toml`](config.example.toml).

Put the token and the RCON password in `secrets/bot.env` rather than in
`config.toml`:

```sh
cat > secrets/bot.env <<'EOF'
DISCORD_TOKEN=<token>
RCON_PASSWORD=<password>
EOF
```

The container runs as uid 65532, which must be able to read the secrets and
write `data/`:

```sh
chmod 600 secrets/* config.toml
chown -R 65532:65532 secrets data config.toml
```

### Start

```sh
docker compose up -d --build
docker compose logs -f
```

The log shows `connected to discord`. The container is limited to 64 MB of
memory (`docker stats` shows the usage) and restarts by itself unless you
stop it with `docker compose down`.

When the bot joins your server it writes a welcome message. Then, as an
administrator, run `/setup` to choose the operator role and the log channel;
the choice is saved in `data/settings.json` and overrides `config.toml`.

To update: `git pull && docker compose up -d --build`.

## Commands

| Command | Who | What it does |
| --- | --- | --- |
| `/startserver` | Operators | Starts the server |
| `/stopserver` | Operators | Warns the players and stops the server; asks for confirmation when someone is online |
| `/config` | Operators | Chooses the server to run, from a menu; only while the server is off |
| `/status` | Everyone | Server, state, uptime, players, version |
| `/onlineplayer` | Everyone | Names of the online players |
| `/setup` | Administrators | Operator role and log channel |

Operators are the members with the Administrator permission and the members
with the operator role. Everyone sees the commands; the bot refuses, with a
message only the user sees, those they cannot use.

## Security

- The bot runs as a non-root user in a read-only container with no
  capabilities; the Minecraft server runs as a non-root user too.
- A stolen bot key can only start, stop or switch between the existing
  servers: no shell, no forwarding, no paths.
- The SSH host key is always checked against `secrets/known_hosts`.
- Token, RCON password and key never appear in the logs, and the server
  address never appears in Discord.
- `config.toml`, `secrets/` and `data/` are ignored by git and Docker builds.

## Development

```sh
go test ./...
go run ./cmd/bot -config config.toml
```

`internal/minecraft` and `internal/control` also have live tests against a
real server; see the comment at the top of their `live_test.go`.
