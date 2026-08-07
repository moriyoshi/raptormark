"""ecvisor's two crate dependencies, fetched by hash rather than by cargo.

`runtime/Cargo.toml` has exactly one direct dependency, `miniz_oxide`, which
brings exactly one transitive one, `adler2`. Both are pure Rust with no build
scripts, which is why hand-writing them is reasonable here and why
`crate_universe` would be more machinery than the problem needs -- it would run
cargo at fetch time to resolve a graph with two nodes in it.

THE HASHES BELOW ARE THE ONES IN `runtime/Cargo.lock`. crates.io's `checksum`
field is the sha256 of the `.crate` file, so these are not a second source of
truth to keep in sync -- they are the same numbers, and
`//runtime:cargo_lock_agrees_test` fails if they ever stop being.

What this buys over the Dockerfile: that build ran `cargo build --locked` with
network access, so it needed the network but could not drift. This needs the
network only at fetch time, verifies both archives against a recorded hash, and
caches them.
"""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

_BUILD_ADLER2 = """
load("@rules_rust//rust:defs.bzl", "rust_library")

# default-features = false: miniz_oxide asks for adler2 without `std`, so this
# is a no_std build. Enabling `std` here would compile a different crate than
# the one Cargo.lock describes.
rust_library(
    name = "adler2",
    srcs = glob(["src/**/*.rs"]),
    edition = "2021",
    visibility = ["//visibility:public"],
)
"""

_BUILD_MINIZ_OXIDE = """
load("@rules_rust//rust:defs.bzl", "rust_library")

# `with-alloc` and nothing else, matching runtime/Cargo.toml's
# default-features = false, features = ["with-alloc"].
rust_library(
    name = "miniz_oxide",
    srcs = glob(["src/**/*.rs"]),
    crate_features = ["with-alloc"],
    edition = "2021",
    visibility = ["//visibility:public"],
    deps = ["@crate_adler2//:adler2"],
)
"""

_CRATES = {
    "crate_adler2": struct(
        name = "adler2",
        version = "2.0.1",
        sha256 = "320119579fcad9c21884f5c4861d16174d0e06250625266f50fe6898340abefa",
        build = _BUILD_ADLER2,
    ),
    "crate_miniz_oxide": struct(
        name = "miniz_oxide",
        version = "0.8.9",
        sha256 = "1fa76a2c86f704bdb222d66965fb3d63269ce38518b83cb0575fca855ebb6316",
        build = _BUILD_MINIZ_OXIDE,
    ),
}

def _crates_impl(_ctx):
    for repo, c in _CRATES.items():
        http_archive(
            name = repo,
            urls = ["https://static.crates.io/crates/{n}/{n}-{v}.crate".format(n = c.name, v = c.version)],
            sha256 = c.sha256,
            strip_prefix = "{n}-{v}".format(n = c.name, v = c.version),
            type = "tar.gz",
            build_file_content = c.build,
        )

crates = module_extension(implementation = _crates_impl)

# Exposed so a test can assert these agree with runtime/Cargo.lock rather than
# trusting that whoever bumped one remembered the other.
CRATE_CHECKSUMS = {c.name + " " + c.version: c.sha256 for c in _CRATES.values()}
