// Package control runs commands on the Minecraft LXC through SSH.
//
// On that host the bot's key is bound to a forced command, minecraft-ctl,
// which reads the requested command from SSH_ORIGINAL_COMMAND and accepts
// only:
//
//	start          systemctl start minecraft.service
//	stop           systemctl stop minecraft.service
//	status         one key=value per line: ActiveState, ActiveEnterTimestamp
//	               (systemd unix format, "@<seconds>") and Server
//	list           the names of the available servers, one per line
//	select <name>  switch the active server; refused while it is running
package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// DefaultTimeout bounds a whole command. It is longer than the 90 s the
// service gets to stop, because "systemctl stop" waits for the shutdown.
const DefaultTimeout = 2 * time.Minute

// Client runs minecraft-ctl commands. Every call opens its own connection and
// closes it afterwards: commands are rare, and the bot never polls over SSH.
type Client struct {
	addr    string
	config  *ssh.ClientConfig
	timeout time.Duration
}

// Status is the state of minecraft.service as reported by minecraft-ctl.
type Status struct {
	ActiveState string    // systemd ActiveState, such as "active" or "inactive"
	Since       time.Time // when the service last became active; zero if never
	Server      string    // folder name of the selected server
}

// New reads the private key and known_hosts files and checks that the host is
// listed in known_hosts. The host key is verified on every connection.
func New(host string, port int, user, keyPath, knownHostsPath string) (*Client, error) {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("ssh key: %w", withoutPath(err))
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		// The parser error is not wrapped, so nothing about the key leaks.
		return nil, errors.New("ssh key: cannot parse private key")
	}
	hostKeys, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("ssh known_hosts: %w", err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	algorithms, err := hostKeyAlgorithms(hostKeys, addr, &net.TCPAddr{IP: net.ParseIP(host), Port: port})
	if err != nil {
		return nil, err
	}
	return &Client{
		addr: addr,
		config: &ssh.ClientConfig{
			User:              user,
			Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback:   hostKeys,
			HostKeyAlgorithms: algorithms,
		},
		timeout: DefaultTimeout,
	}, nil
}

// Start starts the Minecraft service.
func (c *Client) Start(ctx context.Context) error {
	_, err := c.run(ctx, "start")
	return err
}

// Stop stops the Minecraft service and waits until it has stopped.
func (c *Client) Stop(ctx context.Context) error {
	_, err := c.run(ctx, "stop")
	return err
}

// Status returns the state of the service and the selected server.
func (c *Client) Status(ctx context.Context) (Status, error) {
	out, err := c.run(ctx, "status")
	if err != nil {
		return Status{}, err
	}
	return parseStatus(out)
}

// List returns the names of the servers that can be selected.
func (c *Client) List(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "list")
	if err != nil {
		return nil, err
	}
	return parseList(out), nil
}

// Select makes name the active server. The name must be one returned by List;
// anything else is refused before it reaches the server.
func (c *Client) Select(ctx context.Context, name string) error {
	if !validName(name) {
		return fmt.Errorf("ssh select: invalid server name %q", name)
	}
	names, err := c.List(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(names, name) {
		return fmt.Errorf("ssh select: unknown server %q", name)
	}
	_, err = c.run(ctx, "select "+name)
	return err
}

func (c *Client) run(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var dialer net.Dialer
	tcpConn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return "", fmt.Errorf("ssh connect: %w", err)
	}
	// Abort the handshake or a running command as soon as the context ends.
	stop := context.AfterFunc(ctx, func() { _ = tcpConn.Close() })
	defer stop()

	conn, chans, reqs, err := ssh.NewClientConn(tcpConn, c.addr, c.config)
	if err != nil {
		_ = tcpConn.Close()
		return "", fmt.Errorf("ssh handshake: %w", ctxErr(ctx, err))
	}
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", ctxErr(ctx, err))
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	if err := session.Run(command); err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			if msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); msg != "" {
				return "", fmt.Errorf("ssh %s: %s", command, msg)
			}
		}
		return "", fmt.Errorf("ssh %s: %w", command, ctxErr(ctx, err))
	}
	return stdout.String(), nil
}

// hostKeyAlgorithms returns the algorithms of the keys known_hosts lists for
// addr, or an error if the host is not listed. Restricting the handshake to
// them avoids a false key mismatch when the server prefers a key type that
// known_hosts does not contain.
func hostKeyAlgorithms(hostKeys ssh.HostKeyCallback, addr string, remote net.Addr) ([]string, error) {
	var keyErr *knownhosts.KeyError
	// A key that matches nothing makes the callback report every known key.
	if err := hostKeys(addr, remote, probeKey{}); !errors.As(err, &keyErr) || len(keyErr.Want) == 0 {
		return nil, fmt.Errorf("ssh known_hosts: no key for %s", addr)
	}
	var algorithms []string
	for _, known := range keyErr.Want {
		keyType := known.Key.Type()
		candidates := []string{keyType}
		if keyType == ssh.KeyAlgoRSA {
			candidates = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
		}
		for _, a := range candidates {
			if !slices.Contains(algorithms, a) {
				algorithms = append(algorithms, a)
			}
		}
	}
	return algorithms, nil
}

type probeKey struct{}

func (probeKey) Type() string                        { return "probe" }
func (probeKey) Marshal() []byte                     { return []byte("probe") }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("probe key") }

func parseStatus(out string) (Status, error) {
	var st Status
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			st.ActiveState = value
		case "ActiveEnterTimestamp":
			since, err := parseTimestamp(value)
			if err != nil {
				return Status{}, err
			}
			st.Since = since
		case "Server":
			st.Server = value
		}
	}
	if st.ActiveState == "" {
		return Status{}, errors.New("ssh status: missing ActiveState")
	}
	return st, nil
}

// parseTimestamp reads a timestamp printed by "systemctl show --timestamp=unix",
// such as "@1760630400". systemd prints an empty value or "@0" when unset.
func parseTimestamp(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	seconds, err := strconv.ParseInt(strings.TrimPrefix(value, "@"), 10, 64)
	if err != nil || !strings.HasPrefix(value, "@") {
		return time.Time{}, fmt.Errorf("ssh status: invalid ActiveEnterTimestamp %q", value)
	}
	if seconds == 0 {
		return time.Time{}, nil
	}
	return time.Unix(seconds, 0), nil
}

func parseList(out string) []string {
	names := []string{}
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// validName accepts a plain folder name: no path separators, no "." or "..",
// no leading "-" that could pass as an option, no spaces or control characters.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool {
		return r == '/' || r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// withoutPath drops the file path from an os error, so the key location
// never appears in logs.
func withoutPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

// ctxErr prefers the context error when the context caused the failure.
func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
