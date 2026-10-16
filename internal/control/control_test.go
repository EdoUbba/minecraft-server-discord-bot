package control

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// reply is what the fake minecraft-ctl answers to a command.
type reply struct {
	stdout, stderr string
	exit           uint32
}

type fakeHost struct {
	host     string
	port     int
	keyPath  string
	hostKey  ssh.PublicKey // the ed25519 host key; the server also offers an ECDSA one
	commands func() []string
}

// fakeCtl starts an SSH server that accepts only the bot's key, answers every
// command with handler and records the commands it receives.
func fakeCtl(t *testing.T, handler func(command string) reply) *fakeHost {
	t.Helper()

	clientPub, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	authorized, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatal(err)
	}

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return nil, errors.New("unknown key")
			}
			return nil, nil
		},
	}
	edSigner := newSigner(t, ed25519Key(t))
	config.AddHostKey(edSigner)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config.AddHostKey(newSigner(t, ecKey))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var (
		mu       sync.Mutex
		commands []string
	)
	serve := func(conn net.Conn) {
		defer conn.Close()
		_, chans, reqs, err := ssh.NewServerConn(conn, config)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for newCh := range chans {
			if newCh.ChannelType() != "session" {
				_ = newCh.Reject(ssh.UnknownChannelType, "")
				continue
			}
			ch, chReqs, err := newCh.Accept()
			if err != nil {
				return
			}
			go func() {
				defer ch.Close()
				for req := range chReqs {
					var exec struct{ Command string }
					if req.Type != "exec" || ssh.Unmarshal(req.Payload, &exec) != nil {
						_ = req.Reply(false, nil)
						continue
					}
					_ = req.Reply(true, nil)
					mu.Lock()
					commands = append(commands, exec.Command)
					mu.Unlock()
					r := handler(exec.Command)
					_, _ = io.WriteString(ch, r.stdout)
					_, _ = io.WriteString(ch.Stderr(), r.stderr)
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{r.exit}))
					return
				}
			}()
		}
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return &fakeHost{
		host:    addr.IP.String(),
		port:    addr.Port,
		keyPath: keyPath,
		hostKey: edSigner.PublicKey(),
		commands: func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(commands)
		},
	}
}

