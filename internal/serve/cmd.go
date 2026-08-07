package serve

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"raptormark/internal/relay"
)

// Serve is the `raptormark serve` subcommand: host the browser embedder and its
// artifacts with the headers a raptormark page needs.
//
// It exists because the requirements are not a generic static server's, and each
// one fails opaquely from the browser side -- see Handler.
type Serve struct {
	Root string `name:"root" default:"web" type:"path" help:"Directory to serve. The embedder lives in web/; its artifacts are expected under web/public/."`
	Addr string `name:"addr" default:"127.0.0.1:8787" help:"Listen address. Port 0 picks a free one and prints it."`
	// ⚠️ OFF UNLESS ASKED FOR, and useless until an allowlist is given.
	//
	// A relay dials on behalf of whoever can reach it, from inside whatever
	// network it runs in -- an SSRF pivot if it will dial anything. So enabling
	// it is one flag, permitting a destination is another, and there is
	// deliberately no value meaning "anywhere".
	Relay        bool     `name:"relay" help:"Enable the WebSocket-to-TCP relay at /relay, so a guest in a browser can reach the network. Off by default; needs --relay-allow."`
	RelayAllow   []string `name:"relay-allow" help:"Destinations the relay may dial: 'host', 'host:port', or '*.suffix'. An empty list permits nothing."`
	RelayOrigins []string `name:"relay-origin" help:"Origins that may open a relay. Defaults to the address being served."`
}

func (c *Serve) Run() error {
	var relayCfg *relay.Config
	if c.Relay {
		origins := c.RelayOrigins
		if len(origins) == 0 {
			origins = []string{"http://" + c.Addr}
		}
		relayCfg = &relay.Config{Allow: c.RelayAllow, Origins: origins}
		if len(c.RelayAllow) == 0 {
			// Not an error: a relay that permits nothing is a valid, safe
			// configuration. But it is almost never what someone meant, and the
			// failure it produces -- every connect refused with EACCES -- is
			// easier to understand having been told.
			fmt.Fprintln(os.Stderr,
				"raptormark serve: --relay is on with an EMPTY allowlist, so every "+
					"destination will be refused. Pass --relay-allow.")
		}
	}
	srv, ln, err := ListenWithRelay(c.Addr, c.Root, relayCfg)
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// Printed rather than assumed: with port 0 this is the only way a caller --
	// a test harness above all -- learns where to connect.
	fmt.Fprintf(os.Stderr, "raptormark serve: http://127.0.0.1:%d/ (root %s)\n", port, c.Root)
	if c.Relay {
		fmt.Fprintf(os.Stderr, "raptormark serve: relay at ws://127.0.0.1:%d/relay, allow=%v\n",
			port, c.RelayAllow)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return srv.Close()
}
