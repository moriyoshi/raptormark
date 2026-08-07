#!/usr/bin/env bash
# The two C shims, built the way builder/Dockerfile built them, compared
# byte-for-byte against the way //runtime builds them now.
#
# Same role as //builder:tools_equivalence_test, and the same reasoning: the
# reference below is a TRANSCRIPTION of the deleted `RUN` line, so a drift in
# //bazel:wasi_object.bzl fails here rather than surfacing as a linked module
# that behaves subtly differently.
#
# These objects are linked into every emitted module, and `--target` and
# `--sysroot` are exactly the flags whose loss would still produce an object --
# just one for the wrong target.
set -euo pipefail

R="${TEST_SRCDIR}/_main"
WASI="${WASI_SDK_PATH:?WASI_SDK_PATH must be set by the BUILD rule}"

# ❌ NOT a skip -- see the same note in //builder:tools_equivalence_test. A
# missing compiler must not be indistinguishable from a pass.
if [ ! -x "$WASI/bin/clang" ]; then
  echo "$WASI/bin/clang is not here, but the SDK repo rule said it was." >&2
  exit 1
fi

ref="$(mktemp -d)"
trap 'rm -rf "$ref"' EXIT

rc=0
for s in ecv_sp ecv_globals; do
  "$WASI/bin/clang" -O2 --target=wasm32-wasip1 \
    --sysroot="$WASI/share/wasi-sysroot" \
    -c "$R/runtime/cshim/$s.c" -o "$ref/$s.o"
  want="$(sha256sum "$ref/$s.o" | cut -d' ' -f1)"
  got="$(sha256sum "$R/runtime/obj/$s.o" | cut -d' ' -f1)"
  if [ "$want" = "$got" ]; then
    printf 'ok   %-12s %s\n' "$s" "${got:0:16}"
  else
    rc=1
    printf 'FAIL %-12s dockerfile=%s bazel=%s\n' "$s" "${want:0:16}" "${got:0:16}"
  fi
done

if [ "$rc" -ne 0 ]; then
  echo "" >&2
  echo "A C shim no longer matches the recipe builder/Dockerfile used. These link" >&2
  echo "into every emitted module; see //bazel:wasi_object.bzl for the flags." >&2
fi
exit "$rc"
