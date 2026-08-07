"""The LLVM 16 and wasi-sdk toolchains, from either of two sources.

raptormark's companion tools cannot be built against just any LLVM. `ecv-split`,
`ecv-prepare` and `namespace-object` read the bitcode `elflift` writes and write
bitcode that the same image's `clang` consumes, so the LLVM they link against is
part of the pipeline's identity, not an implementation detail. That is why these
recipes lived in `builder/Dockerfile` in the first place: the toolchain was
already in the image, and a `RUN` line is the shortest way to reach it.

Two sources are supported, because they answer different questions:

  image     (default) — the LLVM and wasi-sdk that are already in the pinned
              base image, at `/usr/lib/llvm-<ver>` and `$WASI_SDK_PATH`. The
              tools link against the exact toolchain `elflift` was built with,
              so moving the recipes out of the Dockerfile provably cannot change
              what they emit. Requires the build to run inside the base image.

  hermetic  — LLVM 16.0.6 and wasi-sdk 24.0 downloaded and verified by sha256,
              so `bazel build //...` works on a bare host with no Docker at all.
              ⚠️ The tools then link against a DIFFERENT BUILD of the same LLVM
              version as the image's, and they are NOT byte-identical to it:
              Debian ships one shared libLLVM, upstream ships static components.
              `builder/hermetic_differential.sh` measures what that costs and
              what it does not -- measured 2026-08-23, the emitted OBJECT is
              byte-identical, the intermediate bitcode differs by the 36-byte
              embedded LLVM version string, and the partition cache therefore
              misses across a switch. None of that is assumed anywhere.

              ❌ Hermetic tools built on a NEWER host cannot run INSIDE the
              image (`GLIBC_2.38 not found`). This mode is for host-side builds
              and CI without Docker; staging its output into the builder image
              needs a build host whose glibc is no newer than the image's.

Selected by `RAPTORMARK_BAZEL_SDK`, which `.bazelrc` sets from `--config=image`
(the default) or `--config=hermetic`.

# What is and is not a tracked input

In BOTH modes the compilers are reached by ABSOLUTE PATH, so Bazel does not
track the individual tool files as action inputs. What differs is what pins them:

  image     the image is pinned, and its identity already reaches the
            object-cache key as `raptormark.base_id`
            (`internal/translate.TranslateID`). Bazel is not the system of
            record for toolchain identity in this project and must not be
            mistaken for it.

  hermetic  the ARCHIVE is pinned, by sha256, so its contents cannot drift even
            though the files inside it are not declared inputs.

⚠️ An earlier version of this comment said the hermetic SDK "IS a tracked
input". That was too strong: a recorded archive hash pins the CONTENT, it does
not make the extracted files action inputs. Editing a file inside the external
repo by hand would rebuild nothing, in either mode.

Either way the values below are interpolated into every action's command line,
so switching sources, LLVM versions or wasi-sdk paths re-runs the actions rather
than serving a cached artifact built by the other toolchain.
"""

# The LLVM the pinned elfconv line uses. `builder/Dockerfile` carried this as
# ARG ECV_LLVM_VER, and translate-one still reads ECV_LLVM_VER at run time to
# pick the matching llvm-link/opt/clang -- bitcode has to match. 22 selects the
# LLVM-22 base (raptormark-elfconv-base:llvm22).
_DEFAULT_LLVM_VER = "16"

# wasi-sdk 24, as unpacked in the base image. The base image sets WASI_SDK_PATH;
# this is the fallback when the environment does not.
_IMAGE_WASI_SDK_FALLBACK = "/root/wasi-sdk-24.0-arm64-linux"

