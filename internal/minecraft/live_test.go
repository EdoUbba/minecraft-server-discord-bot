package minecraft

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLiveServer checks ping and RCON against a real server. It only runs
// when MC_TEST_HOST is set, for example:
//
//	MC_TEST_HOST=192.168.1.50 MC_TEST_RCON_PASSWORD=... go test ./internal/minecraft -run TestLiveServer -v
//
// MC_TEST_PORT and MC_TEST_RCON_PORT default to 25565 and 25575. Without
// MC_TEST_RCON_PASSWORD only the ping is checked. It never stops the server.
func TestLiveServer(t *testing.T) {
	host := os.Getenv("MC_TEST_HOST")
	if host == "" {
		t.Skip("MC_TEST_HOST not set")
	}
	port := envInt(t, "MC_TEST_PORT", 25565)
	rconPort := envInt(t, "MC_TEST_RCON_PORT", 25575)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := NewPinger(host, port).Ping(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ping: version %q, %d/%d players online", st.Version, st.OnlinePlayers, st.MaxPlayers)

	password := os.Getenv("MC_TEST_RCON_PASSWORD")
	if password == "" {
		t.Log("MC_TEST_RCON_PASSWORD not set, skipping RCON")
		return
	}
	players, err := NewRCON(host, rconPort, password).ListPlayers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rcon list: %d players %q", len(players), players)
	if len(players) != st.OnlinePlayers {
		t.Logf("note: ping and list counts differ (%d vs %d); players may have joined or left in between",
			st.OnlinePlayers, len(players))
	}
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return n
}
