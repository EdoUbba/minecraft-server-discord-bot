package minecraft

import (
	"context"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorcon/rcon"
)

const testPassword = "test-password"

// closeConn is a handler reply that makes the fake server drop the
// connection instead of answering.
const closeConn = "\x00close"

// fakeRCON starts an RCON server that authenticates with testPassword,
// answers every command with handler and records the commands it receives.
func fakeRCON(t *testing.T, handler func(command string) string) (*RCON, func() []string) {
	t.Helper()
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
		for {
			var req rcon.Packet
			if _, err := req.ReadFrom(conn); err != nil {
				return
			}
			switch req.Type {
			case rcon.SERVERDATA_AUTH:
				if req.Body() != testPassword {
					_, _ = rcon.NewPacket(rcon.SERVERDATA_AUTH_RESPONSE, -1, "").WriteTo(conn)
					continue
				}
				_, _ = rcon.NewPacket(rcon.SERVERDATA_RESPONSE_VALUE, req.ID, "").WriteTo(conn)
				_, _ = rcon.NewPacket(rcon.SERVERDATA_AUTH_RESPONSE, req.ID, "").WriteTo(conn)
			case rcon.SERVERDATA_EXECCOMMAND:
				mu.Lock()
				commands = append(commands, req.Body())
				mu.Unlock()
				out := handler(req.Body())
				if out == closeConn {
					return
				}
				_, _ = rcon.NewPacket(rcon.SERVERDATA_RESPONSE_VALUE, req.ID, out).WriteTo(conn)
			}
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
	received := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(commands)
	}
	return NewRCON(addr.IP.String(), addr.Port, testPassword), received
}

func answer(out string) func(string) string {
	return func(string) string { return out }
}

func TestListPlayers(t *testing.T) {
	client, received := fakeRCON(t, answer("There are 2 of a max of 20 players online: Steve, Alex"))

	players, err := client.ListPlayers(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(players, []string{"Steve", "Alex"}) {
		t.Errorf("players = %q", players)
	}
	if got := received(); !slices.Equal(got, []string{"list"}) {
		t.Errorf("commands = %q, want [list]", got)
	}
}

func TestSay(t *testing.T) {
	client, received := fakeRCON(t, answer(""))

	if err := client.Say(context.Background(), "Server stopping\nin 10 seconds  "); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := received(); !slices.Equal(got, []string{"say Server stopping in 10 seconds"}) {
		t.Errorf("commands = %q", got)
	}
	if err := client.Say(context.Background(), " \n "); err == nil {
		t.Error("expected an error for an empty message")
	}
}

func TestStop(t *testing.T) {
	client, received := fakeRCON(t, answer("Stopping the server"))

	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := received(); !slices.Equal(got, []string{"stop"}) {
		t.Errorf("commands = %q, want [stop]", got)
	}
}

func TestStopToleratesConnectionClosedByServer(t *testing.T) {
	client, _ := fakeRCON(t, answer(closeConn))

	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("stop should succeed when the server closes the connection: %v", err)
	}
}

func TestListFailsWhenConnectionClosed(t *testing.T) {
	client, _ := fakeRCON(t, answer(closeConn))

	if _, err := client.ListPlayers(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestWrongPassword(t *testing.T) {
	client, received := fakeRCON(t, answer(""))
	client.password = "wrong-password"

	_, err := client.ListPlayers(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("error = %v, want authentication failure", err)
	}
	if strings.Contains(err.Error(), "wrong-password") {
		t.Errorf("error leaks the password: %v", err)
	}
	if got := received(); len(got) != 0 {
		t.Errorf("no command should run without authentication, got %q", got)
	}
}

func TestOfflineServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if _, err := NewRCON("127.0.0.1", port, testPassword).ListPlayers(context.Background()); err == nil {
		t.Fatal("expected an error for an offline server")
	}
}

func TestContextCancelsSlowCommand(t *testing.T) {
	client, _ := fakeRCON(t, func(string) string {
		time.Sleep(3 * time.Second)
		return ""
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.ListPlayers(ctx)
	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("command took %s, should stop at the context deadline", elapsed)
	}
}

func TestErrorsDoNotRepeatSayMessage(t *testing.T) {
	client, _ := fakeRCON(t, answer(closeConn))

	err := client.Say(context.Background(), "secret plans")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret plans") {
		t.Errorf("error repeats the message: %v", err)
	}
}

func TestParsePlayerList(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"modern", "There are 3 of a max of 20 players online: Steve, Alex, Notch", []string{"Steve", "Alex", "Notch"}},
		{"modern empty", "There are 0 of a max of 20 players online: ", []string{}},
		{"legacy", "There are 2/20 players online:\nSteve, Alex", []string{"Steve", "Alex"}},
		{"legacy empty", "There are 0/20 players online:", []string{}},
		{"formatting codes", "§6There are §c1§6 of a max of §c20§6 players online: §fSteve", []string{"Steve"}},
		{"no colon", "Unknown command", []string{}},
		{"empty", "", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parsePlayerList(tt.out); !slices.Equal(got, tt.want) {
				t.Errorf("parsePlayerList(%q) = %q, want %q", tt.out, got, tt.want)
			}
		})
	}
}
