package control

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLiveHost checks the client against a real minecraft-ctl. It only runs
// when MC_TEST_SSH_HOST is set, for example:
//
//	MC_TEST_SSH_HOST=192.168.1.50 MC_TEST_SSH_KEY=$PWD/secrets/id_ed25519 \
//	MC_TEST_SSH_KNOWN_HOSTS=$PWD/secrets/known_hosts go test ./internal/control -run TestLiveHost -v
//
// The paths must be absolute: go test runs inside the package folder.
// MC_TEST_SSH_PORT and MC_TEST_SSH_USER default to 22 and mcbot. It never
// starts or stops the server; when the service is stopped it re-selects the
// active server, which changes nothing.
func TestLiveHost(t *testing.T) {
	host := os.Getenv("MC_TEST_SSH_HOST")
	if host == "" {
		t.Skip("MC_TEST_SSH_HOST not set")
	}
	port := 22
	if v := os.Getenv("MC_TEST_SSH_PORT"); v != "" {
		var err error
		if port, err = strconv.Atoi(v); err != nil {
			t.Fatalf("MC_TEST_SSH_PORT: %v", err)
		}
	}
	user := os.Getenv("MC_TEST_SSH_USER")
	if user == "" {
		user = "mcbot"
	}
	c, err := New(host, port, user, os.Getenv("MC_TEST_SSH_KEY"), os.Getenv("MC_TEST_SSH_KNOWN_HOSTS"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("status: %+v", st)

	names, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("list: %q", names)

	if err := c.Select(ctx, "no-such-server-"+strconv.FormatInt(time.Now().Unix(), 10)); err == nil ||
		!strings.Contains(err.Error(), "unknown server") {
		t.Errorf("select of a missing server: error = %v, want unknown server", err)
	}

	if st.ActiveState != "inactive" && st.ActiveState != "failed" {
		t.Logf("service is %s, skipping select", st.ActiveState)
		return
	}
	if !slices.Contains(names, st.Server) {
		t.Logf("active server %q not in list, skipping select", st.Server)
		return
	}
	if err := c.Select(ctx, st.Server); err != nil {
		t.Fatalf("select %s: %v", st.Server, err)
	}
	t.Logf("select %s: ok", st.Server)
}
