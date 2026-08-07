#!/usr/bin/env bash
# Does --config=hermetic produce the same thing as --config=image?
#
# ❌ THIS IS NOT A `bazel test`, and it cannot be. A Bazel test runs in ONE
# configuration, and //bazel:sdk.bzl resolves to ONE toolchain per configuration
# -- that is the whole point of it. Comparing the two means building twice, in
# two places, and only a script outside Bazel can do that.
#
# WHAT IT ANSWERS. "Equivalent" is three different questions and they have three
# different answers, so it checks each one separately rather than collapsing
# them:
#
#   1. Are the ARTIFACTS byte-identical?      -- for 6 of 10, yes. Not the tools.
#   2. Do the tools EMIT the same bitcode?    -- no, by 36 bytes.
#   3. Do those bitcodes COMPILE to the same  -- yes. This is the one that
#      object?                                  decides whether it matters.
#
# MEASURED 2026-08-23 on aarch64 (LLVM 16.0.6 both sides, wasi-sdk 24.0 both
# sides):
#
#   identical: 3 ecvisor archives, 2 C shims, the Go pipeline binary
#   differ:    the 4 LLVM tools -- and NOT by drift. Debian's `llvm-config
#              --libs` returns `-lLLVM-16`, one shared library, giving an 88 KB
#              tool; upstream's returns the static component list, giving a
#              13.7 MB tool. Different linkage model, same LLVM version.
#   bitcode:   differs by exactly 36 bytes at the tail -- the embedded LLVM
#              version string. Upstream carries the git hash
#              ("16.0.6 a0f5cfaf..."), Debian strips it to "16.0.6". The
#              disassembled IR is byte-identical once llvm-dis's ModuleID line
#              (which is just the path it read) is excluded.
#   object:    BYTE-IDENTICAL. The version string is not codegen input.
#
# WHAT THAT MEANS, and it is not "no difference":
#
#   ✅ No correctness hazard for the OBJECT cache. Both tools produce the same
#      .o, so an object cached under one is correct under the other.
#   ⚠️ The PARTITION cache misses across a switch. partcache.go keys a partition
#      on its bitcode BYTES, and those differ by the version string. Switching
#      modes costs a cold partition cache, not a wrong answer.
#   ❌ Hermetic tools built on a NEWER host cannot run IN the image. Measured:
#      `GLIBC_2.38 not found`, because this host's glibc is newer than the base
#      image's. So --config=hermetic is for host-side development and CI without
#      Docker; do NOT stage its output into the builder image unless the build
#      host's glibc is no newer than the image's.
set -euo pipefail

: "${IMAGE:=raptormark-elfconv-base:8bfe80860118}"
# Comparison artifacts go in .agents-workspace/tmp, per AGENTS.md: not /tmp.
: "${WORK:=$(cd "$(dirname "$0")/.." && pwd)/.agents-workspace/tmp/hermetic-diff}"

# ⚠️ Bazel's output base is the ONE thing that cannot live there. Bazel 9
# refuses an output base inside the main repo outright ("can cause spurious
# failures"), so this overrides the AGENTS.md placement rule for a reason the
# rule did not anticipate. Kept separate so a `.agents-workspace/tmp` wipe does
# not cost a 914 MB LLVM re-download.
: "${BZL_ROOT:=${TMPDIR:-/tmp}/raptormark-hermetic-bzl}"
BAZEL="${RAPTORMARK_BAZEL:-$(command -v bazelisk || command -v bazel)}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

[ -x "$BAZEL" ] || { echo "no bazel; set RAPTORMARK_BAZEL" >&2; exit 1; }

rm -rf "$WORK"
mkdir -p "$WORK"/{image,hermetic,lib,work}

echo "== building --config=hermetic on the host =="
"$BAZEL" --output_user_root="$BZL_ROOT" build --config=hermetic \
  --symlink_prefix=/ //builder:stage >/dev/null
cp -a "$BZL_ROOT"/*/execroot/_main/bazel-out/*/bin/builder/stage/. "$WORK/hermetic/"

