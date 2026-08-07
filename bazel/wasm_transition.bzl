"""Build the ecvisor archives for wasm32-wasip1 without forcing the whole build.

`--platforms=//bazel:wasm32_wasip1` on the command line works, and it was the
first thing tried, but it is wrong in a way that shows up immediately: it
retargets EVERYTHING, so the LLVM companion tools (host binaries) and the
`sh_test`s (which need a shell toolchain) stop resolving. `bazel build //...`
has to work, and it cannot if the correct invocation differs per target.

So the platform is a property of the TARGET instead. `wasm_artifacts` wraps
labels in a transition to wasm32-wasip1 and re-exports their files at the
default (host) platform, which is what lets a host-platform `sh_test` or an
`oci_image` depend on a wasm archive without either of them knowing.
"""

def _to_wasm(_settings, _attr):
    return {"//command_line_option:platforms": "//bazel:wasm32_wasip1"}

_wasm_transition = transition(
    implementation = _to_wasm,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _wasm_artifacts_impl(ctx):
    return [DefaultInfo(files = depset(
        transitive = [src[DefaultInfo].files for src in ctx.attr.srcs],
    ))]

wasm_artifacts = rule(
    implementation = _wasm_artifacts_impl,
    attrs = {
        "srcs": attr.label_list(
            cfg = _wasm_transition,
            doc = "Targets to build for wasm32-wasip1.",
        ),
    },
    doc = "Re-exports its srcs' files, built for wasm32-wasip1.",
)
