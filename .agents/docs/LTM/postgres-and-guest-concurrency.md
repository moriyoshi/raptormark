# PostgreSQL under ecvisor, and what guest concurrency costs

Durable record of the PostgreSQL 17 bring-up and the concurrency work it forced.
Chronology and the reasoning trail live in `JOURNAL.md`; this is what remains
true afterwards.

## Status

PostgreSQL 17 runs inside one WebAssembly module: `initdb`, a postmaster with
its background workers, and a guest-side `psql` connecting over a unix socket.
Measured 2026-08-12 under bounded snapshots, which were opt-in at the time and
are unconditional since 2026-08-22:

- **8 simultaneous clients**, 8/8 exiting 0 and 8/8 rows committed, across a
  2 -> 4 -> 8 ladder with no lost writes.
- **948 MiB peak** linear memory for the whole ladder, against 922 MiB for two
  clients: about 26 MiB for six more concurrent connections.
- DDL and DML (`CREATE`/`INSERT`/`UPDATE`/`DELETE`/aggregates) over
  `/tmp/.s.PGSQL.55432` with `listen_addresses` empty, so no TCP path exists.

The module is four programs -- dash, initdb, postgres (with extensions), psql --
selected by the exec map.

## Bring-up chain

The blockers formed a producer/consumer chain rather than one PostgreSQL defect:
TLSDESC relocation output, full file-type bits in RFS `st_mode`, canonical
exec-map paths on a usr-merged rootfs, missing AArch64 CRC/FCVT/vector semantics,
64 KiB aarch64 glibc page assumptions, shared-mapping placement and lifetime,
durability syscalls, AF_UNIX, polling, and finally cross-process file sharing.
Each layer became observable only after the previous one was removed.

Several apparent capacity failures were instruction-lifting bugs. ICU's repeated
rehash and projected 48 GB allocation came from dropping the fifth register bit
in a by-element ASIMD multiply. The temporary 512 MiB arena masked that defect
and was returned to 384 MiB after the lifter was corrected. Treat allocation
growth as a statement-level symptom, not evidence that the arena should grow.

## The arena, and why 384 MiB

`MEMORY_ARENA_SIZE` is 384 MiB. It was briefly 512 to fit a 96 MiB one-shot
allocation from initdb's post-bootstrap backend; that allocation was ICU
rehashing under a mis-decoded by-element `M` bit (patches/0045) and does not
occur with the lifter fixed. **Do not re-derive a layout from the 512 MiB
measurements** -- they describe a workload that no longer exists.

The ceiling is wasm32's 4 GiB linear memory, and it is not slack: under the
full-buffer scheme a run reached 4010 MiB and failed the NEXT 384/512 MiB
request, which was the backend for its first client.

## Bounded snapshots

**The ONLY scheme since 2026-08-22**, by the user's ruling. A suspended process
stores only the ranges it can have written instead of a whole arena buffer. It
was opt-in, became the default the same day, and the environment gate was then
REMOVED outright -- there is no full-buffer alternative to select, and
`diag::tests::the_removed_snapshot_gate_stays_removed` scans `runtime/src` for
the old variable name so a reintroduction cannot be accidental.

⚠️ **A multi-threaded group still takes a full snapshot**, and that is a
property of the process, not a mode: a bounded range set derives from ONE stack
pointer, which says nothing about a sibling's live stack. `EcvContext::snapshot_for`
decides it with `is_multithreaded` alone. `SnapshotData::Full`, `Arena::snapshot`
and the `Full` arm of `Arena::restore_in_place` exist for this and must not be
deleted; the restore must stay EXHAUSTIVE, because an `if let ... Bounded` there
restored no memory at all for a process forked from a thread.

⚠️ **The oracle is gone.** `RAPTORMARK_ECV_SNAPCHECK` compared the live arena
against the incoming process's FULL buffer, and a single-threaded process no
longer has one. `Arena::bytes_differing_outside` returns `Option<SnapDiff>` and
the probe prints `NO-ORACLE` for those switches rather than `miss=0` -- which
would have been a clean bill of health computed from no data, the same failure
shape as `bbmiss insn=` and `undecoded_message`. A `Some` is now hypothetical
too: it exists only for a multi-threaded group, which is restored in full, so it
answers "would a bounded snapshot have been safe here".

Measured dirty set on a real postgres workload: **median 2 MiB, max 6 MiB of
384 (1.6%)** over 102 switches. The ranges are:

- writable PT_LOAD segments (`EcvProgram::writable_loads`)
- `[BRK_START_VMA, brk_cur)`
- the live private mmap extents (`Arena::mmap_live`, which holds `(start,
  LENGTH)` -- not `(start, end)`)
- the used stack, `[sp, STACK_TOP_VMA)`
- **the TLS area**, from a page below `THREAD_PTR` through
  `THREAD_PTR + TCB_SIZE + tls_extent`. Sized from **`.ecv.tls`**, not
  `tls_phdr()`: a fused multi-module image advertises ZERO PT_TLS headers, so
  the phdr path returns 0 for exactly the images that matter.

Three invariants an implementation must keep:

1. **Allocation zero-fills.** `mmap_reserve` and `set_brk` zero what they hand
   out. The live arena is shared, so without this a fresh mapping or a brk
   growth returns the previous process's bytes -- measured as differing in 39 of
   59 switches (mmap) and 9 of 59 (brk).
2. **A cross-program switch re-materialises the image.** Read-only text and
   rodata are identical only WITHIN one program; across programs the arena
   differs by the whole image (57 MiB measured). `materialized_prog` tracks
   what is loaded, and every image load must go through the one function that
   loads AND records it -- `exec_into`, boot and `merge_units` all change it.