echo "== building --config=image inside $IMAGE =="
(cd "$ROOT" && go run ./cmd/raptormark bazel --image "$IMAGE" build //builder:stage >/dev/null)
docker run --rm -v "$WORK:/w" -v raptormark-bazel-cache:/bzlcache "$IMAGE" \
  "set -e; S=\$(find /bzlcache -type d -path '*/bin/builder/stage' | head -1); \
   cp -a \$S/. /w/image/; chown -R $(id -u):$(id -g) /w/image" >/dev/null

echo
echo "── 1. artifacts ────────────────────────────────────────────────────────"
rc=0
(cd "$WORK/image" && find . -type f | sort) | while read -r f; do
  a=$(sha256sum "$WORK/image/$f" | cut -c1-16)
  b=$(sha256sum "$WORK/hermetic/$f" 2>/dev/null | cut -c1-16)
  printf '%-34s %-16s %-16s %s\n' "${f#./}" "$a" "$b" \
    "$([ "$a" = "$b" ] && echo IDENTICAL || echo DIFFERS)"
done

# The image-built tools link libLLVM-16.so.1 dynamically, so running them on the
# host needs that library. The hermetic ones link LLVM statically and need
# nothing.
cid=$(docker create "$IMAGE")
docker cp "$cid:/usr/lib/llvm-16/lib/libLLVM-16.so.1" "$WORK/lib/" 2>/dev/null ||
  docker cp "$cid:/usr/lib/aarch64-linux-gnu/libLLVM-16.so.1" "$WORK/lib/"
docker rm "$cid" >/dev/null

echo
echo "── 2. emitted bitcode ──────────────────────────────────────────────────"
cd "$WORK/work"
cat > t.c <<'EOF'
static int helper(int x) { return x * 3 + 1; }
int table[8] = {1,2,3,4,5,6,7,8};
int pick(int i) { switch (i) { case 0: return helper(1); case 1: return table[2];
  case 2: return helper(9); default: return -1; } }
int entry(int a, int b) { return pick(a) + helper(b); }
EOF
LLVM="$(echo "$BZL_ROOT"/*/external/+sdk+raptormark_sdk/llvm)"
WASI="$(echo "$BZL_ROOT"/*/external/+sdk+raptormark_sdk/wasi-sdk)"
"$LLVM/bin/clang" -O1 -emit-llvm -c t.c -o t.bc

# Both write to the SAME output path, so nothing path-derived can be mistaken
# for a real difference -- an earlier version of this compared files in
# differently-named directories and llvm-dis's ModuleID line made everything
# look different.
for m in image hermetic; do
  env LD_LIBRARY_PATH="$WORK/lib" "$WORK/$m/usr/local/bin/ecv-split" t.bc p 4 >/dev/null
  for f in p[0-9]*; do mv "$f" "${m}_${f}"; done
done
for p in $(ls image_p* | sed 's/^image_//'); do
  a=$(sha256sum "image_$p" | cut -c1-16); b=$(sha256sum "hermetic_$p" | cut -c1-16)
  printf 'bitcode %-6s %-16s %-16s %s\n' "$p" "$a" "$b" \
    "$([ "$a" = "$b" ] && echo IDENTICAL || echo DIFFERS)"
done

echo
echo "── 3. emitted object (the one that decides) ────────────────────────────"
for p in $(ls image_p* | sed 's/^image_//'); do
  for m in image hermetic; do
    # -x ir: the partitions have no extension, and without this clang silently
    # treats them as linker inputs, produces nothing, and two empty files
    # compare equal. That false pass happened here.
    "$WASI/bin/clang" -x ir -O3 --target=wasm32-wasip1 \
      --sysroot="$WASI/share/wasi-sysroot" -fPIC -c "${m}_${p}" -o "${m}_${p}.o" 2>/dev/null
  done
  if [ ! -s "image_${p}.o" ] || [ ! -s "hermetic_${p}.o" ]; then
    echo "object  $p: MISSING OUTPUT -- no conclusion"; rc=1; continue
  fi
  a=$(sha256sum "image_${p}.o" | cut -c1-16); b=$(sha256sum "hermetic_${p}.o" | cut -c1-16)
  printf 'object  %-6s %-16s %-16s %s\n' "$p" "$a" "$b" \
    "$([ "$a" = "$b" ] && echo IDENTICAL || echo DIFFERS)"
  [ "$a" = "$b" ] || rc=1
done

echo
echo "Objects identical => the two toolchains are equivalent for the cached"
echo "artifact. See this file's header for what still differs and what it costs."
exit "$rc"
