package minecraft

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// fakeStatusServer answers the Server List Ping handshake with a fixed JSON
// status. If statusJSON is empty it accepts connections and never answers.
func fakeStatusServer(t *testing.T, statusJSON string) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveStatus(t, conn, statusJSON)
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func serveStatus(t *testing.T, conn net.Conn, statusJSON string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)

	// Handshake, then status request.
	for range 2 {
		if _, err := readPacket(r); err != nil {
			return
		}
	}
	if statusJSON == "" {
		_, _ = io.Copy(io.Discard, r) // never answer
		return
	}

	var body bytes.Buffer
	body.WriteByte(0x00) // status response packet ID
	writeVarInt(&body, len(statusJSON))
	body.WriteString(statusJSON)

	var packet bytes.Buffer
	writeVarInt(&packet, body.Len())
	packet.Write(body.Bytes())
	if _, err := conn.Write(packet.Bytes()); err != nil {
		t.Errorf("write status: %v", err)
	}
}

func readPacket(r *bufio.Reader) ([]byte, error) {
	length, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	data := make([]byte, length)
	_, err = io.ReadFull(r, data)
	return data, err
}

// writeVarInt encodes a non-negative protocol VarInt, which has the same
// layout as an unsigned LEB128 varint.
func writeVarInt(w *bytes.Buffer, v int) {
	w.Write(binary.AppendUvarint(nil, uint64(v)))
}

func TestPingReturnsStatus(t *testing.T) {
	host, port := fakeStatusServer(t, `{
		"version": {"name": "1.21.10", "protocol": 773},
		"players": {"max": 20, "online": 3, "sample": [{"name": "Steve", "id": "4566e69f-c907-48ee-8d71-d7ba5aa00d20"}]},
		"description": {"text": "A Minecraft Server"}
	}`)

	st, err := NewPinger(host, port).Ping(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Status{OnlinePlayers: 3, MaxPlayers: 20, Version: "1.21.10"}
	if st != want {
		t.Errorf("status = %+v, want %+v", st, want)
	}
}

func TestPingStripsFormattingFromVersion(t *testing.T) {
	host, port := fakeStatusServer(t, `{
		"version": {"name": "§aPaper 1.21.10", "protocol": 773},
		"players": {"max": 10, "online": 0},
		"description": "hi"
	}`)

	st, err := NewPinger(host, port).Ping(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Version != "Paper 1.21.10" || st.OnlinePlayers != 0 || st.MaxPlayers != 10 {
		t.Errorf("status = %+v", st)
	}
}

func TestPingOfflineServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // nothing listens on this port any more

	if _, err := NewPinger("127.0.0.1", port).Ping(context.Background()); err == nil {
		t.Fatal("expected an error for an offline server")
	}
}

func TestPingUnresponsiveServerRespectsContext(t *testing.T) {
	host, port := fakeStatusServer(t, "")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := NewPinger(host, port).Ping(ctx); err == nil {
		t.Fatal("expected an error for an unresponsive server")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("ping took %s, should stop at the context deadline", elapsed)
	}
}

func TestTimeoutFor(t *testing.T) {
	if got := timeoutFor(context.Background(), time.Second); got != time.Second {
		t.Errorf("no deadline: got %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got := timeoutFor(ctx, time.Second); got > 100*time.Millisecond {
		t.Errorf("earlier deadline: got %s", got)
	}
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if got := timeoutFor(expired, time.Second); got <= 0 {
		t.Errorf("expired deadline must stay positive, got %s", got)
	}
}
