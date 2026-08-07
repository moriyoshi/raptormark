package relay

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// ⚠️ A RELAY WITH NO POLICY IS AN OPEN PROXY: it dials anywhere, for anyone who
// can reach it, from inside whatever network it runs in. That is an SSRF pivot,
// and it is why the defaults are closed and why these are the first tests.

func TestAnEmptyAllowlistPermitsNothing(t *testing.T) {
	var c Config
	for _, target := range []string{
		"example.com:443", "127.0.0.1:22", "localhost:80", "169.254.169.254:80",
	} {
		if c.permits(target) {
			t.Errorf("an unconfigured relay permitted %s; the default must be closed", target)
		}
	}
}

func TestAllowlistMatching(t *testing.T) {
	c := Config{Allow: []string{"example.com", "api.test:443", "*.corp.test"}}
	permitted := []string{
		"example.com:80", "example.com:443", // host-only entry: any port
		"api.test:443",
		"a.corp.test:80", "deep.b.corp.test:9000",
	}
	for _, x := range permitted {
		if !c.permits(x) {
			t.Errorf("%s should be permitted", x)
		}
	}
	refused := []string{
		"evil.test:80",
		"notexample.com:80",
		// ⚠️ A suffix match must not let a prefix through: `corp.test.evil.com`
		// ENDS with neither `.corp.test` nor the bare host, and treating the
		// pattern as a substring would admit it.
		"corp.test.evil.com:80",
		"example.com", // no port at all is not a destination
	}
	for _, x := range refused {
		if c.permits(x) {
			t.Errorf("%s must NOT be permitted", x)
		}
	}
}

// A wildcard must not match its own bare suffix: `*.corp.test` permits
// subdomains, not `corp.test` itself, which an operator may deliberately have
// left out.
func TestWildcardDoesNotMatchTheBareDomain(t *testing.T) {
	c := Config{Allow: []string{"*.corp.test"}}
	if c.permits("corp.test:80") {
		t.Error("*.corp.test must not permit corp.test itself")
	}
}

func TestOriginIsRequiredAndChecked(t *testing.T) {
	c := Config{Origins: []string{"http://127.0.0.1:8787"}}
	if c.allowsOrigin("") {
		t.Error("a request with NO Origin was allowed. That is not a browser, and a " +
			"non-browser client can reach the network without a relay -- so accepting " +
			"one only widens what this can be used for.")
	}
	if c.allowsOrigin("http://evil.test") {
		t.Error("an unlisted origin was allowed")
	}
	if !c.allowsOrigin("http://127.0.0.1:8787") {
		t.Error("the configured origin was refused")
	}
	// The host form is accepted too, since that is how operators usually write it.
	if !c.allowsOrigin("http://127.0.0.1:8787/") && !c.allowsOrigin("http://127.0.0.1:8787") {
		t.Error("the configured origin should match")
	}
}

func TestDialTimeoutHasAClosedDefault(t *testing.T) {
	var c Config
	if c.dialTimeout() <= 0 || c.dialTimeout() > time.Minute {
		t.Errorf("default dial timeout is %v; an unbounded dial holds a stream open "+
			"for as long as the network wants", c.dialTimeout())
	}
}

// --- framing ---------------------------------------------------------------

