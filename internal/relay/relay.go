package relay

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The wire protocol, mirrored by `web/src/browser/relay.ts`.
//
// One byte of opcode, then a u16 stream id, then opcode-specific bytes. One
// WebSocket carries every stream.
//
// ⚠️ ONE SOCKET, MANY STREAMS, on purpose. A socket per connection would be
// simpler and wrong: browsers cap concurrent sockets, each costs a handshake,
// and inbound reachability and UDP would each need a scheme of their own later.
// Designing the multiplexing once is what lets those be added as opcodes.
//
// ⚠️ These are a DIFFERENT opcode space from the WebSocket frame opcodes in
// ws.go, and they overlap numerically. `msg*` here rides inside an `op*` binary
// frame there; conflating the two is how a close on one layer gets mistaken for
// a close on the other.
const (
	msgOpen     = 1 // body: "host:port"
	msgOpenOK   = 2
	msgOpenErr  = 3 // body[0]: WASI errno
	msgData     = 4
	msgShutdown = 5 // body[0]: bitflags, 1 = read, 2 = write
	msgClose    = 6
)

const hdr = 3

// WASI errnos the client distinguishes. Anything else it treats as a hard
// failure, which is correct -- these are the ones a guest acts on differently.
const (
	errnoConnRefused = 14
	errnoHostUnreach = 23
	errnoTimedOut    = 73
	errnoAccess      = 2
)

// Config is the relay's security posture. Every field defaults to the closed
// position.
//
// ⚠️ A RELAY WITH NO POLICY IS AN OPEN PROXY. It will dial anywhere on behalf of
// anyone who can reach it, from inside whatever network it runs on -- which is
// exactly the shape of an SSRF pivot. That is why the relay is off unless asked
// for, why an empty allowlist permits nothing, and why the origin check is not
// optional.
type Config struct {
	// Allow lists permitted destinations as "host" or "host:port". Empty means
	// NOTHING is permitted; there is deliberately no "allow all" value.
	Allow []string
	// Origins that may open a relay. A browser always sends `Origin`, and it is
	// the one field a page cannot forge, so it is the usable check.
	Origins []string
	// DialTimeout bounds a single outbound connect.
	DialTimeout time.Duration
	// Log, if set, receives one line per stream decision.
	//
	// Every refusal a relay makes reaches the guest as a bare errno, and the two
	// that matter most -- "the allowlist does not permit this" and "the dial
	// failed" -- are indistinguishable from the far side by design. An operator
	// debugging a policy has nothing else to go on.
	Log func(format string, args ...any)
}

func (c *Config) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

func (c *Config) dialTimeout() time.Duration {
	if c.DialTimeout <= 0 {
		return 10 * time.Second
	}
	return c.DialTimeout
}

// permits reports whether `hostport` may be dialled.
//
// Matching is on the requested name, not on a resolved address, and that is a
// deliberate limitation rather than an oversight: resolving first and matching
// the address would let DNS decide what the allowlist means, and re-resolving
// before the dial reopens the same hole. An operator listing a name is
// permitting that name.
func (c *Config) permits(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	for _, a := range c.Allow {
		if a == hostport {
			return true
		}
		if !strings.Contains(a, ":") && a == host {
			return true
		}
		if strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) {
			return true
		}
		_ = port
	}
	return false
}

func (c *Config) allowsOrigin(origin string) bool {
	if origin == "" {
		// No Origin means it is not a browser. Refused rather than allowed: the
		// relay exists for pages, and a non-browser client can reach the network
		// without it -- so accepting one only widens what this can be used for.
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	for _, o := range c.Origins {
		if o == origin || o == u.Host {
			return true
		}
	}
	return false
}

// Handler serves the relay endpoint.
func Handler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cfg.allowsOrigin(r.Header.Get("Origin")) {
			// 403 before the upgrade, so a rejected page sees an HTTP status
			// rather than a socket that opens and immediately closes.
			http.Error(w, "relay: origin not permitted", http.StatusForbidden)
			return
		}
		conn, rw, err := upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer conn.Close()
		serve(rw, cfg)
	})
}

type stream struct {
	id   int
	conn net.Conn
}

