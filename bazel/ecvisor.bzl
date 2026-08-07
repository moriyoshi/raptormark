"""One ecvisor archive, for one network backend.

The three profiles differ ONLY in which `net-*` feature is on. They are three
separate archives rather than one archive with a switch because `wasm-ld` emits
an import for every undefined symbol reachable from live code: a `dyn` trait
object or an enum with a match would keep both backends live and put both import
sets in the emitted module. The selection has to happen at compile time or it
does not happen at all. See `runtime/src/net/mod.rs`.

They live in three PACKAGES because all three must produce `libecvisor.a`. The
crate name is `ecvisor` in every profile -- it was under cargo, and renaming it
would change symbol mangling -- and three targets writing one filename into one
package is a conflicting-action error. Separate packages give separate output
directories and keep the filename the link expects.
"""

load("@rules_rust//rust:defs.bzl", "rust_static_library")
load("//bazel:wasm_transition.bzl", "wasm_artifacts")

# `[profile.release]` from runtime/Cargo.toml, which rules_rust does not read.
#
# ⚠️ Not defaults and not decoration. `panic = "abort"` is load-bearing: ecvisor
# is linked into a module that must stay inside Wasm 2.0, and an unwinding panic
# runtime is exactly the kind of thing that reaches beyond it. If you change one
# of these, change it in Cargo.toml too -- `cargo test` on the host still reads
# that file, so the two builds would otherwise disagree about what they test.
#
# `-Clto` is NOT here. rules_rust compiles with `-Cembed-bitcode=no` by default,
# which rustc rejects alongside `-Clto`, and setting it per-target would leave
# miniz_oxide and adler2 without bitcode to link anyway. The
# `@rules_rust//rust/settings:lto` build setting reaches every crate in the
# graph; it is set in .bazelrc.
RELEASE_PROFILE = [
    "-Copt-level=3",
    "-Ccodegen-units=1",
    "-Cpanic=abort",
]

def ecvisor_profile(name = "ecvisor", feature = None, extra_features = [], srcs = "//runtime:srcs", crate_root = "//runtime:src/lib.rs"):
    """An ecvisor staticlib with exactly one network backend compiled in.

    Args:
      name: target name. Always produces `libecvisor.a`.
      feature: the `net-*` feature, e.g. "net-loopback".
      extra_features: further crate features, e.g. a `load-*` loader backend.
        Kept SEPARATE from `feature` rather than folding both into one list, so
        the "exactly one network backend" check below still means something --
        a list would let a caller pass two `net-*` features and pass the check.
      srcs: the shared source filegroup.
      crate_root: lib.rs, which rules_rust cannot infer across a package
        boundary.
    """
    if not feature:
        fail("ecvisor_profile needs a net-* feature; there is no valid default " +
             "because the backend must be chosen at compile time.")
    if not feature.startswith("net-"):
        fail("ecvisor_profile's `feature` is the NETWORK backend and must be a " +
             "net-* feature; put a loader backend in `extra_features`. Got: " + feature)
    for f in extra_features:
        if f.startswith("net-"):
            fail("a second net-* feature in `extra_features` would defeat the " +
                 "one-backend rule the archive exists to guarantee. Got: " + f)

    rust_static_library(
        name = name,
        srcs = [srcs],
        crate_features = [feature] + extra_features,
        crate_name = "ecvisor",
        crate_root = crate_root,
        edition = "2021",
        rustc_flags = RELEASE_PROFILE,
        visibility = ["//visibility:public"],
        deps = ["@crate_miniz_oxide//:miniz_oxide"],
    )

    # The same archive, reachable from a HOST-platform consumer. The image
    # staging rule and the tests are host-platform targets; depending on the
    # rust target directly would pull it into the host configuration and
    # produce an aarch64 archive, silently.
    wasm_artifacts(
        name = name + "_wasm",
        srcs = [":" + name],
        visibility = ["//visibility:public"],
    )
