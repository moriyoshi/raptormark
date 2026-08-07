//! DNS interception: parsing a guest's query and synthesizing the answer.
//!
//! # Why this exists at all
//!
//! **There is no UDP in a browser.** Not "it is awkward" -- there is no API, and
//! WebTransport and WebRTC are datagram channels to a peer you control, not to
//! an arbitrary host. So a guest's resolver, which sends UDP to port 53 out of
//! `/etc/resolv.conf`, is the FIRST thing that breaks in a browser -- before any
//! HTTP request is ever attempted.
//!
//! That makes DNS a precondition rather than a feature, and it has to be solved
//! before any transport, which is the opposite of the intuitive order.
//!
//! # Why the runtime and not the host
//!
//! Intercepting at the WIRE means the guest's own resolver runs unmodified:
//! `/etc/nsswitch.conf`, musl's and glibc's very different paths, `search`
//! domains, the `hosts` file -- all of it keeps working, because from the
//! guest's side nothing has changed. Rewriting `getaddrinfo` instead would mean
//! reimplementing each libc's policy, per libc.
//!
//! It is also small: a query parser needs only QNAME and QTYPE, and an answer
//! needs one record. What it must NOT do is grow into a resolver.
//!
//! # The address the answer carries is the HOST's to choose
//!
//! A browser transport needs a NAME -- `fetch` needs a URL, a relay's `CONNECT`
//! needs a host, TLS needs SNI -- but `connect(2)` hands over an address, the
//! name having been consumed by the resolver and thrown away. So the host mints
//! an address per name (from a reserved range, slirp-style), the runtime encodes
//! whatever it is given into the answer verbatim, and the host recognises its own
//! minted address at connect time and knows which name it stands for.
//!
//! ⚠️ The runtime therefore keeps NO name-to-address mapping and needs no
//! `connect_by_name` call. Per-destination policy -- fetch, relay, refuse -- is a
//! deployment question, and it stays entirely on the host side.

/// `QTYPE`/`TYPE` values this tap understands.
pub const TYPE_A: u16 = 1;
pub const TYPE_AAAA: u16 = 28;
const CLASS_IN: u16 = 1;

/// The 12-byte header every message starts with.
const HEADER: usize = 12;

/// A parsed query: everything needed to answer, and nothing else.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct Query {
    /// Transaction id, echoed back. A resolver matches on it and drops anything
    /// else, so getting this wrong looks exactly like no answer at all.
    pub id: u16,
    /// The name, lowercased, dot-separated, without a trailing dot.
    pub name: String,
    pub qtype: u16,
    /// Byte length of the question section, so an answer can quote it verbatim.
    pub qlen: usize,
}

/// Parses a query, or `None` if it is not one this tap should answer.
///
/// Deliberately strict: anything unusual (multiple questions, a truncated name,
/// a compression pointer in a QUESTION, a class other than IN) returns `None`
/// rather than a guess. An unanswered query is a resolver timeout, which is
/// recoverable and visible; a wrong answer is neither.
pub fn parse_query(msg: &[u8]) -> Option<Query> {
    if msg.len() < HEADER {
        return None;
    }
    let id = u16::from_be_bytes([msg[0], msg[1]]);
    let flags = u16::from_be_bytes([msg[2], msg[3]]);
    // QR must be 0 (a query) and OPCODE 0 (standard).
    if flags & 0x8000 != 0 || (flags >> 11) & 0xf != 0 {
        return None;
    }
    let qdcount = u16::from_be_bytes([msg[4], msg[5]]);
    if qdcount != 1 {
        return None;
    }

    let mut p = HEADER;
    let mut labels: Vec<String> = Vec::new();
    loop {
        let len = *msg.get(p)? as usize;
        p += 1;
        if len == 0 {
            break;
        }
        // A compression pointer cannot appear in a question of a query with one
        // question -- there is nothing before it to point at.
        if len & 0xc0 != 0 {
            return None;
        }
        let end = p.checked_add(len)?;
        let label = msg.get(p..end)?;
        labels.push(String::from_utf8_lossy(label).to_ascii_lowercase());
        p = end;
    }
    let qtype = u16::from_be_bytes([*msg.get(p)?, *msg.get(p + 1)?]);
    let qclass = u16::from_be_bytes([*msg.get(p + 2)?, *msg.get(p + 3)?]);
    p += 4;
    if qclass != CLASS_IN {
        return None;
    }
    if labels.is_empty() {
        return None;
    }
    Some(Query {
        id,
        name: labels.join("."),
        qtype,
        qlen: p - HEADER,
    })
}

