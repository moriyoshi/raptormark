//! The `poll_oneoff` subscription and event byte layout.
//!
//! Two backends build these buffers by hand -- `net::wasmedge` and
//! `net::wasix` -- and until this module they were two independent sets of
//! magic offsets with **no host test at all**, because both live behind
//! `cfg(target_arch = "wasm32")`. The layout is preview1's and does not vary by
//! host, so it belongs here: pure, uncfg'd, and asserted byte by byte.
//!
//! ```text
//! subscription (48 bytes)          event (32 bytes)
//!   userdata  u64   @0               userdata  u64   @0
//!   type      u8    @8               error     u16   @8
//!   -- union        @16              type      u8    @10
//!      fd_readwrite:                 -- union        @16
//!        fd      u32 @16                fd_readwrite:
//!      clock:                             nbytes  u64 @16
//!        clock_id u32 @16                 flags   u16 @24
//!        timeout  u64 @24
//!        precision u64 @32
//!        flags    u16 @40
//! ```
//!
//! # ⚠️ WHAT THIS MODULE DOES NOT DECIDE: what a zero timeout MEANS
//!
//! The layout is shared; the semantics are not, and the difference is a hang.
//!
//! * **preview1 / WasmEdge** -- a clock subscription with `timeout = 0` returns
//!   immediately. `net::wasmedge::ready` relies on exactly that for its
//!   zero-cost readiness probe.
//! * **WASIX** -- `timeout == 0` means `Duration::MAX`, i.e. **wait forever**,
//!   and the subscription is not even recorded. `timeout == 1` is its immediate
//!   probe.
//!
//! Measured, not read: `.agents/docs/WASIX_ABI.md` has the two runs, one of
//! which has to be killed. So `clock_sub` takes the nanoseconds it is given and
//! each backend passes the value its host means. Defaulting one here would put
//! a hang behind a helper.

/// `sizeof(Subscription)` and `sizeof(Event)`.
pub const SUB_LEN: usize = 48;
pub const EVENT_LEN: usize = 32;

/// `Eventtype`.
const EVENT_CLOCK: u8 = 0;
const EVENT_FD_READ: u8 = 1;
const EVENT_FD_WRITE: u8 = 2;

/// `Clockid::Monotonic`. A wall clock would make a relative timeout jump with
/// an NTP step.
pub const CLOCK_MONOTONIC: u32 = 1;

/// Subscribes slot `k` to readability or writability of `fd`.
///
/// `buf` must be at least `SUB_LEN * (k + 1)` bytes and is expected to start
/// zeroed -- every field this does not write is defined to be zero, and the
/// slice is only ever a fresh local.
pub fn fd_sub(buf: &mut [u8], k: usize, userdata: u64, fd: u32, want_write: bool) {
    let off = SUB_LEN * k;
    buf[off..off + 8].copy_from_slice(&userdata.to_le_bytes());
    buf[off + 8] = if want_write {
        EVENT_FD_WRITE
    } else {
        EVENT_FD_READ
    };
    buf[off + 16..off + 20].copy_from_slice(&fd.to_le_bytes());
}

/// Subscribes slot `k` to a relative monotonic timeout.
///
/// ⚠️ `timeout_nanos` is passed through verbatim. See the module header: 0 is
/// "return now" to preview1 and "never return" to WASIX, and choosing for the
/// caller here is how that becomes a hang nobody can grep for.
pub fn clock_sub(buf: &mut [u8], k: usize, userdata: u64, timeout_nanos: u64) {
    let off = SUB_LEN * k;
    buf[off..off + 8].copy_from_slice(&userdata.to_le_bytes());
    buf[off + 8] = EVENT_CLOCK;
    buf[off + 16..off + 20].copy_from_slice(&CLOCK_MONOTONIC.to_le_bytes());
    buf[off + 24..off + 32].copy_from_slice(&timeout_nanos.to_le_bytes());
}