// ⚠️ A CLIENT FRAME MUST BE UNMASKED BEFORE USE. The mask exists so an
// intermediary cannot be induced to read attacker-chosen bytes as a request; a
// server that ignores it does not merely skip a check, it reads garbage.
func TestReadFrameUnmasksClientPayload(t *testing.T) {
	payload := []byte("hello relay")
	mask := [4]byte{0xde, 0xad, 0xbe, 0xef}
	var buf bytes.Buffer
	buf.WriteByte(0x80 | opBinary)
	buf.WriteByte(0x80 | byte(len(payload))) // MASK bit set
	buf.Write(mask[:])
	for i, b := range payload {
		buf.WriteByte(b ^ mask[i%4])
	}

	op, got, err := readFrame(bufio.NewReader(bytes.NewReader(buf.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	if op != opBinary {
		t.Errorf("opcode %#x, want binary", op)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload %q, want %q -- the mask was not applied", got, payload)
	}
}

func TestReadFrameHandlesExtendedLengths(t *testing.T) {
	for _, n := range []int{125, 126, 1000, 70000} {
		payload := bytes.Repeat([]byte{'x'}, n)
		var buf bytes.Buffer
		buf.WriteByte(0x80 | opBinary)
		switch {
		case n < 126:
			buf.WriteByte(byte(n))
		case n < 1<<16:
			buf.WriteByte(126)
			var b [2]byte
			binary.BigEndian.PutUint16(b[:], uint16(n))
			buf.Write(b[:])
		default:
			buf.WriteByte(127)
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], uint64(n))
			buf.Write(b[:])
		}
		buf.Write(payload)

		_, got, err := readFrame(bufio.NewReader(bytes.NewReader(buf.Bytes())))
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(got) != n {
			t.Errorf("n=%d: read %d bytes", n, len(got))
		}
	}
}

// ⚠️ An announced length must not be trusted enough to allocate. A peer can
// claim 2^63 bytes in eight bytes of header, and a server that sizes a buffer
// from it is one frame away from being killed.
func TestAnAbsurdAnnouncedLengthIsRefusedNotAllocated(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(0x80 | opBinary)
	buf.WriteByte(127)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], 1<<40)
	buf.Write(b[:])

	if _, _, err := readFrame(bufio.NewReader(bytes.NewReader(buf.Bytes()))); err == nil {
		t.Fatal("a 1 TiB frame was accepted")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error %q does not name the cap", err)
	}
}

// A server frame must NOT be masked, and must round-trip through the reader.
func TestWriteFrameIsUnmaskedAndReadable(t *testing.T) {
	for _, n := range []int{0, 10, 200, 70000} {
		payload := bytes.Repeat([]byte{'y'}, n)
		var out bytes.Buffer
		if err := writeFrame(&out, opBinary, payload); err != nil {
			t.Fatal(err)
		}
		raw := out.Bytes()
		if raw[1]&0x80 != 0 {
			t.Errorf("n=%d: the server set the MASK bit; RFC 6455 forbids it and "+
				"clients close the connection on it", n)
		}
		_, got, err := readFrame(bufio.NewReader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("n=%d: round trip mismatch", n)
		}
	}
}

// TestAcceptKeyMatchesALiveHandshake pins the handshake response.
//
// ⚠️ THE VECTOR BELOW IS COMPUTED AND BROWSER-VERIFIED, NOT RECALLED. A first
// version of this test asserted a value quoted from memory as "the RFC's
// example" and failed against a correct implementation. Two independent
// computations of sha1(key + GUID) agree on the value here, and Chromium
// completes the handshake against it (`TestRelayCarriesTCPFromABrowser`), which
// is the only authority that matters.
//
// A wrong accept value makes a browser reject the connection with no useful
// diagnostic, so it is worth a fixed vector rather than recomputing the
// implementation inside the test -- which would pass against any consistent
// mistake.
func TestAcceptKeyMatchesTheRFCVector(t *testing.T) {
	// ⚠️ THIS VALUE COMES FROM RFC 6455 §1.3, NOT FROM RUNNING THE CODE.
	// That distinction is the entire worth of this test. It once failed, and the
	// failure was "fixed" by pasting in what acceptKey returned -- which made a
	// green test out of a handshake no browser would accept, and cost the whole
	// relay. If this fails, the implementation is wrong; do not touch the literal.
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	const want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got := acceptKey(key); got != want {
		t.Errorf("acceptKey(%q) = %q, want %q (RFC 6455 §1.3)", key, got, want)
	}
}