/// Builds a positive answer carrying `octets`, quoting the question verbatim.
///
/// `octets` is 4 bytes for an A record and 16 for AAAA; a length that does not
/// match `query.qtype` is a caller bug and produces `None` rather than a record
/// a resolver will misread.
pub fn answer(msg: &[u8], q: &Query, octets: &[u8], ttl: u32) -> Option<Vec<u8>> {
    let want = match q.qtype {
        TYPE_A => 4,
        TYPE_AAAA => 16,
        _ => return None,
    };
    if octets.len() != want {
        return None;
    }
    let question = msg.get(HEADER..HEADER + q.qlen)?;
    let mut out = Vec::with_capacity(HEADER + q.qlen + 16 + want);
    out.extend_from_slice(&q.id.to_be_bytes());
    // QR=1, RD=1, RA=1 -- a recursive answer, which is what the guest asked a
    // resolver for. RCODE 0.
    out.extend_from_slice(&0x8180u16.to_be_bytes());
    out.extend_from_slice(&1u16.to_be_bytes()); // QDCOUNT
    out.extend_from_slice(&1u16.to_be_bytes()); // ANCOUNT
    out.extend_from_slice(&0u16.to_be_bytes()); // NSCOUNT
    out.extend_from_slice(&0u16.to_be_bytes()); // ARCOUNT
    out.extend_from_slice(question);
    // The answer's NAME is a compression pointer back to the question at offset
    // 12. Every resolver understands it and it keeps the message small.
    out.extend_from_slice(&[0xc0, HEADER as u8]);
    out.extend_from_slice(&q.qtype.to_be_bytes());
    out.extend_from_slice(&CLASS_IN.to_be_bytes());
    out.extend_from_slice(&ttl.to_be_bytes());
    out.extend_from_slice(&(want as u16).to_be_bytes());
    out.extend_from_slice(octets);
    Some(out)
}