def _detect_image_sdk(ctx, llvm_ver):
    """Locate the in-image toolchain, failing loudly rather than half-working."""
    llvm_prefix = "/usr/lib/llvm-" + llvm_ver
    if not ctx.path(llvm_prefix + "/bin/llvm-config").exists:
        fail(
            "RAPTORMARK_BAZEL_SDK=image, but {}/bin/llvm-config is not here.\n".format(llvm_prefix) +
            "This mode expects the build to run INSIDE the elfconv base image, " +
            "which is where LLVM {} lives. Either run it there:\n".format(llvm_ver) +
            "    raptormark bazel build //builder:tools\n" +
            "or build against downloaded toolchains instead:\n" +
            "    bazel build --config=hermetic //builder:tools",
        )

    wasi = ctx.getenv("WASI_SDK_PATH") or _IMAGE_WASI_SDK_FALLBACK
    if not ctx.path(wasi + "/bin/clang").exists:
        fail(
            "RAPTORMARK_BAZEL_SDK=image, but {}/bin/clang is not here.\n".format(wasi) +
            "WASI_SDK_PATH is set by the base image's login shell; a non-login " +
            "shell will not have it. `docker run` with `bash --login -c`, or set " +
            "WASI_SDK_PATH explicitly.",
        )
    return llvm_prefix, wasi

def _sdk_impl(ctx):
    source = ctx.getenv("RAPTORMARK_BAZEL_SDK") or "image"
    llvm_ver = ctx.getenv("RAPTORMARK_BAZEL_LLVM_VER") or _DEFAULT_LLVM_VER

    if source == "image":
        llvm_prefix, wasi_path = _detect_image_sdk(ctx, llvm_ver)
        stamp = "image:llvm-{}:{}".format(llvm_ver, wasi_path)
        # Empty on purpose: the image's LLVM reports NO system libs, so the
        # link line stays byte-for-byte what builder/Dockerfile ran, and
        # //builder:tools_equivalence_test keeps meaning what it says.
        syslib_dir = ""
    elif source == "hermetic":
        llvm_prefix, wasi_path, stamp, syslib_dir = _fetch_hermetic_sdk(ctx, llvm_ver)
    else:
        fail("RAPTORMARK_BAZEL_SDK must be \"image\" or \"hermetic\", got {}".format(repr(source)))

    ctx.file("BUILD.bazel", """# Generated by //bazel:sdk.bzl. Do not edit.
package(default_visibility = ["//visibility:public"])
exports_files(["defs.bzl"])
""")
    ctx.file("defs.bzl", '''"""Generated by //bazel:sdk.bzl. Do not edit."""

SDK_SOURCE = {source}
LLVM_VER = {llvm_ver}
LLVM_PREFIX = {llvm_prefix}
WASI_SDK_PATH = {wasi_path}

# A -L directory that makes `-ltinfo` resolve, or "" when nothing is needed.
# Empty in image mode, so that command line is unchanged.
SYSLIB_DIR = {syslib_dir}

# Interpolated into every action command line so a toolchain switch re-runs the
# action instead of serving an artifact the other toolchain built.
SDK_STAMP = {stamp}
'''.format(
        source = repr(source),
        llvm_ver = repr(llvm_ver),
        llvm_prefix = repr(llvm_prefix),
        wasi_path = repr(wasi_path),
        syslib_dir = repr(syslib_dir),
        stamp = repr(stamp),
    ))

# The archives hermetic mode downloads.
#
# ⚠️ THE LLVM PATCH VERSION IS NOT A FREE CHOICE. The base image carries
# 16.0.6 (`llvm-config --version`, checked), and the whole point of this mode is
# to ask whether a different BUILD of that version produces the same tools. A
# different patch version would not answer that question, it would change it.
#
# ⚠️ AND 16.0.6 HAS NO x86_64 LINUX RELEASE. Checked across the whole 16.x line
# on 2026-08-23: 16.0.0, .2, .3 and .4 publish `clang+llvm-*-x86_64-linux-gnu-*`;
# 16.0.1, 16.0.5 and 16.0.6 do not. aarch64 is published for 16.0.6, which is
# also the architecture this project builds on. So x86_64 fails below with that
# fact rather than silently substituting 16.0.4 -- a patch-version swap is
# exactly the kind of "equivalent, surely" that this mode exists to test rather
# than assume.
_LLVM_RELEASE = {
    "16": {
        "aarch64": struct(
            url = "https://github.com/llvm/llvm-project/releases/download/llvmorg-16.0.6/clang+llvm-16.0.6-aarch64-linux-gnu.tar.xz",
            strip_prefix = "clang+llvm-16.0.6-aarch64-linux-gnu",
            sha256 = "283e904048425f05798a98f1b288ae0d28ce75eb1049e0837f959e911369945b",
            version = "16.0.6",
        ),
    },
}

