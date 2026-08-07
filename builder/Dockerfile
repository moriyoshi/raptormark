# raptormark builder image.
#
# Layered on the base image produced from the pinned elfconv submodule's own
# Dockerfile (third_party/elfconv/Dockerfile, ECV_AARCH64=1), which carries the
# full toolchain (LLVM 16, wasi-sdk 24, emscripten, wasmtime, wasmedge) and a
# built `elflift` at /root/elfconv/build/lifter/elflift.
#
# Build via `raptormark build-image` — it builds the base first, threads the
# submodule pin through the tags, and uses the repo root as context (with
# .dockerignore restricting it to builder/ and runtime/).
ARG ELFCONV_BASE=raptormark-elfconv-base:latest
FROM ${ELFCONV_BASE}

# Rust toolchain for the ecvisor runtime (wasm32-wasip1 staticlib).
# Pinned; libclang for bindgen is already present in the base image.
ARG RUST_VERSION=1.88.0
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs -o /tmp/rustup-init.sh && \
    sh /tmp/rustup-init.sh -y --profile minimal --default-toolchain ${RUST_VERSION} \
      --target wasm32-wasip1 && \
    rm /tmp/rustup-init.sh
ENV PATH=/root/.cargo/bin:${PATH}

# ecvisor: built here so translate-one can link it in place of the upstream
# C++ runtime. Two third-party crates resolve into this build (miniz_oxide +
# adler2), both pure Rust and both pinned by the committed Cargo.lock via
# `--locked`, so the layer needs the network but cannot drift. It is NOT an
# offline build; the comment here claimed "zero crate dependencies — builds
# offline" long after miniz_oxide arrived.
COPY runtime /opt/ecvisor-src
RUN cargo build --manifest-path /opt/ecvisor-src/Cargo.toml \
      --release --target wasm32-wasip1 --locked && \
    mkdir -p /opt/ecvisor && \
    cp /opt/ecvisor-src/target/wasm32-wasip1/release/libecvisor.a /opt/ecvisor/

# The loopback profile: the SAME runtime with `net-loopback` instead of
# `net-wasmedge`, so its network is a pure-Rust in-process one and it calls no
# socket import at all.
#
# It exists to make a property checkable rather than argued. ecvisor is linked
# into every module whole, so the default build demands WasmEdge's eleven
# non-standard socket imports even from a guest that never opens a socket --
# which is why `containerd-shim-wasmtime` can load a raptormark module but not
# run it. The backend seam (runtime/src/net) is supposed to remove them by
# compile-time exclusion; linking against THIS archive is what proves it did,
# because no amount of reasoning about `dyn` substitutes for reading the import
# section of a real module.
#
# A separate --target-dir: a different feature set is a different fingerprint,
# so sharing one would make the two builds evict each other on every image
# build. The layer is small next to LLVM and elfconv.
RUN cargo build --manifest-path /opt/ecvisor-src/Cargo.toml \
      --release --target wasm32-wasip1 --locked \
      --no-default-features --features net-loopback \
      --target-dir /tmp/ecv-loopback && \
    mkdir -p /opt/ecvisor/loopback && \
    cp /tmp/ecv-loopback/wasm32-wasip1/release/libecvisor.a /opt/ecvisor/loopback/ && \
    rm -rf /tmp/ecv-loopback

# The browser profile: the same runtime again, with `net-browser`. Its network
# is the `raptormark_net_v1` import module, and its `wait` NEVER blocks -- a
# browser host cannot be blocked, so the scheduler returns `Idle` and the host
# resumes it. See runtime/src/net/browser.rs.
RUN cargo build --manifest-path /opt/ecvisor-src/Cargo.toml \
      --release --target wasm32-wasip1 --locked \
      --no-default-features --features net-browser \
      --target-dir /tmp/ecv-browser && \
    mkdir -p /opt/ecvisor/browser && \
    cp /tmp/ecv-browser/wasm32-wasip1/release/libecvisor.a /opt/ecvisor/browser/ && \
    rm -rf /tmp/ecv-browser

# The two C shims. Both exist for the same reason: they express things at the
# wasm level that Rust cannot state, so they cannot live in libecvisor.a.
#
#   ecv_sp.c       shadow-stack-pointer probe. All that is left of the old
#                  SjLj/EH yield-unwind shim: suspension is a plain return now
#                  (elfconv patch 0026), so there is no setjmp lowering, no
#                  -lsetjmp, and no wasm-opt --translate-to-exnref.
#   ecv_globals.c  the wasm globals the LIFTED code reads inline instead of
#                  calling into ecvisor for. `__ecv_unwinding` replaces 33,540
#                  `_ecv_suspended` calls per bash-sized module with one
#                  `global.get` each (elfconv patch 0059).
#
# Neither adds a wasm proposal: `mutable-globals` is inside Wasm 2.0 and the
# module already required it for `__stack_pointer`, which is what a stock runwasi
# shim accepts. `--login` provides WASI_SDK_PATH from the base image.
RUN bash --login -c 'set -e; for s in ecv_sp ecv_globals; do \
      "$WASI_SDK_PATH/bin/clang" -O2 --target=wasm32-wasip1 \
        --sysroot="$WASI_SDK_PATH/share/wasi-sysroot" \
        -c "/opt/ecvisor-src/cshim/$s.c" -o "/opt/ecvisor/$s.o"; \
    done'