func ed25519Key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newSigner(t *testing.T, key any) ssh.Signer {
	t.Helper()
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// knownHosts writes a known_hosts file that lists keys for the fake host.
func (f *fakeHost) knownHosts(t *testing.T, keys ...ssh.PublicKey) string {
	t.Helper()
	addr := knownhosts.Normalize(net.JoinHostPort(f.host, strconv.Itoa(f.port)))
	var lines []string
	for _, key := range keys {
		lines = append(lines, knownhosts.Line([]string{addr}, key))
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// client returns a Client that trusts the fake host's ed25519 key only.
func (f *fakeHost) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(f.host, f.port, "mcbot", f.keyPath, f.knownHosts(t, f.hostKey))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var listReply = reply{stdout: "survival\ncreative\n"}

func TestCommandsAreSentVerbatim(t *testing.T) {
	host := fakeCtl(t, func(command string) reply {
		if command == "status" {
			return reply{stdout: "ActiveState=inactive\n"}
		}
		return reply{}
	})
	c := host.client(t)
	ctx := context.Background()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := c.Status(ctx); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := c.List(ctx); err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"start", "stop", "status", "list"}
	if got := host.commands(); !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

func TestStatus(t *testing.T) {
	host := fakeCtl(t, func(string) reply {
		return reply{stdout: "ActiveState=active\nActiveEnterTimestamp=@1760630400\nServer=survival\n"}
	})
	st, err := host.client(t).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Status{ActiveState: "active", Since: time.Unix(1760630400, 0), Server: "survival"}
	if st.ActiveState != want.ActiveState || !st.Since.Equal(want.Since) || st.Server != want.Server {
		t.Errorf("status = %+v, want %+v", st, want)
	}
}

func TestParseStatus(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    Status
		wantErr string
	}{
		{"inactive, never started", "ActiveState=inactive\nActiveEnterTimestamp=\nServer=creative\n",
			Status{ActiveState: "inactive", Server: "creative"}, ""},
		{"zero timestamp", "ActiveState=inactive\nActiveEnterTimestamp=@0\n",
			Status{ActiveState: "inactive"}, ""},
		{"unknown keys and noise ignored", "\n  ActiveState=active  \nSubState=running\ngarbage\n",
			Status{ActiveState: "active"}, ""},
		{"missing ActiveState", "Server=survival\n", Status{}, "missing ActiveState"},
		{"human timestamp", "ActiveState=active\nActiveEnterTimestamp=Thu 2026-10-16 19:47:00 CEST\n",
			Status{}, "invalid ActiveEnterTimestamp"},
		{"timestamp without @", "ActiveState=active\nActiveEnterTimestamp=1760630400\n",
			Status{}, "invalid ActiveEnterTimestamp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStatus(tt.out)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("status = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestList(t *testing.T) {
	host := fakeCtl(t, func(string) reply { return reply{stdout: "survival\n\n  creative \n"} })
	names, err := host.client(t).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"survival", "creative"}; !slices.Equal(names, want) {
		t.Errorf("list = %q, want %q", names, want)
	}
}

func TestListEmpty(t *testing.T) {
	host := fakeCtl(t, func(string) reply { return reply{} })
	names, err := host.client(t).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if names == nil || len(names) != 0 {
		t.Errorf("list = %#v, want empty slice", names)
	}
}

func TestSelect(t *testing.T) {
	host := fakeCtl(t, func(command string) reply {
		if command == "list" {
			return listReply
		}
		return reply{}
	})
	if err := host.client(t).Select(context.Background(), "creative"); err != nil {
		t.Fatal(err)
	}
	if got, want := host.commands(), []string{"list", "select creative"}; !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

func TestSelectRejectsInvalidNamesWithoutConnecting(t *testing.T) {
	host := fakeCtl(t, func(string) reply { return listReply })
	c := host.client(t)
	for _, name := range []string{"", ".", "..", "../etc", "survival/../..", "/opt/mc", `a\b`,
		"-f", "two words", "tab\tname", "new\nline"} {
		err := c.Select(context.Background(), name)
		if err == nil || !strings.Contains(err.Error(), "invalid server name") {
			t.Errorf("select %q: error = %v, want invalid server name", name, err)
		}
	}
	if got := host.commands(); len(got) != 0 {
		t.Errorf("commands = %q, want none", got)
	}
}

func TestSelectRejectsUnknownName(t *testing.T) {
	host := fakeCtl(t, func(string) reply { return listReply })
	err := host.client(t).Select(context.Background(), "hardcore")
	if err == nil || !strings.Contains(err.Error(), "unknown server") {
		t.Fatalf("error = %v, want unknown server", err)
	}
	if got, want := host.commands(), []string{"list"}; !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

func TestRemoteErrorUsesFirstStderrLine(t *testing.T) {
	host := fakeCtl(t, func(command string) reply {
		if command == "list" {
			return listReply
		}
		return reply{stderr: "service is active\nmore details\n", exit: 1}
	})
	err := host.client(t).Select(context.Background(), "survival")
	if err == nil || err.Error() != "ssh select survival: service is active" {
		t.Fatalf("error = %v", err)
	}
}

func TestRemoteErrorWithoutStderr(t *testing.T) {
	host := fakeCtl(t, func(string) reply { return reply{exit: 3} })
	err := host.client(t).Start(context.Background())
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 3 || !strings.HasPrefix(err.Error(), "ssh start: ") {
		t.Fatalf("error = %v, want ssh start exit status 3", err)
	}
}

func TestHostKeyMismatchIsRejected(t *testing.T) {
	host := fakeCtl(t, func(string) reply { return reply{} })
	other := newSigner(t, ed25519Key(t)).PublicKey()
	c, err := New(host.host, host.port, "mcbot", host.keyPath, host.knownHosts(t, other))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Start(context.Background())
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		t.Fatalf("error = %v, want host key mismatch", err)
	}
	if got := host.commands(); len(got) != 0 {
		t.Errorf("commands = %q, want none", got)
	}
}

func TestUnknownHostIsRejected(t *testing.T) {
	host := fakeCtl(t, func(string) reply { return reply{} })
	stranger := &fakeHost{host: "192.0.2.1", port: 22}
	_, err := New(host.host, host.port, "mcbot", host.keyPath, stranger.knownHosts(t, host.hostKey))
	if err == nil || !strings.Contains(err.Error(), "no key for") {
		t.Fatalf("error = %v, want no key for host", err)
	}
}

// The server also offers an ECDSA host key, which the client would pick by
// default; known_hosts lists only the ed25519 one.
func TestKnownHostsWithOneKeyTypeConnects(t *testing.T) {
	host := fakeCtl(t, func(string) reply { return reply{} })
	if err := host.client(t).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestKeyErrorsHideKey(t *testing.T) {
	host := fakeCtl(t, func(string) reply { return reply{} })
	knownHosts := host.knownHosts(t, host.hostKey)

	missing := filepath.Join(t.TempDir(), "secret-location", "id_ed25519")
	_, err := New(host.host, host.port, "mcbot", missing, knownHosts)
	if err == nil || strings.Contains(err.Error(), "secret-location") {
		t.Errorf("missing key: error = %v, want one without the path", err)
	}

	garbage := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(garbage, []byte("not-a-key-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = New(host.host, host.port, "mcbot", garbage, knownHosts)
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), garbage) {
		t.Errorf("bad key: error = %v, want one without content or path", err)
	}
}

func TestContextCancelsSlowCommand(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	host := fakeCtl(t, func(string) reply {
		<-release
		return reply{}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := host.client(t).Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("stop returned after %s", elapsed)
	}
}