# wasi-sdk 24.0, the same release the base image unpacks at
# /root/wasi-sdk-24.0-<arch>-linux.
#
# ⚠️ aarch64 ONLY, and deliberately so. An x86_64 entry was drafted here and
# removed: its sha256 could not be verified from this host, and a plausible-
# looking hash nobody has checked is worse than an absent one -- it reads as
# authority. It would also be unreachable, because the LLVM lookup above fails
# for x86_64 first. Add it together with the LLVM side, from a machine that can
# actually fetch both.
_WASI_RELEASE = {
    "aarch64": struct(
        url = "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-24/wasi-sdk-24.0-arm64-linux.tar.gz",
        strip_prefix = "wasi-sdk-24.0-arm64-linux",
        sha256 = "ae6c1417ea161e54bc54c0a168976af57a0c6e53078857886057a71a0d928646",
        version = "24.0",
    ),
}

# ⚠️ THE ONE PLACE HERMETIC MODE IS NOT HERMETIC, stated plainly rather than
# buried.
#
# The official LLVM build has terminfo support compiled into LLVMSupport, so
# `llvm-config --system-libs` asks for `-ltinfo`. The image's Debian-packaged
# LLVM does not: it reports NO system libs at all (checked, both, 2026-08-23).
# So a link that works in the image fails here on `setupterm`, `tigetnum`,
# `set_curterm`, `del_curterm` -- all reached from exactly one function,
# `Process::FileDescriptorHasColors`, which decides whether to colour output.
#
# ncurses is not vendored here. Vendoring a C library to satisfy colour
# detection in a tool that only ever writes bitcode would be a lot of machinery
# for nothing. Instead the host's runtime `libtinfo.so.6` is used -- present on
# essentially every Linux, since anything linked against ncurses needs it --
# and the missing piece is only the DEV SYMLINK `libtinfo.so`, which is what
# `-ltinfo` actually looks for and which ships in `libtinfo-dev`.
#
# So the repo rule makes that symlink itself, inside the external repo. No root,
# no apt, and the dependency is a runtime library rather than a dev package.
# It is still a host dependency, and this comment is where that is admitted.
_LIBDIRS = [
    "/usr/lib/aarch64-linux-gnu",
    "/usr/lib/x86_64-linux-gnu",
    "/lib/aarch64-linux-gnu",
    "/lib/x86_64-linux-gnu",
    "/usr/lib64",
    "/usr/lib",
]

def _shim_tinfo(ctx):
    """Return a -L directory that makes `-ltinfo` resolve, or "" if unneeded."""

    # Already linkable: a real dev symlink exists, so add nothing and leave the
    # command line identical to image mode's.
    for d in _LIBDIRS:
        if ctx.path(d + "/libtinfo.so").exists:
            return ""

    for d in _LIBDIRS:
        real = d + "/libtinfo.so.6"
        if ctx.path(real).exists:
            ctx.symlink(real, "syslibs/libtinfo.so")
            return str(ctx.path("syslibs"))

    fail(
        "hermetic mode needs libtinfo: the official LLVM build links it for " +
        "terminal colour detection, and neither libtinfo.so nor libtinfo.so.6 " +
        "was found in " + str(_LIBDIRS) + ".\n" +
        "Install libtinfo-dev (or ncurses-devel), or build with --config=image.",
    )

def _host_arch(ctx):
    """Normalise Bazel's os.arch, which spells the same machine several ways."""
    arch = ctx.os.arch
    if arch in ("aarch64", "arm64"):
        return "aarch64"
    if arch in ("amd64", "x86_64"):
        return "x86_64"
    return arch

