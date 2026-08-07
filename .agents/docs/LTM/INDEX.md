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
| [build-pipeline-synthesis.md](build-pipeline-synthesis.md) | Four-stage architecture, producer contracts, deterministic lifting, library-scoped reuse, and cache-safe operation | 6 pipeline source documents |
| [ecvisor-runtime-synthesis.md](ecvisor-runtime-synthesis.md) | Process arenas, bounded snapshots, timed waits, guest networking/files, released Wasm compatibility, and THE ADDRESS BUDGET (why a speculative reservation is free natively and full price here) | 3 runtime source documents |
| [engineering-practice-synthesis.md](engineering-practice-synthesis.md) | Current critical paths, hot-path opt-in design, false-control defenses, regression evidence, and quality gates | 4 engineering source documents |
| [target-enablement-synthesis.md](target-enablement-synthesis.md) | Cross-target bring-up through loader state, function discovery, instruction coverage, and bounded validation | 3 target-enablement source documents |

## Source Topic Documents

| Document | Summary |
|----------|---------|
| [recovery-and-builder-provenance.md](recovery-and-builder-provenance.md) | Recovery evidence, elfconv patch provenance, builder identities, image tags, and Docker preservation rules |
| [image-discovery-and-rootfs.md](image-discovery-and-rootfs.md) | Container discovery, executable closure policy, symlink handling, and RFS sidecar production |
| [fusing-relocations-and-ifunc.md](fusing-relocations-and-ifunc.md) | Dynamic ELF fusing, RELR, TLS/TLSDESC, and GNU IFUNC evaluation |
| [function-boundary-recovery.md](function-boundary-recovery.md) | Function extents, `.eh_frame`, init/fini entries, computed pointers, and `.ecv.funcs` limits |
| [runtime-metadata-producers.md](runtime-metadata-producers.md) | Contracts between recovered runtime custom-section consumers and reconstructed Go producers |
| [translation-linking-and-object-cache.md](translation-linking-and-object-cache.md) | Deterministic lifting, library-scoped partitions, shared names, manifests, and cache correctness |
| [ecvisor-process-and-thread-model.md](ecvisor-process-and-thread-model.md) | Process switching, arena ownership, bounded snapshots, fork replay, futexes, and TLS |
| [postgres-and-guest-concurrency.md](postgres-and-guest-concurrency.md) | PostgreSQL 17 bring-up, bounded arena snapshots, in-runtime AF_UNIX, and shared open files |
| [ecvisor-vfs-syscalls-and-networking.md](ecvisor-vfs-syscalls-and-networking.md) | RFS, timed waits, socket semantics, guest AF_UNIX, shared files, and nginx serving |
| [wasm-runtime-and-oci-compatibility.md](wasm-runtime-and-oci-compatibility.md) | Wasm 2.0 constraints, proposal-free suspension, OCI packaging, and released shim behavior |
| [performance-investigation.md](performance-investigation.md) | Lifter and codegen critical paths, runtime switching, and evidence-based measurement methods |
| [testing-and-regression-method.md](testing-and-regression-method.md) | Host and E2E gates, regression neutralization, false controls, and evidence standards |
| [agent-harness-and-quality-gate.md](agent-harness-and-quality-gate.md) | Documentation roles, memory workflows, and the current Go, Rust, and E2E quality gates |
| [web-embedder-and-browser-networking.md](web-embedder-and-browser-networking.md) | Node and browser embedding, compile-time network profiles, re-entrant execution, DNS, relay and service-worker transports, and nginx-in-a-tab coverage |

## Additional Source Topic Documents

| Document | Summary |
|----------|---------|
| [hot-path-cost-and-opt-in-design.md](hot-path-cost-and-opt-in-design.md) | What a wasm-interpreted hot path costs per instruction, how to add a feature to one without taxing the default, and why invariants held by convention fail |
| [aarch64-lifter-and-coverage.md](aarch64-lifter-and-coverage.md) | BTI indexing, AArch64 decoder patches, undecoded-instruction measurement, the runtime REACHABILITY census (and its zero result on python and postgres), and native-oracle guards |
| [python-redis-and-cryptography-bringup.md](python-redis-and-cryptography-bringup.md) | Dynamic Python and cryptography success, Redis's remaining musl TLS blocker, and plugin-loader integration |
| [ruby-jit-and-jump-table-bringup.md](ruby-jit-and-jump-table-bringup.md) | Ruby PAC jump-table recovery, patch 0062 validation, optional YJIT scope, and remaining adoption decisions |