func serve(rw *bufio.ReadWriter, cfg Config) {
	var mu sync.Mutex // serialises writes: one socket, many stream goroutines
	send := func(op byte, id int, body []byte) {
		mu.Lock()
		defer mu.Unlock()
		f := make([]byte, hdr+len(body))
		f[0] = op
		f[1] = byte(id >> 8)
		f[2] = byte(id)
		copy(f[hdr:], body)
		_ = writeFrame(rw, opBinary, f)
		_ = rw.Flush()
	}

	streams := map[int]*stream{}
	var smu sync.Mutex
	defer func() {
		smu.Lock()
		for _, s := range streams {
			s.conn.Close()
		}
		smu.Unlock()
	}()

	for {
		op, payload, err := readFrame(rw.Reader)
		if err != nil {
			return
		}
		switch op {
		case opClose:
			return
		case opPing:
			mu.Lock()
			_ = writeFrame(rw, opPong, payload)
			_ = rw.Flush()
			mu.Unlock()
			continue
		case opPong, opContinuation, opText:
			continue
		case opBinary:
			// fall through
		default:
			continue
		}
		if len(payload) < hdr {
			continue
		}
		id := int(payload[1])<<8 | int(payload[2])
		body := payload[hdr:]

		switch payload[0] {
		case msgOpen:
			go openStream(cfg, id, string(body), send, streams, &smu)
		case msgData:
			smu.Lock()
			s := streams[id]
			smu.Unlock()
			if s != nil {
				if _, err := s.conn.Write(body); err != nil {
					s.conn.Close()
				}
			}
		case msgShutdown:
			smu.Lock()
			s := streams[id]
			smu.Unlock()
			// Half-close has no WebSocket equivalent, which is why it needs its
			// own opcode: a guest that shuts down its write side and then waits
			// for the peer's response would otherwise hang.
			if s != nil && len(body) > 0 && body[0]&2 != 0 {
				if tc, ok := s.conn.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
			}
		case msgClose:
			smu.Lock()
			if s := streams[id]; s != nil {
				s.conn.Close()
				delete(streams, id)
			}
			smu.Unlock()
		}
	}
}

func openStream(
	cfg Config,
	id int,
	target string,
	send func(byte, int, []byte),
	streams map[int]*stream,
	smu *sync.Mutex,
) {
	if !cfg.permits(target) {
		cfg.logf("relay: stream %d: %q is not permitted by the allowlist %v", id, target, cfg.Allow)
		// ⚠️ Reported as a permission error, NOT as a refusal. A guest cannot
		// distinguish "the relay will not dial this" from "nothing is listening"
		// if both arrive as ECONNREFUSED, and an operator debugging an allowlist
		// needs the difference.
		send(msgOpenErr, id, []byte{errnoAccess})
		return
	}
	c, err := net.DialTimeout("tcp", target, cfg.dialTimeout())
	if err != nil {
		cfg.logf("relay: stream %d: dialling %q: %v", id, target, err)
		// ⚠️ THE ERROR MUST TRAVEL IN BAND. The WebSocket opens fine even when
		// the far-side TCP connect fails, so there is no transport-level signal
		// the client could read it from -- and collapsing refusal, timeout and
		// unreachable into one value destroys distinctions the guest acts on.
		send(msgOpenErr, id, []byte{errnoOf(err)})
		return
	}
	smu.Lock()
	streams[id] = &stream{id: id, conn: c}
	smu.Unlock()
	cfg.logf("relay: stream %d: connected to %q", id, target)
	send(msgOpenOK, id, nil)

	buf := make([]byte, 32*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			send(msgData, id, buf[:n])
		}
		if err != nil {
			// EOF and error both end the stream; the guest learns by reading.
			send(msgClose, id, nil)
			smu.Lock()
			delete(streams, id)
			smu.Unlock()
			c.Close()
			return
		}
	}
}

// errnoOf maps a dial failure to the WASI errno the guest should see.
//
// ⚠️ Do NOT collapse these. `sys_connect` treats the in-progress family as
// resumable and everything else as fatal, and a guest distinguishes "refused"
// from "timed out" when deciding whether to retry. A single value would make
// every failure look the same to code written to tell them apart.
func errnoOf(err error) byte {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errnoTimedOut
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "refused"):
		return errnoConnRefused
	case strings.Contains(s, "no route"), strings.Contains(s, "unreachable"):
		return errnoHostUnreach
	default:
		return errnoConnRefused
	}
}
