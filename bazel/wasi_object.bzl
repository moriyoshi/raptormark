"""The wasi-sdk C shims, moved here from a `RUN` line in builder/Dockerfile.

These two objects exist because they express things at the WASM level that Rust
cannot state, so they cannot live in `libecvisor.a`:

  ecv_sp.c       the shadow-stack-pointer probe.
  ecv_globals.c  the wasm globals lifted code reads inline instead of calling
                 into ecvisor -- `__ecv_unwinding` replaces 33,540
                 `_ecv_suspended` calls per bash-sized module with one
                 `global.get` each.

Neither adds a wasm proposal beyond Wasm 2.0: `mutable-globals` is inside it and
the module already required it for `__stack_pointer`.

Like //bazel:llvm_tool.bzl this shells out to the SDK compiler rather than
standing up a `cc_toolchain`. The reason is the same and slightly stronger here:
there is exactly one compile of exactly one translation unit per object, with no
link step and no library resolution, so a toolchain definition would be pure
ceremony around a single command whose flags must not change.
"""

load("@raptormark_sdk//:defs.bzl", "SDK_STAMP", "WASI_SDK_PATH")

def wasi_object(name, src, **kwargs):
    """Compile one .c to a wasm32-wasip1 relocatable object."""
    native.genrule(
        name = name,
        srcs = [src],
        outs = ["obj/" + name + ".o"],
        cmd = """
set -euo pipefail
: 'sdk: {stamp}'
"{wasi}/bin/clang" -O2 --target=wasm32-wasip1 \\
  --sysroot="{wasi}/share/wasi-sysroot" \\
  -c $(execpath {src}) -o $@
""".format(
            stamp = SDK_STAMP,
            wasi = WASI_SDK_PATH,
            src = src,
        ),
        tags = ["local", "no-remote"],
        **kwargs
    )
