#!/usr/bin/env bash
# `bazel/crates.bzl`'s crate hashes are the same numbers as `runtime/Cargo.lock`'s.
#
# # What this checks and why it has to exist
#
# `crates.bzl` fetches ecvisor's two crate dependencies by URL and sha256 instead
# of running cargo. Its own docstring says the hashes "are not a second source of
# truth to keep in sync -- they are the same numbers, and
# `//runtime:cargo_lock_agrees_test` fails if they ever stop being."
#
# ❗ THAT TEST DID NOT EXIST until 2026-08-27. The claim was written in the
# present tense and the scaffolding for it was already there -- `crates.bzl`
# exports `CRATE_CHECKSUMS` with the comment "Exposed so a test can assert these
# agree with runtime/Cargo.lock rather than trusting that whoever bumped one
# remembered the other" -- and nothing consumed it. So the hashes WERE a second
# source of truth, guarded by a sentence.
#
# # What drift costs
#
# `cargo` reads `Cargo.lock`; Bazel reads `crates.bzl`. A `cargo update` moves
# one and not the other, and then the Rust gate
# (`cargo test --manifest-path runtime/Cargo.toml`) tests one version of
# miniz_oxide while the SHIPPED archive that `//runtime:profiles` builds contains
# a different one. Both stay green. That is the "two gates where one of them
# lies" failure `AGENTS.md` warns about, and nothing else in the tree looks at
# both files.
#
# # How to neutralize it
#
# Change either side and re-run -- the sha256 in `bazel/crates.bzl`, or the
# `checksum` line in `runtime/Cargo.lock`. It must name the crate and print both
# values. Bumping a VERSION on one side only must fail too, which is a separate
# arm: a version present in one file and absent from the other.
set -euo pipefail

lock=${1:?usage: cargo_lock_agrees_test.sh <Cargo.lock> <crate_checksums.txt>}
declared=${2:?usage: cargo_lock_agrees_test.sh <Cargo.lock> <crate_checksums.txt>}

[ -r "$lock" ] || { echo "FAIL cannot read $lock" >&2; exit 1; }
[ -r "$declared" ] || { echo "FAIL cannot read $declared" >&2; exit 1; }

# Cargo.lock's `[[package]]` stanzas, as "name version checksum".
#
# ⚠️ Only the ones WITH a checksum. The `ecvisor` package is the local crate and
# has none; including it would compare a path dependency against a registry hash.
lock_triples=$(awk '
    /^\[\[package\]\]/      { name=""; version=""; checksum=""; next }
    /^name = /              { gsub(/[",]/, "", $3); name=$3; next }
    /^version = /           { gsub(/[",]/, "", $3); version=$3; next }
    /^checksum = /          { gsub(/[",]/, "", $3); print name, version, $3 }
' "$lock" | sort)

# ❗ POSITIVE CONTROL, and this test is worthless without it. Every assertion
# below is a comparison between two extracted sets, and two EMPTY sets compare
# equal. An awk pattern that stops matching -- a Cargo.lock format change, a
# renamed field -- would otherwise turn this into a test that passes forever
# while checking nothing. `AGENTS.md`: a scan must FATAL when its pattern matches
# nothing.
if [ -z "$lock_triples" ]; then
    echo "FAIL extracted ZERO checksummed packages from $lock." >&2
    echo "     The awk patterns above no longer match Cargo.lock's format, so this" >&2
    echo "     test would compare two empty sets and pass while checking nothing." >&2
    exit 1
fi
declared_sorted=$(sort "$declared")
if [ -z "$declared_sorted" ]; then
    echo "FAIL $declared is empty; CRATE_CHECKSUMS produced no entries." >&2
    exit 1
fi

if [ "$lock_triples" = "$declared_sorted" ]; then
    n=$(printf '%s\n' "$lock_triples" | wc -l | tr -d ' ')
    echo "ok: $n crate(s) agree between Cargo.lock and bazel/crates.bzl"
    printf '%s\n' "$lock_triples" | sed 's/^/  /'
    exit 0
fi

echo "FAIL bazel/crates.bzl and runtime/Cargo.lock disagree." >&2
echo >&2
echo "  Cargo.lock says (what cargo builds):" >&2
printf '%s\n' "$lock_triples" | sed 's/^/    /' >&2
echo "  bazel/crates.bzl says (what the SHIPPED archive is built from):" >&2
printf '%s\n' "$declared_sorted" | sed 's/^/    /' >&2
echo >&2
echo "  These must be the same numbers. crates.io's \`checksum\` field IS the" >&2
echo "  sha256 of the .crate file, so a difference means one side was bumped" >&2
echo "  and the other was not -- and then \`cargo test\` and the archive that" >&2
echo "  ships are built from different crate versions, both green." >&2
exit 1
