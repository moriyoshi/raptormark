//! `SockAddr` <-> WASIX `__wasi_addr_port_t`.
//!
//! A sibling of `net::addr`, and pure for the same reason: it takes and returns
//! byte slices rather than reaching into anything, so the whole codec is
//! reachable from `cargo test` without wasm, without wasmer, and without a
//! network. That matters more here than it does for `addr.rs`, because this
//! encoding has two traps and neither of them fails loudly.
//!
//! # Trap 1: the port comes BEFORE the address, after a padding byte
//!
//! ```text
//! __wasi_addr_port_t {           // 20 bytes
//!     tag:      u8,   // @0   Addressfamily
//!     _padding: u8,   // @1   "must be set to zero otherwise the tag will corrupt"
//!     octs:     [u8; 18],  // @2  = port (2 bytes) then the address
//! }
//! ```
//!
//! So: **tag @0, padding @1, port @2..4, address @4**. That is the opposite
//! order to `sockaddr_in`, where the family precedes the port precedes the
//! address -- and the padding byte means even a codec that gets the ORDER right
//! is one byte out if it writes the port at offset 1.
//!
//! # Trap 2: THE TWO DIRECTIONS DISAGREE ABOUT ENDIANNESS
//!
//! ⚠️ This is not a subtlety, it is an inconsistency in wasmer, and a
//! symmetric codec is guaranteed to be wrong in one direction:
//!
//! | direction | wasmer function | port |
//! |---|---|---|
//! | we WRITE: `sock_bind`, `sock_connect`, `sock_send_to` | `read_ip_port` -> `u16::from_ne_bytes` | **little**-endian |
//! | we READ: `sock_addr_local`/`_peer`, `sock_recv_from`, `sock_accept` | `write_ip_port` -> `port.to_be_bytes()` | **big**-endian |
//!
//! Measured against wasmer 7.3.0, not inferred from the source: a bind given
//! the bytes `90 1f` lands on port 8080, and reading it back yields `1f 90`.
//! `.agents/docs/WASIX_ABI.md` has the dump and the probe.
//!
//! The failure this shape produces is the bad kind. A byte-swapped port is
//! still a valid port, so the guest binds successfully to somewhere nobody is
//! looking, or dials an endpoint that merely refuses. Nothing returns an error
//! and nothing is out of range.
//!
//! Hence `encode` and `decode` rather than one round-tripping pair, and hence
//! the tests below assert the raw bytes at each offset rather than that a value
//! survives a round trip -- a round trip is exactly what a consistently wrong
//! codec passes.

use super::SockAddr;

/// `Addressfamily`, from `lib/wasi-types/src/wasi/bindings.rs`.
///
/// ⚠️ `Socktype` is NOT the same numbering as WasmEdge's, either: WASIX has
/// `Stream=1 Dgram=2`, WasmEdge has `Dgram=1 Stream=2`. Those live in the
/// backend; only the families are needed here.
pub const AF_INET4: u8 = 1;
pub const AF_INET6: u8 = 2;

/// `sizeof(__wasi_addr_port_t)`: 1 tag + 1 padding + 18 octets.
pub const ADDR_PORT_LEN: usize = 20;

/// Offsets within the struct, spelled out because every one of them has been a
/// mistake somewhere: the padding byte is easy to omit and the port is easy to
/// put last.
const OFF_TAG: usize = 0;
const OFF_PAD: usize = 1;
const OFF_PORT: usize = 2;
const OFF_ADDR: usize = 4;

/// Encodes an address for the calls that CONSUME one -- `sock_bind`,
/// `sock_connect`, `sock_send_to`.
///
/// The port goes out **little**-endian, because wasmer reads it back with
/// `from_ne_bytes` and the guest memory is wasm32.
pub fn encode(a: &SockAddr) -> [u8; ADDR_PORT_LEN] {
    let mut b = [0u8; ADDR_PORT_LEN];
    b[OFF_TAG] = if a.v6 { AF_INET6 } else { AF_INET4 };
    b[OFF_PAD] = 0;
    b[OFF_PORT..OFF_PORT + 2].copy_from_slice(&a.port.to_le_bytes());
    let octets = a.bytes();
    b[OFF_ADDR..OFF_ADDR + octets.len()].copy_from_slice(octets);
    b
}

