# raptormark builder image: PACKAGING ONLY.
#
# ❌ NOTHING IS BUILT HERE. There is not a single `RUN` line, and adding one is
# the change this file exists to prevent. Everything the image gains over its
# base is built by Bazel and staged by `//builder:stage`, which declares each
# file at the path it has in the image. What used to be here -- a rustup
# installer, three cargo builds, two wasi-sdk compiles and four LLVM tool
# builds -- is now `//runtime`, `//runtime/loopback`, `//runtime/browser` and
# `//builder`.
#
# WHAT THAT BOUGHT, concretely:
#
#   - A tool can be rebuilt without rebuilding an image, and a header edit
#     rebuilds both tools that include it. `COPY` ordering used to decide that.
#   - The pipeline binary is a dependency rather than a prebuilt file. CLAUDE.md
#     records that a raw `docker build` did NOT rebuild it, and that this cost a
#     whole void gate on 2026-08-18 while every label claimed the change had
#     landed. `//builder:stage` depends on
#     `//cmd/raptormark:builder_tools_linux_arm64`, so a stale binary is no
#     longer representable.
#   - The compile flags are checked. //builder:tools_equivalence_test and
#     //runtime:cshim_equivalence_test compare against a transcription of the
#     recipes this file used to carry.
#
# ⚠️ THE BUILD CONTEXT IS THE STAGED TREE, not the repo root. `raptormark
# build-image` writes `//builder:stage`'s output to builder/_stage and passes
# that as the context, so `COPY . /` below places every file at its image path.
# Building this by hand with the repo root as context would copy the repo into
# the image.
#
# Docker still builds this last layer on purpose; see the note on
# `//builder:stage` for why rules_oci is a poor fit for a 4.7 GB local-only
# base.
ARG ELFCONV_BASE=raptormark-elfconv-base:latest
FROM ${ELFCONV_BASE}

# The whole image contents, at their image paths:
#   /opt/ecvisor/libecvisor.a            net-wasmedge, the shipping default
#   /opt/ecvisor/loopback/libecvisor.a   net-loopback, no host imports at all
#   /opt/ecvisor/browser/libecvisor.a    net-browser, the raptormark_net_v1 seam
#   /opt/ecvisor/ecv_sp.o                shadow-stack-pointer probe
#   /opt/ecvisor/ecv_globals.o           the wasm globals lifted code reads inline
#   /usr/local/bin/ecv-prepare           link + internalize + namespace + split
#   /usr/local/bin/ecv-split             content-stable partitioner (the default)
#   /usr/local/bin/namespace-object      the ECV_NO_MERGED_PREPARE path
#   /usr/local/bin/ecv-promote           register promotion, for --promote
#   /usr/local/bin/raptormark-tools      translate-one / link-all
COPY . /

# LLVM toolchain version. 16 = the pinned elfconv line; 22 = the LLVM-22 base
# (raptormark-elfconv-base:llvm22). ECV_LLVM_VER is exported so translate-one
# picks the matching llvm-link/opt/clang -- bitcode must match. The tools in
# /usr/local/bin were built against this same LLVM by //bazel:sdk.bzl, which
# reads it as RAPTORMARK_BAZEL_LLVM_VER.
ARG ECV_LLVM_VER=16
ENV ECV_LLVM_VER=${ECV_LLVM_VER}

# Cache identity for per-binary translated objects
# (internal/translate.TranslateID): the base image digest (elflift + wasi-sdk)
# and a hash of the translation pipeline's sources
# (internal/builder.TranslateSH), both computed and passed by `raptormark
# build-image`. These are stable across ecvisor edits, so keying the .o cache on
# them lets an ecvisor-only change reuse every translated object. Placed LAST so
# changing them does not invalidate the COPY above.
ARG BASE_ID=
ARG TRANSLATE_SH=
LABEL raptormark.base_id=$BASE_ID raptormark.translate_sh=$TRANSLATE_SH

ENTRYPOINT ["/usr/local/bin/raptormark-tools", "translate-one"]
