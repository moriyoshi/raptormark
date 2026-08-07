"""Build rule for the LLVM companion tools that used to be `RUN` lines.

# Why a genrule and not cc_binary

`cc_binary` would need a registered C++ toolchain for the LLVM in question, and
in `image` mode that toolchain is a compiler inside a container reached by
absolute path. Standing one up would mean describing LLVM's own include and
library layout to Bazel -- and the tools do not link a fixed library list, they
link whatever `llvm-config --libs <components>` resolves to, which differs
between a shared and a static LLVM build and between LLVM versions.

Running `llvm-config` is therefore not a shortcut around Bazel; it is the actual
build recipe, and the Dockerfile ran it for the same reason. Keeping it means
the flags that reach the compiler are IDENTICAL to the ones that reached it
before this moved out of the Dockerfile -- which is the property that makes the
move safe to make without re-validating every translated object.

What Bazel adds over the `RUN` line is real and is the point of the exercise:
the sources are declared inputs, so editing `ecv-namespace.h` rebuilds both
tools that include it; the tools are addressable targets rather than side
effects of an image build; and the image no longer has to be rebuilt to rebuild
a tool.
"""

load("@raptormark_sdk//:defs.bzl", "LLVM_PREFIX", "SDK_STAMP", "SYSLIB_DIR")

# The flags the Dockerfile passed, verbatim. -fno-rtti is not optional: LLVM is
# built without RTTI, so a tool compiled with it fails to link against it.
#
# ⚠️ `--system-libs` is the ONE addition to the recipe, and it is provably inert
# on the image path. The Dockerfile ran `llvm-config --cxxflags --ldflags
# --libs`; that is incomplete for static linking and got away with it because
# Debian's LLVM 16 reports NO system libs at all. The official 16.0.6 build
# reports `-lrt -ldl -lpthread -lm -lz -ltinfo`, and without them the link fails
# on terminfo symbols reached from `Process::FileDescriptorHasColors`.
#
# Both were checked directly (2026-08-23): the image's `llvm-config
# --system-libs` prints an empty line. So image mode appends nothing, and
# //builder:tools_equivalence_test -- which still compares against the ORIGINAL
# Dockerfile recipe -- is what proves the addition changed no bytes there.
_CXXFLAGS = ["-std=c++17", "-fno-rtti", "-O2"]

def llvm_tool(name, src, components, hdrs = [], **kwargs):
    """One LLVM companion tool.

    Args:
      name: target name, and the binary's name in the image.
      src: the single .cpp.
      components: llvm-config component names, in the order the Dockerfile had
        them. The order is preserved because `llvm-config --libs` emits link
        order, and while modern linkers are forgiving about it, a reordering
        here would be an unreviewed change to a link line nobody re-tested.
      hdrs: local headers the source includes. Declared so a header edit
        rebuilds the tool -- the thing the Dockerfile could not do, since a
        `COPY` of a changed header invalidated the layer only by accident of
        ordering.
      **kwargs: forwarded to the genrule (visibility, tags).
    """
    # See //bazel:sdk.bzl: EMPTY in image mode, so that command line is exactly
    # what builder/Dockerfile ran. In hermetic mode it points at a `libtinfo.so`
    # symlink the repo rule made, because the official LLVM build asks for
    # `-ltinfo` and the distro one does not.
    syslib_flag = ("-L" + SYSLIB_DIR) if SYSLIB_DIR else ""

    include_flag = ""
    if hdrs:
        # The headers are siblings of the source in the execroot; -I their
        # directory rather than assuming the package path, so this keeps working
        # under any output layout.
        include_flag = "-I$$(dirname $(execpath {}))".format(hdrs[0])

    native.genrule(
        name = name,
        srcs = [src] + hdrs,
        # Under bin/ so the rule name and the output name do not collide --
        # Bazel warns "target is both a rule and a file" otherwise. The BASENAME
        # is what matters, because it is the name the tool has in the image and
        # the name translate-one invokes it by.
        outs = ["bin/" + name],
        # SDK_STAMP is inert in the command but present in it, which is what
        # makes an image-vs-hermetic switch re-run this action instead of
        # serving the other toolchain's binary from cache.
        cmd = """
set -euo pipefail
: 'sdk: {stamp}'
LLVM_CONFIG={prefix}/bin/llvm-config
"{prefix}/bin/clang++" {cxxflags} {include_flag} $(execpath {src}) {syslib_flag} \\
  $$("$$LLVM_CONFIG" --cxxflags --ldflags --libs --system-libs {components}) \\
  -o $@
""".format(
            stamp = SDK_STAMP,
            prefix = LLVM_PREFIX,
            cxxflags = " ".join(_CXXFLAGS),
            include_flag = include_flag,
            syslib_flag = syslib_flag,
            src = src,
            components = " ".join(components),
        ),
        # The toolchain is outside the workspace in image mode, so this action
        # is not hermetic in Bazel's sense and must not be shipped to a remote
        # executor that lacks the image.
        tags = ["local", "no-remote"],
        **kwargs
    )
