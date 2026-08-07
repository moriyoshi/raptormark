#!/usr/bin/env bash
# Each ecvisor profile contains exactly one network backend's code.
#
# WHAT THIS CHECKS, precisely. It reads the symbol table of each archive and
# asserts the OTHER backends' modules are absent from it. That is the
# compile-time exclusion `runtime/src/net/mod.rs` relies on: `wasm-ld` emits an
# import for every undefined symbol reachable from live code, so a backend
# compiled in at all can put its imports in the emitted module.
#
# ⚠️ WHAT IT DOES NOT CHECK. It does NOT read the import section of a linked
# module. An archive is not a module; only the final `wasm-ld` decides what
# becomes an import. This proves "the wasmedge backend is not in the loopback
# archive" -- a precondition for "a loopback module imports no sockets", not
# that claim. That claim needs the E2E suite.
#
# ⚠️ TWO THINGS THIS TEST WAS WRONG ABOUT BEFORE, both found by running it:
#
#   1. The 17 `sock_*` symbols in the default archive are NOT the WasmEdge host
#      imports. They are `ecvisor::net::wasmedge::sock_*`, ecvisor's own Rust
#      functions, undefined across codegen units and resolved inside the same
#      archive. So the patterns below match the MODULE PATH, not `sock_`.
#
#   2. `ecvisor::net::loopback` leaves NO symbols in its own archive. It is pure
#      Rust with no extern imports, so at `-Copt-level=3 -Clto=fat
#      -Ccodegen-units=1` every function inlines into its callers and the module
#      path disappears. An "own backend must be present" assertion therefore
#      fails on the one profile whose whole purpose is having no imports.
#      The positive control is the EXPORTED ENTRY POINTS instead, which survive
#      in every profile because the link needs them.
set -euo pipefail

R="${TEST_SRCDIR}/_main"
NM="${LLVM_PREFIX:?LLVM_PREFIX must be set by the BUILD rule}/bin/llvm-nm"

if [ ! -x "$NM" ]; then
  echo "$NM is not here, but the SDK repo rule said it was." >&2
  exit 1
fi

# Rust mangling for `ecvisor::net::<backend>`: length-prefixed path segments.
pat() { printf '_ZN7ecvisor3net%d%s' "${#1}" "$1"; }

# Guards the empty-archive false pass: an archive with nothing in it would
# satisfy every exclusion assertion below. Measured 2026-08-23: 15, 15 and 16
# across the three profiles, so 10 is a floor with room, not a fitted number.
_MIN_EXPORTS=10

declare -A ARCHIVE=(
  [wasmedge]="$R/runtime/libecvisor.a"
  [loopback]="$R/runtime/loopback/libecvisor.a"
  [browser]="$R/runtime/browser/libecvisor.a"
  # ⚠️ `wasix` also carries a LOADER backend, which the loop below neither
  # knows nor cares about: this test asks only which `ecvisor::net::*` module
  # is compiled in. `loader_exclusion_test.sh` asks the other question of the
  # same archive.
  [wasix]="$R/runtime/wasix/libecvisor.a"
)

# ❗ EVERY BACKEND MUST BE IN THIS LIST, and adding one to `ARCHIVE` without
# adding it here is a silent hole: the new archive would go unchecked AND the
# existing archives would never be checked for the new backend's symbols. The
# loop below is quadratic over this one list precisely so the two cannot drift.
PROFILES="wasmedge loopback browser wasix"

# ⚠️ AND `net-loopback` IS A LABEL, NOT A `cfg` -- `runtime/src/net/mod.rs`
# selects loopback by the ABSENCE of every other backend, from two separate
# `any(...)` lists. A new backend added to one list and not the other compiles
# loopback in ALONGSIDE itself, and this test is what notices.

rc=0
for profile in $PROFILES; do
  a="${ARCHIVE[$profile]}"
  if [ ! -f "$a" ]; then
    echo "FAIL $profile: $a is missing" >&2
    rc=1
    continue
  fi

  exports=$("$NM" --defined-only "$a" 2>/dev/null | grep -cE ' T (ecvisor_|_ecv_|ecv_)' || true)
  if [ "$exports" -lt "$_MIN_EXPORTS" ]; then
    printf 'FAIL %-8s defines only %d exported entry points (want >= %d) -- is the archive real?\n' \
      "$profile" "$exports" "$_MIN_EXPORTS"
    rc=1
  fi

  foreign=0
  for other in $PROFILES; do
    [ "$other" = "$profile" ] && continue
    n=$("$NM" "$a" 2>/dev/null | grep -c "$(pat "$other")" || true)
    if [ "$n" -ne 0 ]; then
      printf 'FAIL %-8s also contains ecvisor::net::%s (%d symbols)\n' "$profile" "$other" "$n"
      foreign=1
      rc=1
    fi
  done
  [ "$foreign" -eq 0 ] && printf 'ok   %-8s exports=%-3d foreign-backends=0\n' "$profile" "$exports"
done

# ❗ THE POSITIVE CONTROL FOR THE PATTERN ITSELF, which this test did not have.
#
# Everything above is an ABSENCE. If `pat()` stopped matching -- a rename, a
# mangling change, a mistyped length prefix -- every assertion would pass
# against every archive forever, and the exports check would not notice because
# it looks at different symbols entirely. Worse, "loopback is not in the wasix
# archive" is unfalsifiable on its own terms: `net::loopback` leaves NO symbols
# anywhere (see note 2 in the header), so that particular exclusion reads as
# satisfied even by an archive that compiled loopback in alongside the real
# backend -- which is exactly what forgetting one of the two `any(...)` lists in
# `runtime/src/net/mod.rs` produces.
#
# So each backend that CALLS AN IMPORT must be found in its own archive. Those
# cannot inline away: an import is an opaque external symbol. `loopback` is
# deliberately not in this list and cannot be -- being invisible is its whole
# purpose.
for profile in wasmedge browser wasix; do
  a="${ARCHIVE[$profile]}"
  [ -f "$a" ] || continue
  n=$("$NM" "$a" 2>/dev/null | grep -c "$(pat "$profile")" || true)
  if [ "$n" -eq 0 ]; then
    printf 'FAIL %-8s does not contain ecvisor::net::%s -- its OWN backend\n' "$profile" "$profile"
    cat >&2 <<MSG

That makes every exclusion above vacuous for this archive: the pattern matches
nothing, so "backend X is absent" would pass whatever the archive contained.
Either the mangled path changed, or //runtime/$profile stopped enabling its
net-* feature, or the backend inlined away -- which it should not, because it
calls a host import. Fix the pattern or the target; do NOT delete this control.
MSG
    rc=1
  else
    printf 'ok   %-8s carries %d ecvisor::net::%s symbol(s) -- pattern is live\n' \
      "$profile" "$n" "$profile"
  fi
done

if [ "$rc" -ne 0 ]; then
  cat >&2 <<'MSG'

A profile carries a backend it should not, or does not look like a real archive.

Backend selection is a COMPILE-time decision (runtime/src/net/mod.rs): a `dyn`
trait object or an enum with a match would keep every backend live and put
every import set in the emitted module. If a `cfg` was loosened, this notices.
MSG
fi
exit "$rc"