def _fetch_hermetic_sdk(ctx, llvm_ver):
    """Download LLVM and wasi-sdk, verified by hash, and return their prefixes."""
    if not ctx.os.name.lower().startswith("linux"):
        fail("RAPTORMARK_BAZEL_SDK=hermetic supports Linux only; this is " + ctx.os.name)

    arch = _host_arch(ctx)

    by_arch = _LLVM_RELEASE.get(llvm_ver)
    if not by_arch:
        fail("no hermetic LLVM {} is configured; //bazel:sdk.bzl knows about {}".format(
            llvm_ver,
            sorted(_LLVM_RELEASE.keys()),
        ))
    llvm = by_arch.get(arch)
    if not llvm:
        fail(
            ("no hermetic LLVM {ver} for {arch}.\n\n" +
             "For {arch} this is upstream's gap, not a missing entry here: LLVM " +
             "publishes no clang+llvm release for {ver}.x on this architecture " +
             "at the patch version the base image carries. 16.0.4 is the newest " +
             "16.x with an x86_64 Linux build.\n\n" +
             "Using it would change what this mode measures -- hermetic exists to " +
             "compare a DIFFERENT BUILD of the SAME version against the image's, " +
             "and a patch-version swap answers a different question. Either build " +
             "with --config=image, or decide deliberately to pin 16.0.4 and record " +
             "why.").format(ver = llvm_ver, arch = arch),
        )

    wasi = _WASI_RELEASE.get(arch)
    if not wasi:
        fail("no hermetic wasi-sdk for {}".format(arch))

    # ~914 MB compressed for LLVM. Bazel's repository cache keys on the sha256,
    # so this is paid once per machine, not once per build.
    ctx.report_progress("downloading LLVM {} ({})".format(llvm.version, arch))
    ctx.download_and_extract(
        url = llvm.url,
        output = "llvm",
        sha256 = llvm.sha256,
        stripPrefix = llvm.strip_prefix,
    )
    ctx.report_progress("downloading wasi-sdk {} ({})".format(wasi.version, arch))
    ctx.download_and_extract(
        url = wasi.url,
        output = "wasi-sdk",
        sha256 = wasi.sha256,
        stripPrefix = wasi.strip_prefix,
    )

    llvm_prefix = str(ctx.path("llvm"))
    wasi_path = str(ctx.path("wasi-sdk"))

    # Fail here rather than at the first compile: an archive that extracted but
    # has a different internal layout would otherwise surface as a confusing
    # "clang++: not found" inside a genrule.
    for probe in (llvm_prefix + "/bin/llvm-config", llvm_prefix + "/bin/clang++", wasi_path + "/bin/clang"):
        if not ctx.path(probe).exists:
            fail("hermetic SDK extracted but {} is missing; the archive layout changed".format(probe))

    syslib_dir = _shim_tinfo(ctx)

    stamp = "hermetic:llvm-{}:wasi-sdk-{}:{}".format(llvm.version, wasi.version, arch)
    return llvm_prefix, wasi_path, stamp, syslib_dir

raptormark_sdk = repository_rule(
    implementation = _sdk_impl,
    # Re-fetch when any of these change: the whole point is that the two
    # sources must not be able to serve each other's artifacts.
    environ = [
        "RAPTORMARK_BAZEL_SDK",
        "RAPTORMARK_BAZEL_LLVM_VER",
        "WASI_SDK_PATH",
    ],
    # ❌ NOT `local = True`, which it was until hermetic mode existed. A local
    # repo is re-fetched whenever Bazel revalidates the workspace, which for
    # image mode meant a cheap stat and for hermetic mode would mean
    # re-extracting a ~5 GB LLVM tree. `environ` above is what actually forces a
    # re-fetch when the selection changes, and it does so in both modes.
    #
    # ⚠️ The cost: in image mode this no longer notices an image whose LLVM moved
    # under a path that did not. That is covered elsewhere and better -- the
    # image's identity is `raptormark.base_id`, which is in the object-cache key.
    doc = "LLVM 16 + wasi-sdk, from the base image or from downloaded archives.",
)

def _sdk_ext_impl(_ctx):
    raptormark_sdk(name = "raptormark_sdk")

sdk = module_extension(implementation = _sdk_ext_impl)