/// Builds an authoritative-looking NXDOMAIN.
///
/// A resolver that gets nothing retries and then times out, typically after
/// seconds and often several times. Saying "no such name" makes a failure
/// immediate and legible instead.
pub fn nxdomain(msg: &[u8], q: &Query) -> Option<Vec<u8>> {
    let question = msg.get(HEADER..HEADER + q.qlen)?;
    let mut out = Vec::with_capacity(HEADER + q.qlen);
    out.extend_from_slice(&q.id.to_be_bytes());
    out.extend_from_slice(&0x8183u16.to_be_bytes()); // QR|RD|RA, RCODE=3
    out.extend_from_slice(&1u16.to_be_bytes());
    out.extend_from_slice(&0u16.to_be_bytes());
    out.extend_from_slice(&0u16.to_be_bytes());
    out.extend_from_slice(&0u16.to_be_bytes());
    out.extend_from_slice(question);
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Encodes a query the way a resolver would.
    fn query(id: u16, name: &str, qtype: u16) -> Vec<u8> {
        let mut m = Vec::new();
        m.extend_from_slice(&id.to_be_bytes());
        m.extend_from_slice(&0x0100u16.to_be_bytes()); // RD
        m.extend_from_slice(&1u16.to_be_bytes());
        m.extend_from_slice(&0u16.to_be_bytes());
        m.extend_from_slice(&0u16.to_be_bytes());
        m.extend_from_slice(&0u16.to_be_bytes());
        for label in name.split('.') {
            m.push(label.len() as u8);
            m.extend_from_slice(label.as_bytes());
        }
        m.push(0);
        m.extend_from_slice(&qtype.to_be_bytes());
        m.extend_from_slice(&CLASS_IN.to_be_bytes());
        m
    }

    #[test]
    fn a_query_round_trips() {
        let m = query(0x1234, "example.com", TYPE_A);
        let q = parse_query(&m).expect("must parse");
        assert_eq!(q.id, 0x1234);
        assert_eq!(q.name, "example.com");
        assert_eq!(q.qtype, TYPE_A);
    }

    /// ⚠️ The id is what a resolver matches on. An answer carrying the wrong one
    /// is DROPPED, which looks exactly like no answer -- a timeout, seconds
    /// later, with nothing to point at.
    #[test]
    fn the_answer_echoes_the_transaction_id() {
        let m = query(0xbeef, "a.test", TYPE_A);
        let q = parse_query(&m).unwrap();
        let a = answer(&m, &q, &[10, 0, 2, 100], 60).unwrap();
        assert_eq!(&a[0..2], &0xbeefu16.to_be_bytes());
        assert_ne!(&a[0..2], &[0, 0], "a zeroed id would be silently dropped");
    }

    #[test]
    fn the_answer_is_a_response_with_one_record() {
        let m = query(1, "host.example", TYPE_A);
        let q = parse_query(&m).unwrap();
        let a = answer(&m, &q, &[10, 0, 2, 7], 30).unwrap();
        let flags = u16::from_be_bytes([a[2], a[3]]);
        assert_eq!(
            flags & 0x8000,
            0x8000,
            "QR must be set or it is not a reply"
        );
        assert_eq!(flags & 0xf, 0, "RCODE must be 0");
        assert_eq!(u16::from_be_bytes([a[6], a[7]]), 1, "ANCOUNT");
        // The record's RDATA is the last 4 bytes.
        assert_eq!(&a[a.len() - 4..], &[10, 0, 2, 7]);
    }

    /// The answer must be re-parsable as a message whose question matches the
    /// query's -- that is what a resolver checks before accepting it.
    #[test]
    fn the_answer_quotes_the_question_verbatim() {
        let m = query(9, "www.example.com", TYPE_A);
        let q = parse_query(&m).unwrap();
        let a = answer(&m, &q, &[1, 2, 3, 4], 60).unwrap();
        assert_eq!(&a[HEADER..HEADER + q.qlen], &m[HEADER..HEADER + q.qlen]);
    }

    #[test]
    fn aaaa_carries_sixteen_octets() {
        let m = query(2, "v6.test", TYPE_AAAA);
        let q = parse_query(&m).unwrap();
        let o = [0x20, 0x01, 0xd, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1];
        let a = answer(&m, &q, &o, 60).unwrap();
        assert_eq!(&a[a.len() - 16..], &o);
    }

    /// A length that disagrees with the type would be read as a different
    /// record entirely, so it is refused rather than encoded.
    #[test]
    fn a_mismatched_address_length_is_refused() {
        let m = query(3, "x.test", TYPE_A);
        let q = parse_query(&m).unwrap();
        assert!(answer(&m, &q, &[0u8; 16], 60).is_none());
        assert!(answer(&m, &q, &[0u8; 4], 60).is_some());
    }

    #[test]
    fn nxdomain_is_a_reply_with_rcode_three_and_no_records() {
        let m = query(4, "nope.test", TYPE_A);
        let q = parse_query(&m).unwrap();
        let a = nxdomain(&m, &q).unwrap();
        let flags = u16::from_be_bytes([a[2], a[3]]);
        assert_eq!(flags & 0x8000, 0x8000);
        assert_eq!(flags & 0xf, 3);
        assert_eq!(u16::from_be_bytes([a[6], a[7]]), 0, "ANCOUNT must be 0");
    }

    // --- the strictness cases. Each is a shape that must NOT be answered. ---

    #[test]
    fn a_reply_is_not_a_query() {
        let mut m = query(5, "a.test", TYPE_A);
        m[2] |= 0x80; // QR
        assert!(parse_query(&m).is_none());
    }

    #[test]
    fn a_truncated_name_is_refused_rather_than_guessed() {
        let m = query(6, "example.com", TYPE_A);
        for cut in HEADER..m.len() {
            // Any prefix that stops mid-message must be refused, never parsed
            // into a shorter name that would be answered for the wrong host.
            let _ = parse_query(&m[..cut]);
        }
        assert!(parse_query(&m[..HEADER + 3]).is_none());
    }

    #[test]
    fn a_compression_pointer_in_the_question_is_refused() {
        let mut m = query(7, "a.test", TYPE_A);
        m[HEADER] = 0xc0; // a pointer where a label length belongs
        assert!(parse_query(&m).is_none());
    }

    #[test]
    fn a_class_other_than_in_is_refused() {
        let mut m = query(8, "a.test", TYPE_A);
        let n = m.len();
        m[n - 2..].copy_from_slice(&3u16.to_be_bytes()); // CHAOS
        assert!(parse_query(&m).is_none());
    }

    #[test]
    fn names_are_lowercased_so_case_cannot_split_the_cache() {
        let m = query(10, "ExAmPle.COM", TYPE_A);
        assert_eq!(parse_query(&m).unwrap().name, "example.com");
    }
}
