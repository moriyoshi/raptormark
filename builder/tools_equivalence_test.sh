#!/usr/bin/env bash
# The four LLVM companion tools, built the way builder/Dockerfile built them,
# compared byte-for-byte against the way //builder builds them now.
#
# WHY THIS EXISTS. Until 2026-08-23 these tools were `RUN` lines in
# builder/Dockerfile. Moving a compile recipe is exactly the kind of change that
# looks free and is not: a dropped `-fno-rtti` still links (until it doesn't), a
# reordered `llvm-config` component list still runs, and a changed `-O` level
# still produces a working tool. None of that would fail a build. What it would
# do is silently change the bytes of every object the pipeline emits, under a
# `raptormark.translate_sh` that says the pipeline is unchanged.
#
# So the reference recipe below is a TRANSCRIPTION of those `RUN` lines, kept
# after the lines themselves were deleted -- the same role
# `TestLiftArgsMatchTheScript` plays for the translate-one shell script it
# replaced. If //bazel:llvm_tool.bzl ever drifts from it, this fails.
#
# ⚠️ WHAT A FALSE PASS WOULD LOOK LIKE, since that is the question this project
# asks of every check: if the reference compile silently failed and produced no
# binary, `sha256sum` would error and `set -o pipefail` would fail the test. If
# it produced a binary from the WRONG source, the hashes would differ and the
# test would fail. The dangerous case is the reference drifting to match a
# changed rule -- which is why this file transcribes the Dockerfile and is not
# generated from the rule it checks.
#
# Runs only in `image` mode, since it needs the LLVM the tools link against.
set -euo pipefail

R="${TEST_SRCDIR}/_main"
LLVM="${LLVM_PREFIX:?LLVM_PREFIX must be set by the BUILD rule}"
CXX="${LLVM}/bin/clang++"
LLVM_CONFIG="${LLVM}/bin/llvm-config"

# ❌ NOT a skip. An earlier draft exited 0 here "because this test only runs in
# image mode", which makes a missing compiler indistinguishable from a pass --
# the exact false-pass shape this project checks every guard for. The SDK repo
# rule (//bazel:sdk.bzl) already fails at fetch time if the toolchain is absent,
# so reaching this line at all means something is wrong.
if [ ! -x "$CXX" ]; then
  echo "$CXX is not here, but the SDK repo rule said it was. Nothing was compared." >&2
  exit 1
fi

ref="$(mktemp -d)"
trap 'rm -rf "$ref"' EXIT

# The headers go beside the sources, which is what -I<dir> then resolves --
# the Dockerfile achieved the same thing by copying everything into /tmp.
cp "$R/builder"/*.cpp "$R/builder"/*.h "$ref/"

# ── The transcribed RUN lines. Flags and component ORDER are both load-bearing.
build_ref() {
  local name="$1"; shift
  local dash_i="$1"; shift
  # shellcheck disable=SC2086  # llvm-config output is intentionally word-split
  "$CXX" -std=c++17 -fno-rtti -O2 $dash_i "$ref/$name.cpp" \
    $("$LLVM_CONFIG" --cxxflags --ldflags --libs "$@") \
    -o "$ref/$name"
}

build_ref ecv-promote      ""          core irreader support bitwriter
build_ref ecv-split        "-I$ref"    core irreader support bitwriter transformutils
build_ref namespace-object "-I$ref"    core irreader support bitwriter
build_ref ecv-prepare      "-I$ref"    core irreader support bitwriter linker ipo passes transformutils

rc=0
for t in ecv-promote ecv-split namespace-object ecv-prepare; do
  want="$(sha256sum "$ref/$t" | cut -d' ' -f1)"
  got="$(sha256sum "$R/builder/bin/$t" | cut -d' ' -f1)"
  if [ "$want" = "$got" ]; then
    printf 'ok   %-18s %s\n' "$t" "${got:0:16}"
  else
    rc=1
    printf 'FAIL %-18s dockerfile=%s bazel=%s\n' "$t" "${want:0:16}" "${got:0:16}"
  fi
done

if [ "$rc" -ne 0 ]; then
  cat >&2 <<'MSG'

A tool //builder builds no longer matches the recipe builder/Dockerfile used.

That is a change to the BYTES the translation pipeline emits. If it is
intended, it belongs in the same change as a bump of the reference recipe in
this file -- and be aware that every translated object cached under the old
`raptormark.translate_sh` is now stale, which is hours of CPU to rebuild.

If it is NOT intended, look at //bazel:llvm_tool.bzl first: the compile flags
and the llvm-config component lists live there.
MSG
fi
exit "$rc"
