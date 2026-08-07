// Package relay carries guest TCP connections over one WebSocket, so a program
// running in a browser tab can reach the network.
//
// # Why a relay exists at all
//
// A browser cannot open a TCP socket, and `fetch` cannot substitute: a
// cross-origin request to a server that has not opted into CORS rejects with an
// opaque error that cannot even be reported accurately, `no-cors` yields a
// response with no status, headers or body, and `Set-Cookie` is never visible to
// JavaScript at any setting. Terminating TLS inside the guest does not help --
// it converts "TLS is opaque" into "CORS blocks it".
//
// A WebSocket is exempt from CORS and carries bytes, so the guest's own TLS
// survives end to end: certificate pinning, client certificates and ALPN all
// keep working, because nothing here looks inside the stream.
package relay

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// A minimal RFC 6455 server: binary frames, no extensions, no fragmentation on
// the send side.
//
// Hand-rolled rather than taken from a library because this module has exactly
// one direct dependency and the subset needed is small and closed -- the parts
// people reach for a library to avoid (extensions, permessage-deflate, client
// mode, autobahn edge cases) are all unused here. What IS easy to get wrong is
// framing, so that is what the tests cover.

// ⚠️ TRANSCRIBE THIS, DO NOT RECALL IT. An earlier version of this line read
// ...-95CA-5AB0DC85B11C -- the last group's leading "C" rotated to the end. It is
// a valid-looking GUID, and every hash computed from it is self-consistent, so
// three independent recomputations all "confirmed" the wrong value. Only a real
// browser rejected it, with "Incorrect Sec-WebSocket-Accept header value".
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxFrame bounds a single inbound frame.
//
// Without a cap a peer can announce a 64-bit length and make the server allocate
// it, which is a one-line denial of service. The guest side never sends frames
// near this.
const maxFrame = 4 << 20

// acceptKey is the handshake response value: base64(sha1(key + GUID)).
//
// The GUID is a constant from the RFC, not a secret. Its only job is to prove
// the responder understood the WebSocket handshake rather than echoing bytes,
// so a caching proxy cannot be tricked into completing one.
func acceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// upgrade completes the handshake and hijacks the connection.
func upgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return nil, nil, fmt.Errorf("relay: not a websocket upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, nil, fmt.Errorf("relay: missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("relay: connection cannot be hijacked")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("relay: hijack: %w", err)
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, rw, nil
}

// readFrame returns one frame's opcode and payload.
//
// ⚠️ A client frame MUST be masked (RFC 6455 §5.1) and a server frame must NOT
// be. Unmasking is not optional politeness: the mask exists so intermediaries
// cannot be induced to interpret attacker-chosen bytes as a request, and a
// server that ignores it reads garbage rather than the payload.
func readFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := int(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = int(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		v := binary.BigEndian.Uint64(b[:])
		if v > maxFrame {
			return 0, nil, fmt.Errorf("relay: frame of %d bytes exceeds the %d cap", v, maxFrame)
		}
		n = int(v)
	}
	if n > maxFrame {
		return 0, nil, fmt.Errorf("relay: frame of %d bytes exceeds the %d cap", n, maxFrame)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range buf {
			buf[i] ^= mask[i%4]
		}
	}
	return op, buf, nil
}

// writeFrame writes one unmasked server frame.
func writeFrame(w io.Writer, op byte, payload []byte) error {
	var h []byte
	n := len(payload)
	switch {
	case n < 126:
		h = []byte{0x80 | op, byte(n)}
	case n < 1<<16:
		h = []byte{0x80 | op, 126, 0, 0}
		binary.BigEndian.PutUint16(h[2:], uint16(n))
	default:
		h = make([]byte, 10)
		h[0] = 0x80 | op
		h[1] = 127
		binary.BigEndian.PutUint64(h[2:], uint64(n))
	}
	if _, err := w.Write(h); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
