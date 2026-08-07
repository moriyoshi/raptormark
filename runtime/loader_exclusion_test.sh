#!/usr/bin/env bash
# The DEFAULT ecvisor archive contains no loader backend that adds a host import.
#
# WHY THIS MATTERS MORE THAN THE NETWORK EQUIVALENT. The three network backends
# all answer the same calls, so picking the wrong one is a behaviour bug. The
# loader backends differ in whether the archive carries an undefined symbol:
#
#     U _ZN7ecvisor6loader6hosted14host_load_side...E
#
# `wasm-ld` turns every undefined symbol reachable from live code into an
# IMPORT, and that one becomes `env.ecv_host_load_side`. No stock runwasi shim
# supplies it, so a default-profile module built with `load-hosted` linked in
# would fail to INSTANTIATE before running a single instruction -- at deploy
# time, with nothing pointing back at a feature flag. See runtime/src/loader/mod.rs.
#
# ⚠️ WHAT THIS DOES NOT CHECK, the same caveat profile_exclusion_test.sh carries:
# it reads an ARCHIVE, not a linked module. Only the final `wasm-ld` decides what
# becomes an import. This proves "the hosted backend is not in the default
# archive" -- a precondition for "the shipping module imports no loader", not
# that claim. `e2e/imports_test.go` pins the linked module's 28 imports.
#
# ⚠️ AND WHY THE PATTERN MATCHES A MODULE PATH RATHER THAN THE IMPORT NAME.
# Grepping for `ecv_host_load_side` finds NOTHING in either archive: the Rust
# symbol is `ecvisor::loader::hosted::host_load_side` and `#[link_name]` renames
# it only at link time. Measured, both archives report 0 for that string. The
# same trap is recorded in profile_exclusion_test.sh for `sock_*`.
set -euo pipefail

R="${TEST_SRCDIR}/_main"
NM="${LLVM_PREFIX:?LLVM_PREFIX must be set by the BUILD rule}/bin/llvm-nm"

if [ ! -x "$NM" ]; then
  echo "$NM is not here, but the SDK repo rule said it was." >&2
  exit 1
fi

DEFAULT="$R/runtime/libecvisor.a"
HOSTED="$R/runtime/hosted/libecvisor.a"
# The ATTRIBUTION control. `//runtime/hosted` is `net-loopback` + `load-hosted`,
# so it differs from DEFAULT in two ways and a symbol found in one and not the
# other could in principle be the net backend's. LOOPBACK is `net-loopback` with
# NO loader feature, so hosted-minus-loopback isolates the loader exactly.
#
# ⚠️ THAT ARGUMENT NO LONGER COVERS `wasix` THE SAME WAY. It was `net-loopback`
# + `load-wasix` until WASIX sockets landed; it is now `net-wasix` +
# `load-wasix`, so it shares a net backend with nothing here. What still holds
# it up is that `pat()` names the `6loader` path component, which cannot match
# `ecvisor::net::wasix` however similar the two read. That is asserted below
# rather than left as a remark -- an argument nobody checks is an argument that
# quietly stops being true.
LOOPBACK="$R/runtime/loopback/libecvisor.a"
WASIX="$R/runtime/wasix/libecvisor.a"

# Rust mangling for `ecvisor::loader::<backend>`: length-prefixed path segments.
pat() { printf '7ecvisor6loader%d%s' "${#1}" "$1"; }

# Guards the empty-archive false pass: an archive with nothing in it satisfies
# every absence assertion below. Measured 2026-08-23: 16 in both archives, so 10
# is a floor with room rather than a fitted number.
_MIN_EXPORTS=10

rc=0
for name in default hosted loopback wasix; do
  a="$DEFAULT"
  [ "$name" = hosted ] && a="$HOSTED"
  [ "$name" = loopback ] && a="$LOOPBACK"
  [ "$name" = wasix ] && a="$WASIX"
  if [ ! -f "$a" ]; then
    echo "FAIL $name: $a is missing" >&2
    rc=1
    continue
  fi
  exports=$("$NM" --defined-only "$a" 2>/dev/null | grep -cE ' T (ecvisor_|_ecv_|ecv_)' || true)
  if [ "$exports" -lt "$_MIN_EXPORTS" ]; then
    printf 'FAIL %-8s defines only %d exported entry points (want >= %d) -- is the archive real?\n' \
      "$name" "$exports" "$_MIN_EXPORTS"
    rc=1
  fi
done

# THE assertion: the shipping archive carries no `hosted` backend.
n=$("$NM" "$DEFAULT" 2>/dev/null | grep -c "$(pat hosted)" || true)
if [ "$n" -ne 0 ]; then
  printf 'FAIL default archive contains ecvisor::loader::hosted (%d symbols)\n' "$n"
  rc=1
else
  printf 'ok   default carries no ecvisor::loader::hosted\n'
fi

# THE POSITIVE CONTROL, and without it the line above is worthless: if the
# pattern stopped matching -- a rename, a mangling change, a backend that
# inlines away -- the assertion would pass against every archive forever.
#
# `hosted` is checkable this way and `preloaded` is NOT: `Preloaded::request`
# returns a constant, so at -Copt-level=3 -Clto=fat it inlines into its callers
# and leaves no symbol at all. That is exactly what profile_exclusion_test.sh
# found for `net::loopback`. `hosted` survives because it calls an import, which
# cannot be inlined away.
m=$("$NM" "$HOSTED" 2>/dev/null | grep -c "$(pat hosted)" || true)
if [ "$m" -eq 0 ]; then
  cat >&2 <<'MSG'
