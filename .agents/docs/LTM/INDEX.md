# Long-Term Memory Index

Durable, topic-organized project knowledge. Synthesis documents provide a fast
orientation across related subsystems; source topic documents preserve the
detailed durable record and remain available for traceability.

`.agents/docs/JOURNAL.md` is the chronological source. `good-sleep` distils
journal entries into topic documents, `deep-sleep` builds broader syntheses, and
`reconcile-journal-ltm` audits coverage rather than trusting record tables.

## Synthesis Documents

| Document | Summary | Sources |
|----------|---------|---------|
| [build-pipeline-synthesis.md](build-pipeline-synthesis.md) | Four-stage architecture, producer contracts, registry/object identity, library reuse, and operation after historical builder removal | 6 pipeline source documents |
| [build-system-and-driver-synthesis.md](build-system-and-driver-synthesis.md) | How a correct builder image is produced and identified, and how the four stages are driven from the CLI | `bazel-build-and-hermetic-sdk.md`, `pipeline-driver-and-cli.md`, `recovery-and-builder-provenance.md`\* |
| [deployment-targets-and-profiles-synthesis.md](deployment-targets-and-profiles-synthesis.md) | The profile matrix, why the backend seams are compile-time, what each host forecloses, and the SCOPED Wasm-2.0 rule | `wasm-runtime-and-oci-compatibility.md`\*, `wasix-and-wasmer.md`, `web-embedder-and-browser-networking.md` |
| [ecvisor-runtime-synthesis.md](ecvisor-runtime-synthesis.md) | Mandatory bounded snapshots, signals and waits, memory/syscall semantics, shared-map lifetime, Wasm compatibility, and the fixed address budget | 3 runtime source documents |
| [engineering-practice-synthesis.md](engineering-practice-synthesis.md) | Critical-path measurement, host timing, mechanism witnesses, false-green defenses, environment gates, and process safety | 4 engineering source documents |
| [target-enablement-synthesis.md](target-enablement-synthesis.md) | Loader and boundary recovery, executed-set instruction coverage, cross-target controls, and Ruby's measured YJIT boundary | 3 target-enablement source documents |

\* Feeds two syntheses, deliberately. `recovery-and-builder-provenance.md` is
read by `build-pipeline-synthesis.md` for what the pipeline inherited and by
`build-system-and-driver-synthesis.md` for image identity;
`wasm-runtime-and-oci-compatibility.md` is read by `ecvisor-runtime-synthesis.md`
for the suspension model and by `deployment-targets-and-profiles-synthesis.md`
for the target constraint. Neither synthesis owns its shared source.

## Source Topic Documents

| Document | Summary |
|----------|---------|
| [recovery-and-builder-provenance.md](recovery-and-builder-provenance.md) | Recovery evidence, elfconv patch provenance, builder identities, and the post-2026-08-23 Docker inventory |
| [image-discovery-and-rootfs.md](image-discovery-and-rootfs.md) | Container discovery, executable closure policy, symlink handling, and RFS sidecar production |
| [fusing-relocations-and-ifunc.md](fusing-relocations-and-ifunc.md) | Dynamic ELF fusing, RELR, TLS/TLSDESC, and GNU IFUNC evaluation |
| [function-boundary-recovery.md](function-boundary-recovery.md) | Function extents, `.eh_frame`, init/fini entries, computed pointers, and `.ecv.funcs` limits |
| [runtime-metadata-producers.md](runtime-metadata-producers.md) | Contracts between recovered runtime custom-section consumers and reconstructed Go producers |
| [translation-linking-and-object-cache.md](translation-linking-and-object-cache.md) | Deterministic lifting, library-scoped partitions, registry-index identity, manifests, and cache correctness |
| [ecvisor-process-and-thread-model.md](ecvisor-process-and-thread-model.md) | Process switching, mandatory bounded snapshots, fork replay, signals, futexes, and TLS |
| [postgres-and-guest-concurrency.md](postgres-and-guest-concurrency.md) | PostgreSQL 17 bring-up, guest concurrency, locale closure, shared-window reclamation, AF_UNIX, and shared files |
| [ecvisor-vfs-syscalls-and-networking.md](ecvisor-vfs-syscalls-and-networking.md) | RFS, timed waits, memory syscalls, signal-aware waits, shared mappings, sockets, and nginx serving |
| [wasm-runtime-and-oci-compatibility.md](wasm-runtime-and-oci-compatibility.md) | Wasm 2.0 constraints, proposal-free suspension, OCI packaging, and released shim behavior |
| [performance-investigation.md](performance-investigation.md) | Lifter and codegen critical paths, external guest timing, index-shift costs, and evidence-based measurement |
| [testing-and-regression-method.md](testing-and-regression-method.md) | Mechanism witnesses, host and E2E gates, regression neutralization, false controls, and evidence standards |
| [agent-harness-and-quality-gate.md](agent-harness-and-quality-gate.md) | Documentation roles, memory workflows, test-environment requirements, process safety, and quality gates |
| [web-embedder-and-browser-networking.md](web-embedder-and-browser-networking.md) | Node and browser embedding, focused host coverage, browser matrix, DNS, relay, service workers, and nginx-in-a-tab |

## Additional Source Topic Documents

| Document | Summary |
|----------|---------|
| [hot-path-cost-and-opt-in-design.md](hot-path-cost-and-opt-in-design.md) | What a wasm-interpreted hot path costs per instruction, how to add a feature to one without taxing the default, and why invariants held by convention fail |
| [aarch64-lifter-and-coverage.md](aarch64-lifter-and-coverage.md) | BTI indexing, AArch64 decoder patches, undecoded-instruction measurement, the runtime REACHABILITY census (and its zero result on python and postgres), and native-oracle guards |
| [python-redis-and-cryptography-bringup.md](python-redis-and-cryptography-bringup.md) | Dynamic Python and cryptography success, Redis's remaining musl TLS blocker, and plugin-loader integration |
| [ruby-jit-and-jump-table-bringup.md](ruby-jit-and-jump-table-bringup.md) | Ruby PAC jump-table recovery, startup mappings and prctls, and YJIT's measured pre-codegen walls |
| [dynamic-side-module-loading.md](dynamic-side-module-loading.md) | The plugin band, per-unit fusing, the dlopen map, handle-aware `dlsym`, the loader seam and its backends, mid-run `dlopen` and `execve` loads, and `patches/0066` |
| [wasix-and-wasmer.md](wasix-and-wasmer.md) | The measured WASIX ABI and its silent traps, `--profile wasix` sockets, the deferred loader half and its PIC-Rust-std wall, and the WASIX process models |
| [bazel-build-and-hermetic-sdk.md](bazel-build-and-hermetic-sdk.md) | Bazel owning the builder image contents, byte-identity equivalence tests, `raptormark bazel`'s sharp edges, cache identity, and the hermetic mode's measured differential |
| [pipeline-driver-and-cli.md](pipeline-driver-and-cli.md) | `raptormark build` and `raptormark run`, the three runtimes' directory flags, and the defects a one-program fixture cannot express |

## Related Documents Outside LTM

| Document | Summary |
|----------|---------|
| [../WASIX_ABI.md](../WASIX_ABI.md) | The measured WASIX ABI record with its probes, for both the socket and the process halves. Summarised in `wasix-and-wasmer.md`. |
| [../MULTIMODULE.md](../MULTIMODULE.md) | The multi-module analysis and the §8 placement protocol, with §4's loader bullet amended for WASIX. |
| [../QUALITY_GATE.md](../QUALITY_GATE.md) | The canonical gate commands, test counts, and E2E environment. |