# LLVM toolchain version. 16 = the pinned elfconv line; 22 = the LLVM-22 base
# (raptormark-elfconv-base:llvm22). ecv-promote is built with this LLVM, and ECV_LLVM_VER is
# exported so translate-one picks the matching llvm-link/opt/split/clang (bitcode must match).
ARG ECV_LLVM_VER=16
ENV ECV_LLVM_VER=${ECV_LLVM_VER}

# ecv-promote: the companion register-promotion pass (post-lift LLVM tool over elflift's
# remill-standard norm_mode bitcode). translate-one runs it on the lifted bitcode when
# --promote is passed. See .agents/docs/VRO_REWRITE_PLAN.md.
COPY builder/ecv-promote.cpp /tmp/ecv-promote.cpp
RUN /usr/lib/llvm-${ECV_LLVM_VER}/bin/clang++ -std=c++17 -fno-rtti -O2 /tmp/ecv-promote.cpp \
      $(/usr/lib/llvm-${ECV_LLVM_VER}/bin/llvm-config --cxxflags --ldflags --libs core irreader support bitwriter) \
      -o /usr/local/bin/ecv-promote && rm /tmp/ecv-promote.cpp

# namespace-object: per-program symbol namespacing, run between internalize and
# the split codegen so llvm-split can externalize locals without two programs
# colliding at the ecvisor link. See the file header.
# ecv-split: content-stable module partitioning, replacing llvm-split when
# ECV_STABLE_SPLIT=1. llvm-split balances partitions by size across the whole
# module, so any change reshuffles every bucket and no partition can be cached;
# this assigns by a stable hash of the symbol name while keeping llvm-split's
# promotion and its blockaddress clustering. See the file header.
COPY builder/ecv-split.cpp builder/ecv-partition.h /tmp/
RUN /usr/lib/llvm-${ECV_LLVM_VER}/bin/clang++ -std=c++17 -fno-rtti -O2 -I/tmp /tmp/ecv-split.cpp \
      $(/usr/lib/llvm-${ECV_LLVM_VER}/bin/llvm-config --cxxflags --ldflags --libs core irreader support bitwriter transformutils) \
      -o /usr/local/bin/ecv-split && rm /tmp/ecv-split.cpp

COPY builder/namespace-object.cpp builder/ecv-namespace.h /tmp/
RUN /usr/lib/llvm-${ECV_LLVM_VER}/bin/clang++ -std=c++17 -fno-rtti -O2 -I/tmp /tmp/namespace-object.cpp \
      $(/usr/lib/llvm-${ECV_LLVM_VER}/bin/llvm-config --cxxflags --ldflags --libs core irreader support bitwriter) \
      -o /usr/local/bin/namespace-object && rm /tmp/namespace-object.cpp

# ecv-prepare: llvm-link + internalize/globaldce + namespace-object in ONE parse.
# Those three each parsed and re-serialized the whole module for work measured in
# hundredths of a second -- internalize+globaldce is 0.06 s behind a 4.7 s parse
# on bash-glibc. Selected by ECV_MERGED_PREPARE; the default path still runs the
# three separate tools. Shares builder/ecv-namespace.h with namespace-object so
# the two cannot emit different bytes. See the file header.
COPY builder/ecv-prepare.cpp /tmp/ecv-prepare.cpp
RUN /usr/lib/llvm-${ECV_LLVM_VER}/bin/clang++ -std=c++17 -fno-rtti -O2 -I/tmp /tmp/ecv-prepare.cpp \
      $(/usr/lib/llvm-${ECV_LLVM_VER}/bin/llvm-config --cxxflags --ldflags --libs core irreader support bitwriter linker ipo passes transformutils) \
      -o /usr/local/bin/ecv-prepare && rm /tmp/ecv-prepare.cpp /tmp/ecv-namespace.h /tmp/ecv-partition.h

# The builder tools (translate-one, link-all) — one static Go binary,
# cross-built for this image's platform by `raptormark build-image`, which is
# also what puts it here. Building it on the host rather than in a Docker stage
# keeps this image free of a Go toolchain.
#
# It resolves WASI_SDK_PATH by asking a login shell, so unlike the scripts it
# replaced it does not need a `bash --login` entrypoint wrapper.
COPY builder/_tools/raptormark-builder-tools /usr/local/bin/raptormark-tools
RUN chmod +x /usr/local/bin/raptormark-tools

# Cache identity for per-binary translated objects (internal/translate.TranslateID;
# .agents/docs/PERF.md, Lever A): the base image digest (elflift + wasi-sdk) and a
# hash of the translation pipeline's sources (internal/builder.TranslateSH), both
# computed and passed by `raptormark build-image`. These are stable across ecvisor
# edits (which rebuild only the layers above, not the base), so keying the .o cache
# on them lets an ecvisor-only change reuse every translated object. Placed LAST so
# changing them does not invalidate the expensive Rust/cargo layers.
ARG BASE_ID=
ARG TRANSLATE_SH=
LABEL raptormark.base_id=$BASE_ID raptormark.translate_sh=$TRANSLATE_SH

ENTRYPOINT ["/usr/local/bin/raptormark-tools", "translate-one"]