FAIL the hosted archive does not contain ecvisor::loader::hosted either.

That makes the assertion above vacuous: it would pass for any archive, forever.
Either the mangled path changed, or //runtime/hosted stopped enabling
`load-hosted`, or the backend now inlines away. Fix the pattern or the target --
do NOT delete this control.
MSG
  rc=1
else
  printf 'ok   hosted carries %d ecvisor::loader::hosted symbol(s) -- pattern is live\n' "$m"
fi

# THE ATTRIBUTION ASSERTION: loopback carries the same net backend as hosted and
# no loader feature, so if IT also had `ecvisor::loader::hosted` the difference
# above would not be the loader's doing. Without this, switching hosted's net
# backend could silently change what the control proves.
l=$("$NM" "$LOOPBACK" 2>/dev/null | grep -c "$(pat hosted)" || true)
if [ "$l" -ne 0 ]; then
  printf 'FAIL loopback (net-loopback, no loader feature) contains ecvisor::loader::hosted (%d)\n' "$l"
  rc=1
else
  printf 'ok   loopback carries no ecvisor::loader::hosted -- the difference is the LOADER feature, not the net backend\n'
fi

# THE WASIX BACKEND, same rules. Its import is `wasix_32v1.dlopen`, which only
# wasmer supplies -- a shipping module carrying it fails to instantiate on every
# other engine, so the exclusion matters exactly as much as `hosted`'s.
w=$("$NM" "$DEFAULT" 2>/dev/null | grep -c "$(pat wasix)" || true)
if [ "$w" -ne 0 ]; then
  printf 'FAIL default archive contains ecvisor::loader::wasix (%d symbols)\n' "$w"
  rc=1
else
  printf 'ok   default carries no ecvisor::loader::wasix\n'
fi

# Its positive control, for the same reason `hosted` has one: without it the
# line above passes against any archive forever.
wc_=$("$NM" "$WASIX" 2>/dev/null | grep -c "$(pat wasix)" || true)
if [ "$wc_" -eq 0 ]; then
  cat >&2 <<'MSG'
FAIL the wasix archive does not contain ecvisor::loader::wasix either.

That makes the assertion above vacuous. Either the mangled path changed, or
//runtime/wasix stopped enabling `load-wasix`, or the backend inlined away --
which it should not, because it calls an import.
MSG
  rc=1
else
  printf 'ok   wasix carries %d ecvisor::loader::wasix symbol(s) -- pattern is live\n' "$wc_"
fi

# THE WASIX ATTRIBUTION ASSERTION, and it is doing more work than hosted's.
#
# `//runtime/wasix` no longer shares a net backend with any archive here, so the
# only thing separating `ecvisor::loader::wasix` from `ecvisor::net::wasix` is
# the `6loader` component of the pattern. If that ever stopped discriminating --
# a rename, a re-export, a pattern edited to `5wasix` -- the positive control
# above would start passing for the NET backend and the exclusion would mean
# nothing. LOOPBACK carries neither, so a non-zero here says the pattern has
# gone loose.
lw=$("$NM" "$LOOPBACK" 2>/dev/null | grep -c "$(pat wasix)" || true)
if [ "$lw" -ne 0 ]; then
  printf 'FAIL loopback (no loader feature) contains ecvisor::loader::wasix (%d)\n' "$lw"
  rc=1
else
  printf 'ok   loopback carries no ecvisor::loader::wasix -- the difference is the LOADER feature\n'
fi

# And the net-side half of the same worry, stated directly: the DEFAULT archive
# must carry neither `ecvisor::loader::wasix` (asserted above) nor
# `ecvisor::net::wasix`. The second is profile_exclusion_test's subject, but the
# two names are one typo apart and this is where someone reading about `wasix`
# exclusion will look.
nw=$("$NM" "$DEFAULT" 2>/dev/null | grep -c '_ZN7ecvisor3net5wasix' || true)
if [ "$nw" -ne 0 ]; then
  printf 'FAIL default archive contains ecvisor::net::wasix (%d symbols)\n' "$nw"
  rc=1
else
  printf 'ok   default carries no ecvisor::net::wasix either\n'
fi

# And the two LOADER archives must not contain each other's backend: they are
# alternatives, and a build with both would import from two engines at once.
x=$("$NM" "$HOSTED" 2>/dev/null | grep -c "$(pat wasix)" || true)
y=$("$NM" "$WASIX" 2>/dev/null | grep -c "$(pat hosted)" || true)
if [ "$x" -ne 0 ] || [ "$y" -ne 0 ]; then
  printf 'FAIL the loader archives overlap: hosted has %d wasix symbols, wasix has %d hosted\n' "$x" "$y"
  rc=1
else
  printf 'ok   hosted and wasix carry only their own backend\n'
fi

if [ "$rc" -ne 0 ]; then
  cat >&2 <<'MSG'

The shipping archive carries a loader backend it should not, or an archive does
not look real.

Loader selection is a COMPILE-time decision (runtime/src/loader/mod.rs): a `dyn`
trait object or an enum with a match would keep every backend live and put every
backend's imports in the module. If a `cfg` was loosened, or a feature leaked
into the default build, this notices.
MSG
fi
exit "$rc"
