// Package minecraft talks to the Minecraft server over the network: Server
// List Ping for the status and RCON for player lists, broadcasts and stop.
package minecraft

import (
	"context"
	"fmt"
	"time"

	"github.com/mcstatus-io/mcutil/v4/options"
	"github.com/mcstatus-io/mcutil/v4/status"
)

// DefaultTimeout bounds a single ping or RCON exchange when the context has
// no earlier deadline.
const DefaultTimeout = 5 * time.Second

// Status is the result of a successful Server List Ping.
type Status struct {
	OnlinePlayers int
	MaxPlayers    int
	Version       string
}

// Pinger queries a Java Edition server with the Server List Ping protocol.
type Pinger struct {
	host    string
	port    uint16
	timeout time.Duration
}

func NewPinger(host string, port int) *Pinger {
	return &Pinger{host: host, port: uint16(port), timeout: DefaultTimeout}
}

// Ping returns the current server status. Any error means the server did not
// answer, either because it is offline or because it is not responding.
func (p *Pinger) Ping(ctx context.Context) (Status, error) {
	resp, err := status.Modern(ctx, p.host, p.port, options.StatusModern{
		EnableSRV:         false,
		Timeout:           timeoutFor(ctx, p.timeout),
		ProtocolVersion:   -1,
		ReceiveLimitBytes: 1 << 20,
		Ping:              false,
	})
	if err != nil {
		return Status{}, fmt.Errorf("ping: %w", err)
	}

	st := Status{Version: resp.Version.Name.Clean}
	if resp.Players.Online != nil {
		st.OnlinePlayers = int(*resp.Players.Online)
	}
	if resp.Players.Max != nil {
		st.MaxPlayers = int(*resp.Players.Max)
	}
	return st, nil
}

// timeoutFor returns def, shortened to the context deadline if that is sooner.
func timeoutFor(ctx context.Context, def time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < def {
			return max(left, time.Millisecond)
		}
	}
	return def
}
