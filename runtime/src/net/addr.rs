//! Linux `sockaddr_in` / `sockaddr_in6` <-> `SockAddr`.
//!
//! Pure byte-slice functions, deliberately: they take and return buffers rather
//! than reaching into the arena, so the whole codec -- including the in/out
//! `addrlen` protocol, which is the part that is easy to get subtly wrong -- is
//! reachable from `cargo test` without a guest, a host, or wasm.

use super::SockAddr;

/// Linux `AF_INET` / `AF_INET6`, in the guest's numbering.
pub const AF_INET: u16 = 2;
pub const AF_INET6: u16 = 10;

/// `sizeof(struct sockaddr_in)` and `sizeof(struct sockaddr_in6)`.
pub const SOCKADDR_IN_LEN: usize = 16;
pub const SOCKADDR_IN6_LEN: usize = 28;

/// Parses a guest `sockaddr_in`/`sockaddr_in6`.
///
/// `addrlen` is the caller-supplied length (a value, as in connect/bind/sendto),
/// and it is load-bearing rather than advisory: an `AF_INET6` address is only
/// accepted when the caller claims at least 28 bytes, because reading the
/// 16-byte address out of a 16-byte `sockaddr_in` would read past what the
/// guest actually provided.
pub fn parse(bytes: &[u8], addrlen: usize) -> Option<SockAddr> {
    if addrlen < 8 || bytes.len() < 8 {
        return None;
    }
    let fam = u16::from_le_bytes([bytes[0], bytes[1]]);
    // The port is NETWORK order on the wire and host order in `SockAddr`.
    let port = u16::from_be_bytes([bytes[2], bytes[3]]);
    match fam {
        AF_INET if bytes.len() >= 8 => {
            let mut o = [0u8; 16];
            o[..4].copy_from_slice(&bytes[4..8]);
            Some(SockAddr {
                octets: o,
                v6: false,
                port,
            })
        }
        AF_INET6 if addrlen >= SOCKADDR_IN6_LEN && bytes.len() >= 24 => {
            let mut o = [0u8; 16];
            o.copy_from_slice(&bytes[8..24]);
            Some(SockAddr {
                octets: o,
                v6: true,
                port,
            })
        }
        _ => None,
    }
}

/// Encodes a `SockAddr` as the Linux sockaddr a guest expects, returning the
/// bytes and their true size.
pub fn encode(a: &SockAddr) -> (Vec<u8>, usize) {
    if a.v6 {
        let mut b = vec![0u8; SOCKADDR_IN6_LEN];
        b[0..2].copy_from_slice(&AF_INET6.to_le_bytes());
        b[2..4].copy_from_slice(&a.port.to_be_bytes());
        b[8..24].copy_from_slice(&a.octets);
        (b, SOCKADDR_IN6_LEN)
    } else {
        let mut b = vec![0u8; SOCKADDR_IN_LEN];
        b[0..2].copy_from_slice(&AF_INET.to_le_bytes());
        b[2..4].copy_from_slice(&a.port.to_be_bytes());
        b[4..8].copy_from_slice(&a.octets[..4]);
        (b, SOCKADDR_IN_LEN)
    }
}

/// How much of an encoded address to copy out, given the caller's buffer size.
///
/// ⚠️ THE RETURNED SIZE IS THE TRUE SIZE, NOT THE COPIED SIZE. accept,
/// getsockname and recvfrom all take `addrlen` as an in/out parameter, and the
/// contract is that a short buffer receives a TRUNCATED address while
/// `*addrlen` reports what the full one would have been -- that is how the
/// caller learns it was truncated. Writing back the copied length instead makes
/// truncation invisible, and the guest then believes it has a whole address.
pub fn fit(size: usize, avail: usize) -> (usize, usize) {
    (size.min(avail), size)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn v4_bytes(port: u16, octets: [u8; 4]) -> Vec<u8> {
        let mut b = vec![0u8; SOCKADDR_IN_LEN];
        b[0..2].copy_from_slice(&AF_INET.to_le_bytes());
        b[2..4].copy_from_slice(&port.to_be_bytes());
        b[4..8].copy_from_slice(&octets);
        b
    }

    #[test]
    fn v4_round_trips() {
        let bytes = v4_bytes(8080, [127, 0, 0, 1]);
        let a = parse(&bytes, SOCKADDR_IN_LEN).expect("v4 must parse");
        assert!(!a.v6);
        assert_eq!(a.port, 8080);
        assert_eq!(a.bytes(), &[127, 0, 0, 1]);

        let (out, size) = encode(&a);
        assert_eq!(size, SOCKADDR_IN_LEN);
        assert_eq!(out, bytes, "encode must reproduce the guest's own bytes");
    }

    #[test]
    fn v6_round_trips() {
        let octets: [u8; 16] = [0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1];
        let a = SockAddr {
            octets,
            v6: true,
            port: 443,
        };
        let (bytes, size) = encode(&a);
        assert_eq!(size, SOCKADDR_IN6_LEN);
        let back = parse(&bytes, SOCKADDR_IN6_LEN).expect("v6 must parse");
        assert_eq!(back, a);
    }

    #[test]
    fn the_port_is_byte_swapped_exactly_once() {
        // A port whose two bytes differ, so a missing or doubled swap shows.
        // 8080 = 0x1f90; on the wire that is 1f 90, and host order is 0x1f90.
        let bytes = v4_bytes(8080, [10, 0, 0, 7]);
        assert_eq!(bytes[2], 0x1f, "wire byte 0 must be the high byte");
        assert_eq!(bytes[3], 0x90);
        assert_eq!(parse(&bytes, SOCKADDR_IN_LEN).unwrap().port, 8080);
    }

    #[test]
    fn v6_needs_the_caller_to_claim_28_bytes() {
        let a = SockAddr {
            octets: [1u8; 16],
            v6: true,
            port: 1,
        };
        let (bytes, _) = encode(&a);
        // The bytes are a valid sockaddr_in6, but a caller claiming only
        // sizeof(sockaddr_in) has not provided a v6 address -- reading 16
        // octets out of it would read past what it supplied.
        assert!(parse(&bytes, SOCKADDR_IN_LEN).is_none());
        assert!(parse(&bytes, SOCKADDR_IN6_LEN).is_some());
    }

    #[test]
    fn a_short_addrlen_is_rejected_outright() {
        let bytes = v4_bytes(80, [1, 2, 3, 4]);
        assert!(parse(&bytes, 7).is_none());
        assert!(parse(&bytes, 8).is_some());
    }

    #[test]
    fn an_unknown_family_is_rejected() {
        let mut bytes = v4_bytes(80, [1, 2, 3, 4]);
        bytes[0] = 99; // neither AF_INET nor AF_INET6
        assert!(parse(&bytes, SOCKADDR_IN_LEN).is_none());
    }

    #[test]
    fn fit_reports_the_true_size_when_truncating() {
        // The whole point: a 4-byte buffer receives 4 bytes, and the caller is
        // told the address is really 16 so it knows it was truncated.
        assert_eq!(fit(SOCKADDR_IN_LEN, 4), (4, SOCKADDR_IN_LEN));
        assert_eq!(fit(SOCKADDR_IN_LEN, 64), (SOCKADDR_IN_LEN, SOCKADDR_IN_LEN));
        assert_eq!(fit(SOCKADDR_IN6_LEN, 16), (16, SOCKADDR_IN6_LEN));
    }
}