/// The `userdata` of event `i`, which is how a result is matched back to the
/// subscription that asked for it.
pub fn event_userdata(events: &[u8], i: usize) -> u64 {
    let off = EVENT_LEN * i;
    u64::from_le_bytes(events[off..off + 8].try_into().unwrap())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// ⚠️ WHAT WOULD A PASS LOOK LIKE IF THE CLAIM WERE FALSE? A subscription
    /// writer with every field shifted still round-trips against a reader that
    /// is shifted the same way -- and there is no reader here, the host is. So
    /// these assert absolute offsets against the preview1 struct, which is what
    /// the host actually parses.
    #[test]
    fn an_fd_subscription_lands_at_the_preview1_offsets() {
        let mut b = [0u8; SUB_LEN];
        fd_sub(&mut b, 0, 0x0102_0304_0506_0708, 0x1234, false);
        assert_eq!(
            &b[0..8],
            &[0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01],
            "userdata is a little-endian u64 at @0"
        );
        assert_eq!(b[8], EVENT_FD_READ, "@8 is the event type");
        assert!(b[9..16].iter().all(|&x| x == 0), "@9..16 is padding");
        assert_eq!(&b[16..20], &[0x34, 0x12, 0, 0], "@16 is the fd, u32 LE");
        assert!(b[20..].iter().all(|&x| x == 0), "nothing past the fd");
    }

    #[test]
    fn write_interest_differs_from_read_only_in_the_type_byte() {
        let mut r = [0u8; SUB_LEN];
        let mut w = [0u8; SUB_LEN];
        fd_sub(&mut r, 0, 7, 3, false);
        fd_sub(&mut w, 0, 7, 3, true);
        assert_eq!(r[8], EVENT_FD_READ);
        assert_eq!(w[8], EVENT_FD_WRITE);
        r[8] = 0;
        w[8] = 0;
        assert_eq!(r, w, "the interest must not leak into any other field");
    }

    /// The clock's fields are spread across the union with a gap, which is the
    /// part that gets written from memory and gets wrong: `timeout` is at @24,
    /// not at @20 where a naive packing would put it after a u32 clock id.
    #[test]
    fn a_clock_subscription_puts_the_timeout_at_offset_24() {
        let mut b = [0u8; SUB_LEN];
        clock_sub(&mut b, 0, 5, 1_000_000_000);
        assert_eq!(&b[0..8], &5u64.to_le_bytes());
        assert_eq!(b[8], EVENT_CLOCK);
        assert_eq!(
            &b[16..20],
            &CLOCK_MONOTONIC.to_le_bytes(),
            "@16 is the clock id"
        );
        assert!(
            b[20..24].iter().all(|&x| x == 0),
            "@20..24 is padding before the u64 -- writing the timeout here is \
             the mistake this asserts against"
        );
        assert_eq!(&b[24..32], &1_000_000_000u64.to_le_bytes(), "@24 timeout");
        assert!(
            b[32..].iter().all(|&x| x == 0),
            "precision and flags stay zero: relative, no coalescing"
        );
    }

    /// A zero timeout is written as a zero, unchanged. The module header says
    /// why that is a deliberate non-decision rather than an oversight.
    #[test]
    fn a_zero_timeout_is_passed_through_rather_than_reinterpreted() {
        let mut b = [0u8; SUB_LEN];
        clock_sub(&mut b, 0, 0, 0);
        assert_eq!(&b[24..32], &[0u8; 8], "0 goes to the host as 0");
        let mut one = [0u8; SUB_LEN];
        clock_sub(&mut one, 0, 0, 1);
        assert_eq!(&one[24..32], &1u64.to_le_bytes(), "and 1 as 1");
    }

    #[test]
    fn slots_are_independent_and_stride_by_48() {
        let mut b = [0u8; SUB_LEN * 3];
        fd_sub(&mut b, 0, 100, 10, false);
        clock_sub(&mut b, 1, 101, 42);
        fd_sub(&mut b, 2, 102, 12, true);
        assert_eq!(&b[0..8], &100u64.to_le_bytes());
        assert_eq!(&b[48..56], &101u64.to_le_bytes());
        assert_eq!(&b[96..104], &102u64.to_le_bytes());
        assert_eq!(&b[48 + 24..48 + 32], &42u64.to_le_bytes());
        assert_eq!(&b[96 + 16..96 + 20], &12u32.to_le_bytes());
    }

    #[test]
    fn event_userdata_strides_by_32_not_48() {
        let mut ev = [0u8; EVENT_LEN * 2];
        ev[0..8].copy_from_slice(&9u64.to_le_bytes());
        ev[32..40].copy_from_slice(&77u64.to_le_bytes());
        assert_eq!(event_userdata(&ev, 0), 9);
        assert_eq!(
            event_userdata(&ev, 1),
            77,
            "an Event is 32 bytes; reading it at the SUBSCRIPTION stride is a \
             mistake that produces a plausible userdata from the wrong event"
        );
    }
}