/// Decodes an address from the calls that PRODUCE one -- `sock_addr_local`,
/// `sock_addr_peer`, `sock_recv_from`, `sock_accept`.
///
/// The port arrives **big**-endian, because wasmer writes it with
/// `to_be_bytes`. Yes, that contradicts `encode`. See the module header.
///
/// `None` for a buffer that is too short or a family this runtime does not
/// speak -- including `Unspec`, which is what an unfilled buffer looks like and
/// must not be mistaken for `0.0.0.0:0`.
pub fn decode(b: &[u8]) -> Option<SockAddr> {
    if b.len() < ADDR_PORT_LEN {
        return None;
    }
    let port = u16::from_be_bytes([b[OFF_PORT], b[OFF_PORT + 1]]);
    let mut octets = [0u8; 16];
    match b[OFF_TAG] {
        AF_INET4 => {
            octets[..4].copy_from_slice(&b[OFF_ADDR..OFF_ADDR + 4]);
            Some(SockAddr {
                octets,
                v6: false,
                port,
            })
        }
        AF_INET6 => {
            octets.copy_from_slice(&b[OFF_ADDR..OFF_ADDR + 16]);
            Some(SockAddr {
                octets,
                v6: true,
                port,
            })
        }
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 8080 = 0x1f90. The two bytes differ, which is the whole point: a port
    /// like 0 or 4369 (0x1111) cannot tell a correct codec from a byte-swapped
    /// one, and neither can any assertion made with one.
    const PORT: u16 = 8080;

    fn v4() -> SockAddr {
        SockAddr::v4([127, 0, 0, 1], PORT)
    }

    /// ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A codec that put
    /// the port at offset 1 (forgetting `_padding`) would shift every field by
    /// one and still round-trip perfectly against its own decoder. So this
    /// asserts the BYTES, against the dump in `.agents/docs/WASIX_ABI.md` that
    /// came out of a real wasmer.
    #[test]
    fn encode_lays_the_struct_out_at_the_measured_offsets() {
        let b = encode(&v4());
        assert_eq!(b.len(), 20, "__wasi_addr_port_t is 20 bytes");
        assert_eq!(b[0], AF_INET4, "@0 tag");
        assert_eq!(
            b[1], 0,
            "@1 is _padding and MUST be zero -- wasmer's own \
                             comment says a non-zero byte corrupts the tag"
        );
        assert_eq!(
            b[2], 0x90,
            "@2 port low byte: the port is LITTLE-endian going out"
        );
        assert_eq!(b[3], 0x1f, "@3 port high byte");
        assert_eq!(&b[4..8], &[127, 0, 0, 1], "@4 the address, AFTER the port");
        assert!(b[8..].iter().all(|&x| x == 0), "the v4 tail must be zeroed");
    }

    /// The other direction, against the same measured dump: wasmer answered
    /// `01 00 1f 90 7f 00 00 01` for a socket bound to 127.0.0.1:8080.
    #[test]
    fn decode_reads_the_port_big_endian_because_wasmer_writes_it_that_way() {
        let mut b = [0u8; ADDR_PORT_LEN];
        b[0] = AF_INET4;
        b[2] = 0x1f; // high byte FIRST on the way in
        b[3] = 0x90;
        b[4..8].copy_from_slice(&[127, 0, 0, 1]);
        let a = decode(&b).expect("a well-formed Inet4 address must decode");
        assert_eq!(a.port, 8080);
        assert!(!a.v6);
        assert_eq!(a.bytes(), &[127, 0, 0, 1]);
    }

    /// ❗ THE ASYMMETRY ITSELF, stated as a property rather than as two
    /// separate byte assertions that could drift apart.
    ///
    /// If someone "fixes" the inconsistency by making both directions agree,
    /// every other test here still passes -- each one only looks at one
    /// direction. This is the one that fails.
    #[test]
    fn encode_and_decode_deliberately_disagree_about_byte_order() {
        let encoded = encode(&v4());
        let back = decode(&encoded).expect("it is still a valid struct");
        assert_ne!(
            back.port, PORT,
            "encode writes the port little-endian and decode reads it \
             big-endian, so feeding one to the other MUST byte-swap. If this \
             now round-trips, the codec was made symmetric -- and it is then \
             wrong against wasmer in exactly one direction, silently, because \
             a byte-swapped port is still a valid port. See the measured table \
             in .agents/docs/WASIX_ABI.md before changing either function."
        );
        assert_eq!(
            back.port,
            PORT.swap_bytes(),
            "and the difference is exactly a swap"
        );
        // Everything that is NOT the port does round-trip, which is what makes
        // the assertion above about the port alone.
        assert_eq!(back.bytes(), v4().bytes());
        assert_eq!(back.v6, false);
    }

    #[test]
    fn v6_uses_all_sixteen_octets_and_still_starts_at_offset_four() {
        let octets: [u8; 16] = [0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1];
        let a = SockAddr {
            octets,
            v6: true,
            port: 443,
        };
        let b = encode(&a);
        assert_eq!(b[0], AF_INET6);
        assert_eq!(
            &b[4..20],
            &octets,
            "16 octets from @4 fill the struct exactly"
        );
        // 443 = 0x01bb, little-endian on the way out.
        assert_eq!([b[2], b[3]], [0xbb, 0x01]);

        let mut r = b;
        r[2..4].copy_from_slice(&443u16.to_be_bytes()); // as wasmer would write it
        let back = decode(&r).expect("v6 must decode");
        assert_eq!(back.port, 443);
        assert!(back.v6);
        assert_eq!(back.octets, octets);
    }

    /// An unfilled buffer is all zeros, and zero is `Addressfamily::Unspec`.
    /// Accepting it would report `0.0.0.0:0` as a real peer -- which is what a
    /// failed `sock_addr_peer` leaves behind.
    #[test]
    fn an_unspecified_family_is_rejected_rather_than_read_as_v4() {
        let b = [0u8; ADDR_PORT_LEN];
        assert!(decode(&b).is_none(), "Unspec (0) must not decode");
        let mut unix = [0u8; ADDR_PORT_LEN];
        unix[0] = 3; // Addressfamily::Unix
        assert!(
            decode(&unix).is_none(),
            "Unix must not decode as an IP address"
        );
    }

    #[test]
    fn a_short_buffer_is_rejected_rather_than_read_past() {
        let mut b = [0u8; ADDR_PORT_LEN];
        b[0] = AF_INET4;
        assert!(decode(&b[..19]).is_none(), "19 bytes is not the struct");
        assert!(decode(&b).is_some());
    }
}
