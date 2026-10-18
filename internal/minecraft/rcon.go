package minecraft

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorcon/rcon"
)

// RCON runs commands on the server. Every call opens its own connection and
// closes it afterwards: commands are rare, and this way a server restart
// never leaves a stale connection behind.
type RCON struct {
	addr     string
	password string
	timeout  time.Duration
}

func NewRCON(host string, port int, password string) *RCON {
	return &RCON{
		addr:     net.JoinHostPort(host, strconv.Itoa(port)),
		password: password,
		timeout:  DefaultTimeout,
	}
}

// ListPlayers returns the names of the players currently online.
func (r *RCON) ListPlayers(ctx context.Context) ([]string, error) {
	out, err := r.execute(ctx, "list")
	if err != nil {
		return nil, err
	}
	return parsePlayerList(out), nil
}

// Say broadcasts a message to every player in the game chat.
func (r *RCON) Say(ctx context.Context, message string) error {
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return errors.New("rcon say: empty message")
	}
	_, err := r.execute(ctx, "say "+message)
	return err
}

// Command runs any console command, with or without the leading slash, and
// returns the server's answer.
func (r *RCON) Command(ctx context.Context, command string) (string, error) {
	command = strings.TrimPrefix(strings.Join(strings.Fields(command), " "), "/")
	if command == "" {
		return "", errors.New("rcon: empty command")
	}
	out, err := r.execute(ctx, command)
	return formattingCode.ReplaceAllString(out, ""), err
}

// Stop asks the server to save the worlds and shut down.
func (r *RCON) Stop(ctx context.Context) error {
	_, err := r.execute(ctx, "stop")
	if err != nil && closedByPeer(err) {
		// The server may close the connection before it answers; whether
		// it actually stopped is checked by the caller.
		return nil
	}
	return err
}

func (r *RCON) execute(ctx context.Context, command string) (string, error) {
	var dialer net.Dialer
	tcpConn, err := dialer.DialContext(ctx, "tcp", r.addr)
	if err != nil {
		return "", fmt.Errorf("rcon connect: %w", err)
	}
	// Abort any blocking read or write as soon as the context ends.
	stop := context.AfterFunc(ctx, func() { _ = tcpConn.Close() })
	defer stop()

	conn, err := rcon.Open(tcpConn, r.password, rcon.SetDeadline(timeoutFor(ctx, r.timeout)))
	if err != nil {
		_ = tcpConn.Close()
		if errors.Is(err, rcon.ErrAuthFailed) {
			return "", errors.New("rcon: authentication failed")
		}
		return "", fmt.Errorf("rcon open: %w", ctxErr(ctx, err))
	}
	defer conn.Close()

	out, err := conn.Execute(command)
	if err != nil {
		return "", fmt.Errorf("rcon %s: %w", commandName(command), ctxErr(ctx, err))
	}
	return out, nil
}

// ctxErr prefers the context error when the context caused the failure.
func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// commandName keeps only the command keyword, so arguments such as broadcast
// messages are not repeated in error messages.
func commandName(command string) string {
	name, _, _ := strings.Cut(command, " ")
	return name
}

func closedByPeer(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, net.ErrClosed)
}

var formattingCode = regexp.MustCompile(`§.`)

// parsePlayerList extracts player names from the output of the list command,
// for example "There are 2 of a max of 20 players online: Steve, Alex".
// Older servers use "There are 2/20 players online:" followed by the names.
func parsePlayerList(out string) []string {
	out = formattingCode.ReplaceAllString(out, "")
	_, names, found := strings.Cut(out, ":")
	if !found {
		return []string{}
	}
	players := []string{}
	for _, name := range strings.FieldsFunc(names, func(r rune) bool {
		return r == ',' || r == '\n'
	}) {
		if name = strings.TrimSpace(name); name != "" {
			players = append(players, name)
		}
	}
	return players
}