3. **The stack below `sp` is not saved.** Dead frames under AAPCS64 (no red
   zone). Deliberate, and up to ~451 bytes differ in practice -- 1,307 on
   nginx. ⚠️ This is the one thing the 2026-08-22 ruling ACCEPTED rather than
   closed, and the figures are samples rather than bounds. Removing the flag the
   same day made it **unsettleable by differential**: there is no unbounded run
   left to compare against, so closing it needs an argument from the ABI or an
   instrumented guest, not an A/B run. That narrowing was taken knowingly.

`RAPTORMARK_ECV_SNAPCHECK=1` verified the whole premise against the full-buffer
scheme as an oracle, per region and per program pair. **It only worked while
both schemes existed.** It is still wired up and still useful on a multi-threaded
group, where a `Full` snapshot survives; everywhere else it now reports
`NO-ORACLE` and compares nothing. Do not read a run of `NO-ORACLE` lines as a
pass.

## Named AF_UNIX sockets

WASI has no AF_UNIX -- WasmEdge's socket extension knows only INET4/INET6 -- and
it should not: both endpoints are guest processes in one module, and the
rendezvous path exists only in the guest's rfs/tmpfs. So it is implemented
entirely in-runtime.

- `OpenFile::UnixSocket` covers only the unconnected states. `connect` and
  `accept` both yield the existing `SocketPair`, which already had blocking,
  readiness, EOF and SCM_RIGHTS.
- `bind` publishes a real `NodeKind::Socket` VFS node. `S_ISSOCK` is
  load-bearing: postgres stats its socket path before unlinking a stale one.
- `unlink` clears the NAME, not the listener -- postgres unlinks its live socket
  during shutdown while backends are still connected.
- The backlog is a FIFO and is not capacity-enforced. Abstract names (leading
  NUL) are delimited by `addrlen`, never by a NUL.

Two syscalls a UDS client needs that were missing: **`ppoll` (73)** -- aarch64
has no plain `poll`, so libpq's `pqSocketCheck` lands there on every connect and
query -- and **`send`/`recv`**, which are `sendto`/`recvfrom` and were dispatched
on "does this fd have a host socket", giving ENOTSOCK for every in-guest
endpoint.

## Open files are shared by path

`OpenFile::Mem` holds an index into a context-global `open_files` table, one
`MemFile` per path, refcounted at `fork`/`dup`/`close` exactly like a pipe end,
flushed to the tmpfs upper layer on the LAST close.

It was previously a private copy per descriptor, which made two processes
writing one file diverge with last-close-wins. Unreachable until bounded
snapshots allowed two backends at once, then immediately fatal:
`unexpected data beyond EOF in block 0 of relation base/5/16384`.

`O_TRUNC`, `truncate(path)` and `rename` must act on the SHARED entry. `pos`
remains per descriptor, where Linux shares the offset across `dup` and `fork`.

## Running it

- Sidecars are built from the LINK's manifest (`link.WriteLinkInputs` writes
  `registry.c` and `programs.json` together); `-map path=INDEX` names programs
  by index so no module hash is ever typed. The runtime warns loudly when an
  exec-map entry names a program the module does not contain.
- `wasmedge --dir /:/out` means a sidecar at `/out/x.img` on the host is
  `/x.img` to the guest. Without a preopen the runtime cannot read it at all.
- **`sleep` does not exist in the guest** (coreutils, not a lifted program), and
  a spin-wait STARVES the process being waited for -- the scheduler is
  cooperative and only switches when the running process blocks or exits. Wait
  by doing work that yields, e.g. a retry loop where each attempt is a process.
- Guest scripts must avoid backticks when embedded in Go raw strings.
## Planner-Path Validation and Remaining Decode Coverage

The historical `SELECT 6*7` milestone is constant-folded. `SELECT count(*) FROM pg_class` reaches statistics planning and found undecoded `fnmul` in `get_variable_numdistinct`; the same failure occurs before patch 0062. Future PostgreSQL validation should also plan over a real relation. Full initdb and the recorded query showed patch 0062 has zero measured lifting effect on PostgreSQL.

## Consolidated Update: Planner and Data-Path Rungs

Patches 0064 and 0065 advanced PostgreSQL through planning, scans, joins, sorting, DDL, DML, indexing, hash aggregation, numeric average, and JSON extraction with native-matching results.

`count(*)` did not execute sites in `ExecInitAgg`; `GROUP BY` forced that path. Remaining sites are leads, not blockers until executed.

## Consolidated Update: Locale Closure and Shared-Window Reclamation

Appending `/usr/bin/locale` as program 4 adds no library to the closure and preserves cache hits for programs 0-3. It removes initdb's `Exec format error`; both absolute invocation and `popen("locale -a")` return `C`, `C.utf8`, `POSIX`, and `en_US.utf8`. The completed five-program run registers 879 collations, including the expected three additional libc collations.

The first five-program run exposed a shared-window reclaim defect. Glibc maps the permanent locale archive, alias file, and gconv cache read-only with `MAP_SHARED`; the old path-exists rule pinned 3,211,264 bytes and lowered `shm_top` from `0x16000000` to `0x10fd0000`, leaving too little private mmap space for the postmaster's 78,618,624-byte request. Reclaim now follows whether the region was ever writable. Read-only locale mappings are released when their last mapper exits, while writable named PostgreSQL DSM remains reusable across backends. The repaired run reaches `BOOT: DONE` with the established SQL results.
