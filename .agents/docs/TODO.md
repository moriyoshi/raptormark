# TODO

Open work items. Normally populated by the `good-sleep` skill, which extracts
to-do items out of `.agents/docs/JOURNAL.md` as it consolidates entries into
`.agents/docs/LTM/`.

**This file holds OPEN items only, as of 2026-08-15.** It had grown to 78
completed entries against 48 open ones, so it no longer answered the question it
exists to answer. The completed entries were moved verbatim to `JOURNAL.md`
under `## 2026-08-15 -- Completed TODO entries, moved out of TODO.md verbatim`,
grouped by the section they came from. Nothing was summarised or dropped: the
move was verified line-by-line, and their in-file cross-references ("the item
below", "original entry below") were deliberately left unrewritten, so some now
point at neighbours that stayed here. When you finish an item, move it there
rather than checking a box here.

> **Re-verify every entry against the tree before acting on it.** This is not
> boilerplate here. Much of this tree was reconstructed rather than recovered, so
> an entry can describe an intent the code never reached; and other agents share
> this working directory, so an entry can have been closed since it was written.
> An item that is still open after you have checked the code is a task. An item
> you have only read is a hypothesis.

**Consolidated on 2026-08-09.** The entries below combine `README.md`'s own
"Not there yet" / "Next, in order" lists with open work extracted from the full
`JOURNAL.md`. Every item still requires re-verification before implementation.

## Correctness

- [ ] **(historical framing, kept for the trail) the same bug read as a capacity
      problem for three fixes.** The per-allocation profile settles it. Opening one collator walks
      malloc's arena ladder 0.75, 1.5, 3, 6, 12, 24, 48, 96 MiB -- every rung
      granted, every superseded arena reclaimed -- and then asks for 192 MiB,
      and would ask for 384 next:
      ```
      [mmap] pid=11 0x16050000..0x1c050000 len=100663296
      [ecv] munmap 0x13050000 len=50331648 -> reclaimed; bump 0x1c050000, 1 hole(s)
      [ecv] mmap region exhausted (want 201326592 bytes, bump 0x1c050000, ...) -> ENOMEM
      FATAL:  could not open collator for locale "ak-GH"
      ```
      Native postgres imports all ~800 locales in about a second in a few MiB.
      The guest is allocating ~192 MiB of LIVE data for one `ucol_open`, so no
      arena size fixes it -- doubling means each extra megabyte buys at most one
      more locale, which is precisely the observed 73 MiB -> locale 1,
      202 MiB -> locale 2 scaling. The shape (allocate per iteration, never
      free) is a guest loop that does not terminate or a size computation that
      is wrong, i.e. the same class as the nginx runaway recursion. Point
      `RAPTORMARK_ECV_COUNTRET` / `_DTRACE_LO/_HI` at ICU's collator path, or
      build the wazero fntrace harness in the plan file, and find the function.
      ❌ Do not size the arena again for this. Verified 2026-08-11.
- [x] **DECIDED 2026-08-22, BY THE USER: bounded snapshots are the ONLY scheme,
      and the flag is GONE.** Do not re-litigate this entry; it is kept for the
      evidence trail and for the one thing the ruling did NOT close (below).

      The ruling was made on the CEILING, not on the switch cost: without
      bounded snapshots there are no guest-side clients at all. Made the default
      the same day, then the environment gate was removed outright a few hours
      later on a second, explicit ruling. Removed with it:
      `diag::bounded_snapshots()`, `diag::bounded_on()`,
      `diag::bounded_requested()`, `diag::set_bounded()`, `BOUNDED_FLAG`, the
      `sys::init_diag_flags` wiring, and the seven host tests that existed only
      to pin the gate's polarity and spelling.

      **KEPT, deliberately**: `SnapshotData::Full`, `Arena::snapshot`, and the
      `Full` arm of `Arena::restore_in_place`. A **multi-threaded group** must
      still take a full snapshot, because a bounded range set derives from ONE
      stack pointer and a sibling thread's stack is live memory that pointer
      says nothing about. `EcvContext::snapshot_for` now tests
      `is_multithreaded` and nothing else -- a property of the process, not a
      mode. Deleting these breaks threads. `Arena::swap_with` and
      `Arena::adopt_shared_from` are also kept but are no longer on any switch
      path; only their tests call them.

      `diag::tests::the_removed_snapshot_gate_stays_removed` scans every `.rs`
      under `runtime/src` -- RAW source, comments and string literals included,
      deliberately unlike the other source scans -- and fails if the variable
      name reappears anywhere.

      The three paired E2E guards (`uds`, `divergemem`, `crossprog`) COLLAPSED
      back to one test each. With no flag both halves ran the identical scheme
      while reading, from their names, as two-scheme coverage; each survivor
      carries a comment naming the twin it absorbed and why.

      ⚠️ **STILL OPEN, accepted rather than closed, and now HARDER to close:
      the stack below `sp`.** Up to 1,307 bytes observed on nginx, outside the
      copied range set. Dead frames under AAPCS64 (aarch64 has no red zone), the
      asyncify interaction has not been looked at, and the figures are SAMPLES
      rather than bounds. Accepted by the user as part of the ruling. Removing
      the flag makes it **unsettleable by differential**: there is no unbounded
      run left to compare against, so `RAPTORMARK_ECV_SNAPCHECK` reports
      `NO-ORACLE` for every single-threaded switch instead of a fabricated
      `miss=0`. Closing this needs an ABI argument or an instrumented guest, not
      an A/B run. That is a real narrowing of the options and was taken
      knowingly.

      Original entry follows.
- [x] **Decide whether bounded snapshots stop being opt-in.** (Superseded by the
      entry above; the "ONE open question" it names was ACCEPTED, not answered.
      ⚠️ The environment variable this entry names was REMOVED on 2026-08-22 --
      it is history, not an instruction, and setting it now does nothing.)
      They were behind
      `RAPTORMARK_ECV_BOUNDED=1`. **The decision is now down to ONE open
      question** (see `JOURNAL.md`, "nginx under bounded snapshots", 2026-08-18).

      For: 47 unit tests; four e2e guests on both schemes; a postgres ladder to
      8 concurrent clients with no lost writes; **nginx, 4 workers, 100 requests
      under `RAPTORMARK_ECV_BOUNDED=1`, byte-identical to the unbounded run**
      (2026-08-18 -- this was the "has never run under it" line in the old
      version of this entry); and switch cost measured at 79 us cross-program /
      66 us same-program against 42/35 unbounded.

      Also for, and it changes the shape of the question: without bounded,
      shipping means no guest-side clients at all -- unbounded dies with
      `memory allocation of 402653184 bytes failed` the moment psql forks as a
      fourth process (item below).

      Against, and it is now the whole of it: **the stack below `sp` is outside
      the range set**, and nginx raises the observed figure to 1,307 bytes from
      the ~451 measured on postgres. The argument for leaving it uncopied is that
      those are dead frames under AAPCS64 (aarch64 has no red zone), which is
      almost certainly right -- but it is a ruling nobody has made explicitly, the
      asyncify interaction has not been looked at, and the numbers above are
      SAMPLES rather than bounds. Everything else the snapcheck probe once
      reported is now clean or unobservable: `brk` reports 0 misses, and mmap
      differences lie outside the incoming process's live extents, where
      `Arena::mmap_reserve`'s zero-fill makes them unreadable.

      ⚠️ One limit on the evidence: with `RAPTORMARK_ECV_SNAPCHECK=1` on, nginx
      never reaches `accept` (the probe is a 384 MiB compare per switch), so the
      snapcheck numbers cover startup and worker fork, NOT request handling. The
      bounded RUN covers request handling; the safety PROBE does not. Retried
      with ONE worker in case fewer processes meant fewer switches -- same
      outcome, so this is a property of the probe rather than of the workload.
      Verified 2026-08-18.
- [ ] **Bounded arena snapshots: the design, grounded in the code.** This is the
      one change that lifts the concurrency ceiling, and it is now the binding
      constraint on postgres -- measured 2026-08-12, one guest-side client costs
      SEVEN concurrent arenas (dash, postmaster, checkpointer, bgwriter,
      walwriter, backend, psql), and 384 MiB allows about ten.

      **Today**: every non-running process owns a FULL-SIZE `Vec<u8>`, and
      `Arena::swap_with` trades buffers rather than copying (`core::mem::swap`
      on five fields). That is O(1) per switch and was a deliberate trade: the
      previous copy-based scheme measured `snapshot` 19.98 ms and `restore`
      17.05 ms, ~37 ms per switch, 95% of nginx's request wall clock. So the
      current design bought latency with memory, and the memory has run out.

      **The proposal**: keep ONE live arena and copy back only what a process
      can actually have modified, which is far smaller than 384 MiB:
        - the WRITABLE PT_LOAD segments of its image (.data/.got/.bss). The
          runtime already has the program header table -- `EcvProgram.e_ph`,
          `e_phent`, `e_phnum` -- and already walks it for PT_TLS in
          `abi.rs:tls_phdr`, so the writable ranges need no new plumbing. .text
          and .rodata are identical in every process and never need saving.
        - `[BRK_START_VMA, brk_cur)`, already tracked.
        - the live private mmap extents, already tracked exactly, in
          `Arena::mmap_live`.
        - the used stack, `[sp, STACK_TOP_VMA)`.
      Shared segments are already exempt from save/restore, so they are
      untouched by this.

      **Why it should be affordable**: the 37 ms measurement was for 384 MiB in
      each direction. The dirty set above is tens of MiB for a postgres backend,
      so the copy is proportionally cheaper -- but that is arithmetic, and this
      tree has been wrong about arithmetic before. MEASURE the dirty set on a
      real backend before building it.

      **The risks, in order**: (1) a write outside the assumed ranges is silent
      corruption, not a crash. **MEASURED 2026-08-12 with
      `RAPTORMARK_ECV_SNAPCHECK=1` over 48 same-program switches, and the range
      set above is INCOMPLETE in exactly two ways, both now identified by
      address:**

        - **The TLS area is missed, and it straddles the image base.** Diffs land
          at `0xff9d8` (5 bytes, in about half the samples) and at `0x100040`
          (1 byte, once). `THREAD_PTR` IS the image base, 0x100000: the 16-byte
          TCB sits at [0x100000, 0x100010) with the static TLS blocks after it,
          and the runtime writes pthread/dl bring-up state just BELOW at
          0xff9a0..0xff9e0. None of it is in a PT_LOAD, all of it is
          per-process. The `0x100040` byte is 64 bytes past the thread pointer
          -- inside the TLS block, not image data; it only looked like an image
          diff because the two regions share a start address. FIX: add
          `[floor_below_scratch, THREAD_PTR + TCB_SIZE + tls_memsz)` to the range
          set; `tls_phdr()` already supplies `memsz`. A few KB.
        - **The stack below `sp` differs, up to ~451 bytes.** Dead frames by
          AAPCS64 (aarch64 has no red zone), so it is almost certainly safe to
          leave uncopied -- but that is a ruling to make explicitly, and the
          asyncify interaction deserves a second look before it is made.

        - **Unmapped memory must be ZEROED, or a bounded snapshot leaks data
          between processes.** brk differs above the incoming process's
          `brk_cur` (33 KB, 9 of 59 samples) and mmap outside its live extents
          (257 KB, 39 of 59). Neither is read while unmapped, but Linux
          guarantees a `brk` growth and a fresh `MAP_ANONYMOUS` come back zeroed,
          and under a bounded snapshot they would hold the previous process's
          bytes. Today the per-process arena makes this impossible; bounded
          snapshots must add explicit zero-fill on brk growth and mmap
          allocation. (An earlier note here claimed brk and mmap were always
          byte-identical -- that came from 13 samples and was wrong.)

          **✅ DONE, and this entry was stale until 2026-08-18.** Both zero-fills
          are implemented one level below the syscall, in `Arena::mmap_reserve`
          and `Arena::set_brk`, each carrying a comment that cites the 39-of-59
          and 9-of-59 measurements above. Confirmed on nginx: `brk` now reports
          **0** snapcheck misses, and the mmap differences that remain are
          outside the incoming process's live extents, i.e. in memory no later
          mapping can read un-zeroed. ⚠️ Reading `NR_MMAP` alone says the
          opposite -- it hands back `state.set_ret(at)` with no fill -- so check
          the allocator before concluding this is open again.

      Cross-program switches differ by the whole image (57 MiB), so a program
      change has to re-materialise it from the module's static data -- cheap,
      needs no per-process storage, but a snapshot that ignored it would corrupt
      across every execve; (2) `swap_with` changes the live arena's ADDRESS today and every
      holder of `base_ptr` re-reads it per scheduler leg, so a copy-based scheme
      must not quietly reintroduce a stale-pointer assumption in the other
      direction; (3) the asyncify-saved stack bakes in the fixed arena base, so
      the live buffer's address must stay fixed, which a single live arena gives
      for free.

      **The cheap first step is DONE, and it says GO** (2026-08-12).
      `RAPTORMARK_ECV_SNAPSTAT=1` reports each process's bounded-snapshot size at
      every switch (`Arena::bounded_snapshot_bytes`). Measured over 102 switches
      of a real postgres workload -- initdb, postmaster, background workers,
      backend, psql: median 2 MiB, p90 3 MiB, **max 6 MiB of 384, i.e. 1.6%**.
      Worst sample: image_w 1.9, brk 3.7, mmap 1.1 MiB, stack 30 KiB. So a
      suspended process can cost ~60x less, and the copy is well under a
      millisecond against the ~37 ms the old whole-arena scheme paid.
      ⚠️ The first version of this probe read `mmap_live` as (start, end) when it
      holds (start, LENGTH) and reported 17592186044163 MiB -- a u64 underflow
      that looked like a plausible measurement. Fixed, and covered by
      `bounded_snapshot_bytes_sums_the_ranges_it_claims_to`.

- [ ] **⭐ DECIDE what ecvisor does about SPECULATIVE RESERVATIONS. This is the
      cross-cutting question the four entries around it are each one face of.**
      Raised 2026-08-22 after three independent guests hit it in one day. Full
      synthesis with measurements in
      `.agents/docs/LTM/ecvisor-runtime-synthesis.md`, "The Address Budget".

      **The constraint in one line: a mapping IS address space in a fixed linear
      memory, so a reservation costs exactly what an allocation costs -- while
      natively it costs almost nothing.** Ruby's shape cache is the measurement
      that makes it concrete: `/proc/self/smaps` reports **RSS 28 kB against a
      384 MiB mapping**, about 14,000 to 1. A guest cannot tell it has crossed
      that line.

      The same constraint, four ways, all measured:

      | guest | asks for | outcome |
      |---|---|---|
      | ruby object-shape cache | 402,653,184 B (= `MEMORY_ARENA_SIZE` exactly) | refused; degrades, **2.7x** on ivar reads at >= 10 ivars |
      | ruby `Init_default_shapes` (the other one) | 20,971,520 B | succeeds with **~74 MiB** of window left; `rb_memerror()` if it ever fails |
      | YJIT | 128 MiB into a **96 MiB** window | can never succeed; ruby aborts |
      | PostgreSQL `MAP_SHARED` | shares the same 96 MiB with private mmap | starves malloc's mmap fallback |

      **Why it is a decision and not work**: every available answer is a trade
      nobody has been asked to make -- grow `MEMORY_ARENA_SIZE` (costs every
      guest's linear memory, and 384 MiB is already what a bounded snapshot was
      built to survive); split the shared and private windows (the entry below);
      make reservations lazy so untouched pages cost nothing (a real memory-model
      change, and the one that would fix all four at once); or accept the ceiling
      and document which images are out of reach.

      ⚠️ **The trap, and it has already caught one investigation.** Ruby's
      402,653,184 collides with `MEMORY_ARENA_SIZE` **by coincidence** -- it is
      `(0x80000 * 32) * 24` from ruby's own headers, not anything read from us.
      Reading the collision as causal sends the work into the runtime instead of
      into the guest's arithmetic. Get the SIZE and its provenance before
      touching the arena.

- [ ] **The shared window and the private mmap arena are the same 96 MiB.**
      (Still 96 MiB as of 2026-08-12: the arena reverted to 384 MiB, so
      MMAP_START 0x10000000 .. MMAP_END 0x16000000 is unchanged from when this
      was written.)
      A large `MAP_SHARED` therefore starves malloc's mmap fallback: with
      `shared_buffers` at 76 MiB the private side is ~20 MiB. Splitting them, or
      at least sizing the shared window from a policy rather than letting it eat
      the window top-down, is what makes the item above tractable without simply
      buying more arena. Verified 2026-08-11.
- [ ] **`fmov <Vd>.2S, #imm` is unlifted.** Patch 0010 covers only the 2D
      vector-immediate form (`FMOV_ASIMDIMM_D2_D`); the S form is a decode stub,
      and `fmov v1.2s, #1.0` is what a compiler emits for `v2f x = {1.0f,1.0f}`.
      Found because it killed a test guest before any of its checks ran.
      Verified 2026-08-11.
- [ ] **A by-element 2S multiply cannot express lanes 2 and 3.** The index is
      H:L regardless of Q, so `fmul v0.2s, v1.2s, v2.s[3]` is legal, but the
      semantics take both operands as the same arrangement and a 64-bit view has
      only two lanes. Patch 0045 makes the decoder REFUSE those rather than read
      out of range (loud beats silently wrong), which means such an instruction
      now stops the module. Doing it properly needs the element operand read as
      the full 128-bit register and new ISELs taking (v2f, v4f, index).
      Verified 2026-08-11.
- [x] **The pre-wipe postgres fixture ran initdb NATIVELY; we are deliberately
      not doing that.** ✅ **CLOSED 2026-08-22 -- the TARGET this entry set aside
      the workaround FOR has been met, repeatedly.** initdb now runs INSIDE the
      guest: the four-program closure (postgres, initdb, dash, psql) completed
      `BOOT: initdb` -> `BOOT: postmaster` -> real SQL -> `BOOT: DONE` on every
      run taken this session, and it selected `posix` dynamic shared memory
      rather than needing the fixture's `sysv` workaround.

      This entry was never open WORK -- its own text says "Set aside by
      direction" and keeps it only because the recovered fixture documents three
      facts. Those facts remain true and are preserved below; what is no longer
      true is that they describe an outstanding decision. An archival note in an
      open box inflates the count and reads as a task.
      Original text below.

- [x] **(original framing, archival) The pre-wipe postgres fixture ran initdb NATIVELY; we are deliberately
      not doing that.** `_recovery/reference/test-fixtures.txt` records the
      pre-apocalypse shape: `RUN initdb -D /pgdata --auth=trust --no-sync -U
      postgres` at image BUILD time, `pg_checksums --disable`, then an
      ENTRYPOINT running only the server with `dynamic_shared_memory_type=sysv`,
      `shared_buffers=16MB`, `fsync=off`, `max_connections=10`, unix socket
      only, `LANG=C`. That is why ICU's collation import was never in the lifted
      path, and it is why postgres "worked" pre-wipe. Reproduced as a Dockerfile
      and confirmed to serve natively (2026-08-11).
      **Set aside by direction**: running initdb inside the guest is the target,
      not the workaround. Kept here because the fixture also documents three
      facts worth having -- the recovered fixture is postgres 18 (checksums on
      by default, hence `pg_checksums --disable`; `io_method` is 18-only), the
      16 MiB shared_buffers matches what the arena arithmetic independently
      demanded, and `dynamic_shared_memory_type=sysv` sidesteps the POSIX DSM
      path entirely.
- [x] **The exec map silently falls back to program 0, and three separate
      mistakes have hit it.** ✅ **CLOSED 2026-08-21 -- both fixes the body
      describes are dated DONE 2026-08-12 and the box was never ticked.**
      Re-verified: `internal/link/manifest.go` has `WriteManifest`, so the link
      writes `programs.json` and the sidecar build reads it, naming programs by
      INDEX -- the second derivation this entry blamed is gone, and no hash is
      derived or typed twice. The runtime's unconditional warning for an
      exec-map entry naming a hash the registry lacks is the other half, and was
      verified in both directions on the day (a stale id warns, a correct
      sidecar is silent). Eight Go tests cover it.

      The "what remains" the body names -- a sidecar built against a DIFFERENT
      module -- is precisely what the warning exists for, so it is covered rather
      than outstanding. Original text below.

- [x] **(original framing) The exec map silently falls back to program 0, and three separate
      mistakes have hit it.** A hash the runtime cannot match is dropped
      WITHOUT a diagnostic, so the guest runs the wrong program and the symptom
      appears wherever that program first disagrees with its argv. Seen as
      `unrecognized configuration parameter "username"` (postgres running
      initdb's flags) and as `dash: invalid argument: "/run-pg.sh"` with
      postgres's `Try "%s --help"` format under dash's argv[0].
      The three causes so far, all avoidable:
      1. a re-lift changed the module IDs and the sidecar was stale;
      2. a non-canonical guest path (`/bin/dash` on a usr-merged image, where
         the resolved path is `/usr/bin/dash`);
      3. the sidecar generated with a DIFFERENT builder tag than the one whose
         objects are in the registry -- and note the identity moves for reasons
         beyond the lifter: renaming `0046-fcm` to `0047-fcm` changed the
         patched-base image content, hence its ID, hence every module ID.
      Fix at the source: the runtime now WARNS, unconditionally, when an
      exec-map entry names a hash the registry does not contain -- naming the
      path, the missing hash and the registry's contents (DONE 2026-08-12,
      verified in both directions: a stale id warns, a correct sidecar is
      silent). AND the second derivation is gone (DONE 2026-08-12): the link
      writes `programs.json` (`link.WriteManifest`) and the sidecar build reads
      it, naming programs by INDEX (`-map path=3`), so no hash is derived or
      typed twice. A manifest-built sidecar is byte-identical to the
      hand-derived one; a bad index fails at build time. Eight Go tests cover
      it. What remains is only the case a manifest cannot help: a sidecar built
      against a DIFFERENT module, which is what the runtime warning is for.
      Verified 2026-08-12.
- [x] **Next postgres milestones, in order.**
      ✅ **CLOSED 2026-08-21 -- every rung on the ladder is struck through and
      dated, and the box was simply never ticked.** All four: the `postgres
      --single` query (2026-08-12, later AMENDED because `SELECT 6*7` is
      constant-folded, and re-cleared through the real PLANNER by
      `patches/0064`), the postmaster with its background workers (2026-08-12,
      an entry that was itself corrected for having been marked stale while item
      3 below it already implied it), a CLIENT both over TCP and in-guest via
      `psql` over `/tmp/.s.PGSQL.55432`, and DDL/DML/hash-aggregate/index-scan/
      json (2026-08-19, needing `patches/0065`).

      ⚠️ A closed LADDER is not a finished postgres. What this closes is "these
      four rungs", nothing wider. The durable lesson from rung 1 is the one to
      carry: **any future postgres validation must plan over a real relation**,
      because a constant-folded query never enters the planner and certified
      less than it appeared to. Original text below.

- [x] **(original framing) Next postgres milestones, in order.** initdb completing is not a
      server. The remaining ladder, cheapest first:
      1. ~~A QUERY through `postgres --single`.~~ DONE 2026-08-12.
         `SELECT 6*7 AS answer` returns 42 from a stand-alone backend, after
         initdb, in one module lifetime driven by `dash /run-pg.sh` -- and the
         shutdown checkpoint completes (3 buffers, lsn=0/14EC758), so the WAL
         path works too. No new lift was needed.

         ⚠️ **AMENDED 2026-08-19: `SELECT 6*7` is CONSTANT-FOLDED and never
         enters the planner**, so this rung proved less than it reads. The
         PLANNER path -- seq scan, join, sort over real catalog relations --
         was blocked on an undecoded `fnmul` in `get_variable_numdistinct` and
         is now DONE too, by `patches/0064-fnmul-and-fnmadd.patch`:

             PG-RESULT rc=0 const=1 seqscan=1 join=1 order=1
             nclass = "415"   njoin = "415"   relname = "pg_aggregate"

         same values as the native run, same `lsn=0/14EC758`. Any future
         postgres validation should plan over a real relation for this reason.
      2. ~~The POSTMASTER via `pg_ctl start` or `postgres -D`.~~ **DONE, and
         this entry was STALE.** A different code path entirely -- fork of
         background workers (checkpointer, walwriter, autovacuum launcher), the
         latch/WaitEventSet machinery, signal plumbing, and unix-socket
         listen/accept. The nginx work covered sockets; the background-worker
         supervision is untried.

         ⚠️ **Corrected 2026-08-19 by reading, not by re-running.** The text
         above says the supervision is untried while item 3 BELOW it is struck
         through and describes psql running DDL/DML *against the postmaster*
         over `/tmp/.s.PGSQL.55432` -- which cannot happen without one.
         `.agents/docs/LTM/postgres-and-guest-concurrency.md` settles it: "a
         postmaster with its background workers", and **8 simultaneous clients,
         8/8 exiting 0 and 8/8 rows committed**. The rung was cleared on
         2026-08-12 and only item 3 was struck through. Original text kept so
         the list of what the milestone COVERS is not lost.
      4. ~~DDL, DML, a HASH AGGREGATE, an index scan and json.~~ DONE
         2026-08-19, needing `patches/0065-scalar-sisd-add-sub.patch`
         (`ExecInitAgg` stopped on an undecoded scalar `add d1, d1, d31`):
         `PG-RESULT rc=0 groupby=1 index=1 avg=1 json=1`, every value matching
         the native run, `av = "375.470"` computed after the UPDATE and DELETE.
      3. ~~A CLIENT.~~ DONE 2026-08-12, both ways. Over TCP with a host-side
         `psql`, and then IN THE GUEST: `psql` is lifted as a fourth program
         (13m27s to translate) and runs DDL/DML against the postmaster over
         `/tmp/.s.PGSQL.55432` with `listen_addresses` empty, so no TCP
         listener exists and the in-runtime AF_UNIX path carries every byte.
      Verified 2026-08-12.
- [ ] **`fuse.Options.Extra` handles ONE dlopen'd plugin, and does not
      generalise.** Added 2026-08-11 so `dict_snowball.so` reaches the closure --
      nothing DT_NEEDEDs it, and an AOT closure gets no second chance because
      `dlopen` answers with a sentinel handle and `dlsym` resolves through
      `.ecv.dlsyms`, which lists only what was fused. A missing module therefore
      "loads" and has every symbol resolve to NULL, which postgres reports as
      `missing magic block` -- a version mismatch by appearance, an absent object
      in fact.
      It does not scale to postgres's other 78 modules: every extension defines
      `Pg_magic_func`, `_PG_init` and `pg_finfo_*`, so a second module collides
      in the flat namespace and first-wins binds the wrong one silently. Our
      `dlsym` compounds it by ignoring the handle and resolving globally, so even
      separate storage could not disambiguate. Generalising needs per-module
      namespacing in the fuser plus a handle-aware `dlsym` in the runtime.
      Verified 2026-08-11.
- [ ] **⚠️ THE TITLE BELOW IS NOW FALSE, and the entry is PARTLY closed.**
      Re-measured 2026-08-22 against the e2e run on
      `raptormark-builder:sweep0821c`. When this was written only
      `e2e/fcvtvec_test.go` existed. There are now **EIGHT differential pairs**,
      each an under-ecvisor test beside a native baseline on the same guest:

      | family | tests |
      |---|---|
      | FCVT directed rounding | `TestFCVTDirectedRounding{UnderEcvisor,NativeBaseline}` |
      | FCVT vector | `TestFCVTVector{UnderEcvisor,NativeBaseline}` |
      | pairwise widening add | `TestPairwiseWideningAdd{UnderEcvisor,NativeBaseline}` |
      | by-element multiply | `TestVectorByElementMultiply{UnderEcvisor,NativeBaseline}` |
      | vector integer | `TestVectorInteger{UnderEcvisor,NativeBaseline}` |
      | round + int-to-float | `TestVectorRoundAndIntToFloat{UnderEcvisor,NativeBaseline}` |
      | vector sign | `TestVectorSign{UnderEcvisor,NativeBaseline}` |
      | TBL table lookup | `TestTBLTableLookupMatchesNative` |

      All 15 pass. So "nothing checks the rest" is wrong; what is true is the
      SECOND half of this entry -- the coverage is still per-family and added
      one patch at a time, not the **systematic** sweep it asks for ("run each
      ASIMD form under ecvisor and natively on the same inputs, with lanes that
      DIFFER, and diff").

      ⚠️ Keep the load-bearing clause when this is eventually done: **lanes that
      differ**. Every one of the four original bugs was invisible with equal
      lanes. Original text below.

- [ ] **(original framing, title now false) Nothing checks the rest of the vector semantics.** Three of the four
      bugs above were in code that had been there all along and was simply never
      executed by a test: a vector op with a FLOAT destination was wrong in
      every form tried until 0043/0044. `e2e/fcvtvec_test.go` now covers
      FCVT/FRINT/SCVTF/UCVTF and, as controls, `fadd`/`fmul`/`umov`/`mov s,v.s[]`
      -- that is a small corner of `SIMD.cpp`. The cheap systematic move is a
      differential guest: run each ASIMD form under ecvisor and natively on the
      same inputs, with lanes that DIFFER, and diff. Lanes that differ is the
      load-bearing part; every one of these bugs was invisible with equal lanes.
      Verified 2026-08-11.
- [ ] **The shared window's floor is the RUNNING process's `arena.mmap_cur`.**
      That bump travels with the arena, so another process may already sit
      higher than the one allocating -- a shared region can in principle be
      carved over a private mapping belonging to somebody else. Not observed;
      the honest floor is a high-water mark across all processes, which costs
      window space for everybody and so was left out of the reclamation change.
      Verified 2026-08-11.
- [ ] **`munmap` of part of a shared region is ignored.** Only an unmap whose
      address equals the region start counts; a partial unmap would have to split
      the region, which neither `ShmWindow` nor `adopt_shared_from` can express.
      Ignoring it leaks, which is the safe direction, but a guest that resizes a
      DSM segment by unmapping its tail will slowly consume the window.
      Verified 2026-08-11.

      ⚠️ **CORRECTION, 2026-08-21: the start-only match was ignoring the TAIL
      case and HONOURING the head case.** `NR_MUNMAP` never looked at `len`, so
      `munmap(region, 4096)` on a 16 MiB region dropped the caller's claim and,
      as the last mapper, released all 16 MiB to the window while the caller
      still had the rest of it mapped -- the recycling-memory-somebody-still-
      reads direction the entry says it avoids. FIXED: the arm now requires the
      page-rounded length to cover the whole region
      (`SharedSeg::unmap_is_whole`, `runtime/src/arena.rs`), so a head unmap
      joins the tail case and leaks instead. Two host regression tests, both
      neutralized.

      **The leak itself is deliberately still open, and is bigger than it
      looks.** A split cannot be expressed by ONE of the three structures alone:
      `SharedSeg.mappers` is a set of pids with no extents, so a split cannot say
      which process holds which half; `shm_files` keys a POSIX region by its
      start VMA (the name other processes map it by); and `ShmSeg.vma` keys a
      SysV segment the same way (the shmid `shmdt`/`shmctl` find it with).
      `ShmWindow::release` and `adopt_shared_from` would both cope with a
      sub-range as they stand. Reachability is low in this tree: postgres detaches
      whole mappings in every DSM backend, the recovered fixture pins
      `dynamic_shared_memory_type=sysv` (which goes through `shmdt`, exact-start
      by construction), and no e2e guest performs a partial unmap.
- [ ] **ecvisor aborts where the kernel raises a SIGNAL.** The errno half of this
      is done (see `JOURNAL.md`, "Every `fatal!` audited"): all 32 sites were
      classified and the 4 in `NR_MMAP` that a kernel answers with an errno now
      do, leaving 28 that a kernel has no way to report because no syscall is in
      flight. Four of those 28 are the exception, and they are a DIFFERENT fix:
      Linux reports them by killing the PROCESS with a signal, while ecvisor
      kills the whole module.

      | site | Linux |
      |---|---|
      | `__remill_error` (undecodable opcode, guest `brk`) | SIGILL / SIGTRAP |
      | `__ecv_wild_store` (store outside the arena) | SIGSEGV |
      | `report_runaway_recursion` | SIGSEGV (stack overflow) |
      | `run_signal_handler`, handler not lifted | SIGSEGV |

      **The pattern already exists in the tree**: `__ecv_warning` posts SIGILL to
      the faulting thread and falls back to `fatal!` only when the guest has no
      handler. `__remill_error` sits ten lines above it and does not.
      ⚠️ Not obviously worth doing. `__remill_error`'s own comment records why it
      is loud -- `brk` lifted as a no-op fell through and surfaced hundreds of
      instructions later as an unrelated null call -- and a guest that swallows
      SIGTRAP would restore exactly that. Weigh before implementing.
      Verified 2026-08-18.
- [x] **`mmap(NULL, 0, ...)` succeeds; Linux returns EINVAL.** DONE 2026-08-21.
      Both length rules now live in `arena::mmap_round_len` -- EINVAL for zero,
      ENOMEM for a length whose page alignment overflows -- checked at the top of
      `NR_MMAP`, ahead of the MAP_FIXED branch because `do_mmap` checks `!len`
      ahead of everything. It is in `arena` and not `sys` because `sys` is
      `#[cfg(target_arch = "wasm32")]` and so unreachable from `cargo test`; the
      same seam `madvise_zeroes` uses. Three unit tests in `arena::tests`, and
      `zerolen`/`hugelen` cases in `e2e/mmapfail_test.go` (raw syscall, so a libc
      cannot answer them first). **CORRECTION to the estimate above:** the
      overflow is not "a colossal request becomes a small one" -- the wrapped sum
      is always below `GUEST_PAGE_MASK`, which the mask then clears, so every
      overflowing length rounded to exactly 0. The two bugs therefore converged
      on the same zero-byte mapping by different routes, and a `len == 0` guard
      alone does NOT subsume the overflow, because that zero appears after the
      rounding. Native aarch64 Linux 6.17 was probed directly: EINVAL(22) and
      ENOMEM(12). The e2e cases themselves are UNRUN (no Docker in that session).
- [x] **A fused DYNAMIC glibc image cannot create a thread.**
      ✅ **CLOSED 2026-08-22.** The entry's own chain is complete: the default
      pthread attr, `_dl_tls_static_size`/`_dl_tls_static_align`, and finally the
      **ld.so hook cluster** -- which the body identifies as the last blocker and
      prescribes the fix for ("identify that function STRUCTURALLY ... emit its
      entry VMA ... and CALL it at bring-up the way `apply_early_init` calls
      `__libc_early_init`"). That is implemented: `runtime/src/context.rs:4985`
      logs `ld.so hook installer ran: 0x{init:x}`, and it was observed firing
      TWICE in a real fused-glibc postgres run and in python
      (`ld.so hook installer ran: 0xe1064c`, `0xe14bc0`).

      Verified by the tests rather than by the log alone -- all green in the
      2026-08-22 e2e run on `raptormark-builder:sweep0821c`:

      | test | result |
      |---|---|
      | `TestRTLDHooksUnderEcvisor` | PASS (5.45 s) |
      | `TestTLSDescUnderEcvisor` | PASS (6.52 s) |
      | `TestThreadsUnderEcvisor` | PASS (2.70 s) |

      `e2e/tlsdesc_test.go` is the reproducer this entry names, and it needs TWO
      THREADS in a fused dynamic glibc image to say anything -- which is exactly
      the capability the title says is missing. It passes ungated, with native
      baselines beside it. Original text below, kept for the six-fix trail.

- [x] **(original framing) A fused DYNAMIC glibc image cannot create a thread:
      `__default_pthread_attr.stacksize` is zero.** Found 2026-08-15 by the
      TLSDESC probe, which needs two threads to say anything.

      ```
      check tlsdesc-through-a-shared-object
      Fatal glibc error: allocatestack.c:335 (allocate_stack): assertion failed: size != 0
      ```

      ld.so sets the default stack size from RLIMIT_STACK during
      `__pthread_initialize_minimal`; a fused image enters the executable's
      `_start` and never runs it, so `allocate_stack` asserts. This is the exact
      counterpart of musl's `libc.tls_*` (closed 2026-08-15) on the other libc,
      and it hid for the same reason: `e2e/threads_test.go` passes because a
      STATIC glibc guest does that setup in its own `__libc_start_main`. Every
      threading guard in this tree is static glibc or fused musl; nothing
      covered fused glibc until now.

      **HALF DONE 2026-08-15, and the other half is somewhere else.**
      `EcvContext::apply_pthread_attr_default` now gives glibc a default thread
      stack by calling glibc's OWN exported API at bring-up -- `pthread_attr_init`,
      `pthread_attr_setstacksize`, `pthread_setattr_default_np` -- so it pokes
      no struct and needs no layout evidence at all, unlike the two musl seeds.
      Size is 1 MiB, `RAPTORMARK_ECV_THREAD_STACK` overrides; the platform
      answer (32 MiB, glibc's aarch64 default when RLIMIT_STACK is infinite,
      which is what this runtime reports) would allow TWO threads in the 96 MiB
      guest mmap window.

      It is INSTALLED AND CONFIRMED -- the seed reads it back through
      `pthread_getattr_default_np` + `pthread_attr_getstacksize` and refuses to
      claim success unless glibc reports the value it was given:

      ```
      [ecvisor] pthread default stack seed: 1048576 bytes (RAPTORMARK_ECV_THREAD_STACK)
      Fatal glibc error: allocatestack.c:335 (allocate_stack): assertion failed: size != 0
      ```

      **So the assert is NOT about the default attr.** glibc reads our value and
      still computes zero, which puts the fault downstream -- the prime suspect
      being `size &= ~__static_tls_align_m1` in `allocate_stack`, where
      `__static_tls_align_m1` is `GLRO(dl_tls_static_align) - 1`. ld.so sets
      `_dl_tls_static_align`/`_dl_tls_static_size`; a fused image leaves them 0,
      and `0 - 1` is all-ones, so ANY size ANDs to zero. That is the glibc
      counterpart of musl's `libc.tls_size`/`tls_align`, which is a shape this
      tree has now seen three times.

      **CONFIRMED AND FIXED 2026-08-15, and it revealed a third layer.**
      The experiment settled it: with the guest passing an EXPLICIT attr
      carrying its own 1 MiB stacksize, glibc still asserted -- so the default
      attr was exonerated and the fault was downstream, as predicted.

      `_dl_tls_static_size` and `_dl_tls_static_align` are now decoded by the
      prelinker (`glroTLSStaticVMAs`, words 10-11 of `.ecv.stacklists`) and
      seeded from the image's own TLS geometry. The offsets come from
      `__pthread_get_minstack`, whose whole body is
      `roundup(GLRO(dl_tls_static_size), GLRO(dl_tls_static_align)) +
      GLRO(dl_pagesize) + PTHREAD_STACK_MIN`, with three conditions or nothing
      is emitted: the GOT slot must hold `&_rtld_global_ro`, there must be
      exactly ONE such `ldp`, and the neighbouring plain `ldr` must name the
      `_dl_pagesize` offset that `__getpagesize` gives INDEPENDENTLY.

      ```
      [ecvisor] _dl_tls_static bring-up: size=320 align=32 (0xa3fd40, 0xa3fd48)
      ```

      The `size != 0` assert is gone. The guest now dies further in:

      ```
      fatal: vma 0x0 not in the lifted function table (__remill_function_call) lr=0xa0f398
      ```

      -- a call through a NULL function pointer, which is almost certainly
      `GL(_dl_rtld_lock_recursive)`: `_dl_allocate_tls` takes the rtld lock, ld.so
      installs those pointers, and a fused image leaves them zero. **That is
      exactly what `.ecv.stacklists` words 6 and 7 were reserved for** ("rtld
      recursive-lock fn-ptr slot" / "ld.so's no-op lock function"), left zero
      since they were written because thread_db does not describe glibc's lock
      members.

      **NOT THE RTLD LOCK -- corrected 2026-08-15 by disassembling the real
      library.** In glibc 2.41 `__rtld_lock_lock_recursive` compiles to a DIRECT
      `bl __pthread_mutex_lock` (seen in `dl_iterate_phdr`), so words 6 and 7
      have no consumer to point at and the null call is something else.

      The crash localises exactly. ld.so is fused at base 0xa00000, so the
      reported `fn=0xa0f360` is `ld.so+0xf360`, and at +0xf394:

      ```
      f368: adrp x1, 40000 <_rtld_global>
      f36c: add  x1, x1, #0xb58        ; GL(dl_tls_static_size)-ish
      f380: adrp x2, 3f000
      f38c: ldr  x2, [x2, #2816]       ; *(0x3fb00) -- an ld.so HOOK POINTER
      f394: blr  x2                    ; <- vma 0
      ```

      0x3fb00 is one of SIX consecutive pointer slots at [0x3fb00, 0x3fb30),
      immediately below `_rtld_global_ro` and immediately above
      `__libc_enable_secure`. They are ld.so's own indirect hooks -- the
      `__rtld_malloc`/`calloc`/`realloc`/`free` family and neighbours -- called
      from 9, 53, 45, 4, 13 and 22 sites respectively. **No relocation covers
      them**: `_rtld_global` and this cluster carry no RELR entries (ld.so's
      whole `.rela.dyn` is one GLOB_DAT), and `_rtld_global`'s file image holds
      six small integers and not one pointer. ld.so installs them at startup, in
      code a fused image never runs.

      Why this never bit before: fused glibc guests here are processes, not
      threads (nginx, postgres), and python's dlopen path is INTERCEPTED by the
      runtime, so real `_dl_open` never runs. `_dl_allocate_tls_storage` on the
      pthread_create path is the first thing to need them.

      ⚠️ Do NOT seed the six slots individually. They are `attribute_hidden`,
      unnamed in dynsym, and installing `free` where `calloc` belongs is silent
      corruption. The scan found a FUNCTION that stores constants into four of
      them (`__rtld_malloc_init_stubs`, unnamed, disassembling after
      `_dl_audit_symbind_alt`) -- so the tractable design is to identify that
      function STRUCTURALLY ("stores constant function addresses into >= 4 slots
      of the cluster, takes no arguments"), emit its entry VMA using the
      function boundaries fusing already recovers, and CALL it at bring-up the
      way `apply_early_init` calls `__libc_early_init`. One call restores the
      whole family instead of six guesses.
      — *source: `2026-08-15 — the ld.so hook cluster`*

      ⚠️ Each iteration here costs a COLD translate (~8-10 min): a producer
      change alters the fused ELF, which changes the object-cache key. Batch
      producer changes before re-running.

      Superseded plan, kept because the reasoning was sound and the conclusion
      was not: settle it by EXPERIMENT rather than by reading glibc.
      Have the guest pass an EXPLICIT attr with a stacksize. That takes the
      default-attr branch out of the picture entirely while leaving the `&=`
      line in it -- so a still-failing run confirms the alignment hypothesis and
      a passing one refutes it. Then seed via `.ecv.stacklists`, which already
      carries `_rtld_global_ro` offsets the prelinker derived by decoding
      `__getpagesize`/`__sysconf`.

      Reproducer: `e2e/tlsdesc_test.go` (fixture builds from local images only;
      the fused guest is 2 MB and translates in ~10 min cold, then hits the
      object cache).
      — *source: `2026-08-15 — TLSDESC, and the glibc seed underneath it`*

- [x] **TLSDESC is verified structurally, not at run time.** EXECUTED 2026-08-15. The stub is emitted,
      lifted (`..._tlsdesc_return_____63583_7423000`) and the descriptors hold
      the right two words, but nothing has yet executed one: the descriptors are
      in `libicuuc`/`libsystemd` and neither `--version` nor `--describe-config`
      reaches ICU collation or `sd_notify`. Confirm with a path that reads a
      thread-local through one of those libraries -- `initdb` or a real backend.
      Verified 2026-08-09. UPDATE 2026-08-11: initdb's post-bootstrap phase now
      executes inside `libicuuc` (it stops on a vector FCVTZS near
      `uenum_reset_76`), so the confirming path exists as soon as that
      instruction lifts.

      **CLOSED 2026-08-15. A descriptor has run.** `e2e/tlsdesc_test.go` passes
      ungated: two threads read a thread-local through a shared object's
      accessors and get DIFFERENT dynamic TLS blocks, which is the observation a
      single-threaded check cannot make. The fixture BUILD asserts the .so
      really carries R_AARCH64_TLSDESC relocations, so the test cannot quietly
      become a general-dynamic test if a toolchain changes dialect.

      Reaching it took six fixes, NONE of them about TLSDESC -- a fused dynamic
      glibc image could not create a thread at all. See the entries above.

      **UPDATE 2026-08-15: no longer blocked on that instruction.**
      `e2e/tlsdesc_test.go` builds a purpose-made fixture -- a shared object with
      thread-locals, read through its accessors from two threads, with the
      fixture BUILD asserting the .so really carries R_AARCH64_TLSDESC
      relocations so the test cannot quietly become a general-dynamic test. It
      fuses, lifts and runs. It is now blocked on the glibc entry ABOVE
      instead: the guest reaches `check tlsdesc-through-a-shared-object` and
      dies in `pthread_create`, before any descriptor executes. Two threads are
      not optional here -- a descriptor resolving to the initial thread's block
      returns the same address to everybody and passes every single-threaded
      check.

- [ ] **The original ~7s multi-worker stall is still unexplained.** It stopped
      firing at the 2026-08-09 fork-model change and does not reproduce on any
      build since, including `epolltmo` which still carries the non-blocking recv
      deadlock. Testing showed the 30-abort reproducer produces ZERO `recvfrom`
      parks -- an aborted client makes recv return EOF, not EAGAIN -- so the
      deadlock fixed this session is NOT its cause, despite two journal entries
      having implied so (corrected in place). The two constraining measurements
      from the original investigation still stand and are still the place to
      start if a ~7s stall ever returns: it self-recovers on its own clock, and
      later traffic does not unstick it. Verified 2026-08-09.

- [ ] **⚠️ NOW BACKEND-SPECIFIC, and the title over-states it.** Re-verified
      2026-08-22. When this was written there was one network backend and the
      limitation was the runtime's. The `NetBackend` seam changed that, and the
      code says so at three sites:

      - `net/mod.rs:267-270` -- `setsockopt`'s `level`/`name` are **Linux**
        numbers, and translating them to a backend's own numbering "is the
        backend's job -- WasmEdge needs it, and **a backend that does not should
        not inherit the limitation**". It then names the limitation exactly:
        "WasmEdge has no TCP level at all, so `TCP_NODELAY` is inexpressible
        there **and nowhere else**."
      - `net/wasmedge.rs:144` -- the same statement at the backend that has the
        problem.
      - `net/browser.rs:18` -- "there is no TCP level in its option set, so
        `TCP_NODELAY` is inexpressible **there** and **expressible here**."

      So what remains open is narrower than the title: nginx's `tcp_nodelay on`
      is inert **under the WasmEdge backend only**, which is still the shipping
      default, so the practical impact is unchanged for the shipping profile.
      The entry's own ask -- "either a WasmEdge-side extension or a measurement
      showing it does not matter" -- is untouched and still the work.
      Original text below.

- [ ] **(original framing, now backend-specific) `TCP_NODELAY` cannot be expressed through WasmEdge's socket options.**
      There is no TCP option level, so nginx's `tcp_nodelay on` is silently
      inert. Unmeasured: at ~10 ms per request Nagle is not obviously biting, but
      it would show on keepalive request pipelining. Needs either a WasmEdge-side
      extension or a measurement showing it does not matter. Verified 2026-08-09.

- [x] **Unhandled syscalls seen in real guests, none yet fatal.** ✅ **CLOSED
      2026-08-21 by re-verification, not by new work** -- all four were
      implemented at some point after this entry was written and it was never
      checked off. Each is a real answer rather than ENOSYS, and each carries a
      comment stating why that answer is the honest one, which is exactly what
      the entry asked to be decided per syscall:

      | syscall | `runtime/src/sys.rs` | what it now answers |
      |---|---|---|
      | 233 `madvise` | `NR_MADVISE` :1388 | zeroes the range on a zeroing advice, else 0 |
      | 216 `mremap` | `NR_MREMAP` :1410 | shrink and in-place grow; EINVAL on a bad range, ENOMEM otherwise -- which is what Linux answers when it cannot extend in place |
      | 283 `membarrier` | `NR_MEMBARRIER` :1458 | 0, with the reason stated: contexts switch only at syscall boundaries, so every other context is already at a barrier by construction |
      | 179 `sysinfo` | `NR_SYSINFO` :1466 | fills `totalram` from `linear_memory_mib` (exact -- it is what the host granted) and zeroes the rest; EFAULT on an out-of-arena buffer |

      The entry's own worry -- "`mremap` is the one to watch: a libc that treats
      the failure as fatal would stop dead" -- is answered by the `NR_MREMAP`
      comment, which records that ENOSYS *was* survivable because both libcs fall
      back to malloc+memcpy+free. Original text below.

      Observed
      2026-08-13/14 with `RAPTORMARK_ECV_DEBUG=1`: **179** (`sysinfo`), **216**
      (`mremap`), **233** (`madvise`), **283** (`membarrier`). Every one returned
      ENOSYS and the guest continued, so none is urgent -- but `mremap` is the
      one to watch: a libc that falls back to alloc-copy-free is merely slower,
      while one that treats the failure as fatal would stop dead, and realloc of
      a large block is a common path. Decide per syscall whether ENOSYS is the
      honest answer (`membarrier` on a single-threaded-at-a-time scheduler
      arguably is) or a gap.
      — *source: `2026-08-14 — capturing the container output`*

- [ ] **852 decoder stubs remain, and stubscan-by-grep does NOT find the useful
      ones.** Counted 2026-08-13. Every target so far has found its own handful
      by DYING on them, at ~30 minutes per round trip.

      ❌ The mnemonic-grep version of this idea was tried and does not work.
      852 stub decoders reduce to 395 distinct mnemonics, of which 126 appear in
      the cryptography image -- including `mov` (1,015,808), `add` (423,997) and
      `str` (244,161). The mapping mnemonic -> decoder is many-to-one and only
      some variants are stubs, so presence proves nothing. The narrower scan that
      produced patch 0056 worked only because it was restricted BY HAND to three
      encoding groups -- the guesswork the tool was meant to remove -- and it
      missed `usubw` and `cmlt`, costing two more lifts.

      ✅ RESOLVED 2026-08-14 as far as *finding* them goes. elflift logs the
      encoding at lift time (patch 0057), `RAPTORMARK_TRANSLATE_VERBOSE=1` tees
      it, and `decode-report -log` names each encoding and extracts
      its fields from QEMU's decodetree. The bitcode route was never needed.

      What remains open here is the second half — *implementing* the named
      instructions — and the ⚠️ below is unchanged by the tooling.

      ⚠️ Do NOT respond by implementing whole encoding groups. Several remaining
      stubs are reciprocal ESTIMATES (`FRECPE`, `FRSQRTE`, `URECPE`, `URSQRTE`)
      whose exact results are hard to verify, and an unverifiable approximation
      in crypto arithmetic is worse than a loud `__ecv_warning`.
      — *source: `2026-08-13 — a dynamically-linked OpenSSL consumer`*

- [x] **Guest signal handlers run only at `ppoll` / `epoll_pwait`.**
      ✅ **CLOSED -- the body already said so on 2026-08-15 and the box was never
      ticked.** Re-verified against the tree 2026-08-21 rather than against the
      prose: `e2e/threadgaps_test.go` records that the `signals` arm's
      `RAPTORMARK_E2E_GAPS=1` gate "came off on 2026-08-15 when the last of them
      closed", both subtests are now REGRESSION GUARDS, and it went
      14 failures -> 11 -> 3 -> 0 across four fixes. `deliver_pending_signals`
      now has 9 call sites in `sys.rs` and 10 in `context.rs`, against the two
      this entry was written about.

      The one uncovered case named at the end -- a signal arriving while a thread
      waits on an UNTIMED futex with no other runnable task -- is an ACCEPTED
      limitation, not residual work: the scheduler reports a deadlock, correctly,
      and no guest in this tree has produced one. Original text below.

- [x] **(original framing) Guest signal handlers run only at `ppoll` / `epoll_pwait`.** Found
      2026-08-13 while testing threads, and it is NOT a thread bug --
      `deliver_pending_signals` has exactly two call sites in `sys.rs`. A guest
      that does `kill()` and then `usleep`, `pause`, `sigsuspend` or ordinary
      work never runs its handler; the pending bit is set and nothing consumes
      it. nginx and postgres never met this because both wait in epoll.

      Cheap partial fixes: call it from `pause`, `rt_sigsuspend`,
      `rt_sigtimedwait` and `clock_nanosleep`, which is where a signal-driven
      program actually waits. A general answer (deliver at any syscall boundary,
      or at scheduler re-entry) is a bigger change to the resume model and needs
      thought about re-entrancy with `Replay`.

      Reproducer: the removed check in `e2e/threadproc_test.go`'s header comment.
      — *source: `2026-08-13 — threads meet the process model`*

      **UPDATE 2026-08-14: measured, and there is now a written guard.**
      `e2e/threadgaps_test.go` runs six waits under ecvisor, gated behind
      `RAPTORMARK_E2E_GAPS=1` so the default suite stays green until the fix
      lands. Result on a static glibc guest, builder `:strip3`: **14 failures,
      and the `rt_sigsuspend` control PASSES** -- so delivery works exactly
      where it has a call site and nowhere else. A signal is delivered from
      neither `nanosleep`, `sigtimedwait`, `pthread_cond_timedwait`, a
      group-directed `kill`, nor a thread-directed `pthread_kill` to a parked
      non-leader. `deliver_pending_signals` now has THREE call sites, not two:
      `sys_ppoll` (`sys.rs:3268`), `sys_epoll_pwait` (`:3420`) and
      `sys_rt_sigsuspend` (`:4191`), plus the SIGILL path in
      `intrinsics.rs:312`. The same guest passes every check natively on glibc
      AND on musl, so none of this is the test's own assumption.

      **CLOSED 2026-08-15.** Delivery boundaries are now `sys_ppoll`,
      `sys_epoll_pwait`, `sys_rt_sigsuspend`, `do_sleep` and `sys_futex`, and
      the guard's signals arm is GREEN -- 8 checks, 0 failures, gate removed. It
      took four fixes, each with its own entry: the missing sleep syscalls, a
      same-group wake that never fired, the per-thread mask (with its selection
      half), and finally:
      (a) the FUTEX path now delivers on resume, which is where
          `pthread_cond_timedwait` and every glibc timed wait parks. It reports
          an ordinary WAKE, never EINTR -- POSIX says a condvar wait does not
          report interruption, and the guard asserts both halves (the handler
          ran AND the wait still timed out at its full deadline). A futex waiter
          is also a wake candidate now, so a signal reaches it promptly rather
          than at its timeout.
      (b) `rt_sigtimedwait` (137) is implemented: it ACCEPTS the signal from
          either queue and deliberately does NOT run its handler, which is the
          whole point of the syscall and the thing a naive version gets wrong.

      What is NOT covered: a signal that arrives while a thread waits on an
      UNTIMED futex with no other runnable task. Nothing wakes the group, and
      the scheduler reports a deadlock -- correctly, since no guest in this tree
      has produced one.

- [ ] **Instruction coverage for the crypto image: 2,800 sites, 474 encodings.**
      Re-measured 2026-08-14 after patch 0058, against `:elem`. Ranked by sites:
      `st1` 736, `tbl` 706, `sli` 535, `trn1`/`trn2`/`uzp2` 371, `fcvt` 106,
      `st4` 81, `ld4` 69, then ~40 smaller families.

      **CORRECTION 2026-08-15: the total was 2,805 here and is 2,800**, and the
      cause is known. `decode-report -log` over the surviving logs
      reproduces every other figure in this entry EXACTLY -- 8,159 padding, 474
      encodings, and each of `st1` 736, `tbl` 706, `sli` 535,
      `trn1`/`trn2`/`uzp2` 371, `fcvt` 106, `st4` 81, `ld4` 69 -- so these are
      the same runs.

      2,805 was arithmetic from the wrong subtrahend. The pre-0058 run has
      3,362 non-padding sites; subtracting the `umlal` count (557) gives 2,805,
      but the family that 0058 closed was `umlal` 557 **plus `umull` 5**, so
      the right subtraction is 3,362 - 562 = 2,800. The "down from 557" below is
      correct for `umlal` alone and is what got subtracted.

      Re-verified end to end 2026-08-15: 3,362 -> 2,800 sites, 652 -> 474
      encodings, the whole family 562 -> **0**, and the padding control
      **unchanged at 8,159** across both runs. Patch 0058 stands, at scale.

      ⚠️ `usubw` 8, below, is a PATTERN count, not a mnemonic count. objdump
      splits it `usubw` 4 + `usubw2` 4; both are the one `USUBW` pattern
      (`a64.decode:1198`), discriminated by `q`. Expect this wherever a
      `2`-suffixed upper-half form exists.

      0058 verified AT SCALE: `umlal`/`umull`/`smlal`/`smull` are now **0 sites**,
      down from 557, with no residue from the shapes the decoder refuses. The
      padding count was unchanged at 8,159 across both runs -- a useful control,
      since it says instruction coverage moved and function-boundary recovery
      did not.

      ⚠️ Do NOT work from a runtime crash address; work this list. `usubw`, the
      instruction the TLS handshake dies on, is 8 sites and far down it.

      NOTE the character of the remaining head: `st1` and `tbl` are addressing
      modes and table lookups, not arithmetic, so they will not look like the
      five patches of 2026-08-13/14. `st1` is 74 distinct post-index forms;
      `tbl` needs consecutive-register-group handling and has
      `TBL_ASIMDTBL_L1_1` as prior art.

      ✅ TOOLING 2026-08-14: `decode-report` (in `tools/decode-oracle`) now names each of these
      encodings and extracts its operands, so the "74 distinct post-index forms"
      framing is a decodetree artefact of doing it by hand. QEMU expresses the
      same space as **seven** `ST_mult` patterns over one `@ldst_mult` format
      with `rpt`/`selem` as constants, and `tbl` as ONE line whose `len` field is
      the register-group count this entry says is needed:

          TBL_TBX  0 q:1 00 1110 000 rm:5 0 len:2 tbx:1 00 rn:5 rd:5

      Run it before starting any of these; the masks and field positions come
      out of the pinned table rather than off a whiteboard, which is the step
      `AGENTS.md` says is "wrong more often than not".
      Reproduce: `RAPTORMARK_TRANSLATE_VERBOSE=1` with a COLD
      `RAPTORMARK_OBJECT_CACHE`, then `decode-report -log <log>`
      (it filters `enc=0x00000000` and groups by encoding for you, and reports
      the padding count separately as a control).
      — *source: `2026-08-14 — capturing the container output`*

- [ ] **⚠️ THE RE-VERIFICATION THIS ENTRY ASKS FOR HAS BEEN DONE, and it comes
      out on the DATA side.** The entry says "Check whether the sites fall inside
      real functions first". Done 2026-08-22 with
      `.agents-workspace/tmp/smecheck.py`, which walks every 4-byte word in the
      EXECUTABLE sections and asks whether its address lies inside an `STT_FUNC`
      extent:

      | fixture | occurrences of the 4 named encodings | inside a FUNC |
      |---|---|---|
      | `postgres-glibc.fused` (63,584 funcs) | **0** | 0 |
      | `aptget-glibc.fused` (18,946 funcs) | 4, one each | **0 of 4** |

      So the four repeated-byte examples the entry flags -- `0x80808000`,
      `0xe0e000e0`, `0xc000c0c0`, `0x19191900` -- occur once each on aptget,
      **none of them inside any function**, and not at all on postgres. That is
      the signature of data disassembled as code, exactly as suspected.

      Corroborating, from a different direction: the 2026-08-22 reachability
      census found postgres executing **ZERO** undecoded instructions across
      initdb, the postmaster and real SQL. Nothing SME-shaped is being run.

      ⚠️ **What this does NOT establish.** It checks the four encodings the entry
      NAMES, not all 24/50 reported sites -- those were found by the decode
      oracle and enumerating them needs that run repeated, which was not done.
      So: the examples are data, and the case for spending anything on SME is
      weaker than when this was written, but "all 24/50 are data" is not proved.
      ❌ Do not vendor `sme.decode` on the strength of this; do the full
      enumeration first if it is ever worth anything at all.
      Original text below.

- [ ] **(original framing) SME is the decode oracle's remaining blind spot, and it is small.**
      Measured 2026-08-14 with a64 and sve vendored: 24 sites on
      `aptget-glibc` (1.6 M words) and 50 on `postgres-glibc` (4.9 M words) --
      `fmopa`, `bfmopa`, ZA-array `ldr`/`str`, `st1q`.

      ⚠️ Re-verify that these are real before spending anything on them.
      Several of the reported examples are repeated-byte words -- `0x80808000`,
      `0xe0e000e0`, `0xc000c0c0`, `0x19191900` -- which is the signature of
      DATA that objdump decoded as code, not of SME a guest would execute.
      Check whether the sites fall inside real functions first.

      If it is worth doing: vendor `sme.decode` (and possibly `sme-fa64.decode`)
      at the same pin, extend `PROVENANCE.md`, add to the embed shim, and insert
      it **between** a64 and sve in `decode.AArch64` -- QEMU dispatches
      `!disas_a64() && !disas_sme() && !disas_sve()`, and the tables may overlap
      each other, so the position is not cosmetic.
      — *source: `2026-08-14 — SVE closes the decode oracle's blind spot`*

- [ ] **A full TLS handshake stops on `usubw` / `cmlt` in libcrypto.** Verified
      2026-08-13. Dynamically-linked OpenSSL WORKS for hashing and context setup
      -- `_hashlib.openssl_sha256` matches the host oracle, `ssl.OPENSSL_VERSION`
      reads from libssl.so.3, `SSL_CTX_new` runs -- but a complete TLS 1.3
      handshake over `ssl.MemoryBIO` (both endpoints in-process, no sockets;
      natively `TLSv1.3 TLS_AES_256_GCM_SHA384`) dies here:

      ```
      4a762fc: 2e2332d6  usubw  v22.8h, v22.8h, v3.8b     <-- fatal
      4a76304: 6e2332f7  usubw2 v23.8h, v23.8h, v3.16b
      4a7630c: 4e60aadc  cmlt   v28.8h, v22.8h, #0
      ```

      Both are in the SAME BASIC BLOCK as the `usubl` patch 0056 fixed. The
      reproducer is staged at `.agents-workspace/tmp/py/rootfs/opt/tls.py` with a
      self-signed cert beside it, and runs on the cryptography module
      (`crypto/build3`) with no re-fuse.

      ⚠️ Before treating a failure here as new, check which patches the module
      was LIFTED with. The first attempt died on `usubl`, which 0056 already
      implements -- the module predated it.
      — *source: `2026-08-13 — a dynamically-linked OpenSSL consumer`*

- [ ] **A plugin-heavy closure loses the shared library layout.** With 95
      libraries the closure-wide plan needs 0xcc20b38 and the fused region ends
      at 0xa000000, so `FuseClosure` falls back to per-image packing:

      ```
      closure-wide layout needs 0xcc20b38 but the fused region ends at 0xa000000
      (95 libraries over 1 programs)
      ```

      `libAlign` is 2 MiB per library, so ~95 libraries exhaust a 160 MiB
      region on alignment padding alone. Harmless for python (one program, packed
      per-image anyway) but a plugin-heavy MULTI-program closure -- postgres with
      its extensions -- would silently lose library sharing and the whole `#34`
      reuse win. The fallback is correct and it does report itself; what changed
      is that the trigger is now reachable.

      Options: shrink `libAlign` for small objects (an extension module is tens
      of KiB and does not need 2 MiB), or grow the region, or pack plugins
      densely below the shared band. Measure first -- the 2 MiB alignment may be
      load-bearing for something.
      — *source: `2026-08-13 — dlopen'd plugins`*

- [x] **Batch lifter patches before large translations.**
      ✅ **CLOSED 2026-08-22 as a STANDING CONSTRAINT, not work** -- the same
      classification this file already gives `patches/0030` ("a standing
      constraint, not work"). There is nothing to do here; there is something to
      remember, and it is remembered in a durable place:
      `.agents/docs/LTM/ruby-jit-and-jump-table-bringup.md` records "Adopting a
      lifter patch changes `BaseID` and invalidates translated objects", and
      `CLAUDE.md`'s Building section carries the side-build recipe that exists
      for exactly this reason.

      ⚠️ The constraint itself is UNCHANGED and still binding. It also just got
      cheaper to obey and more expensive to ignore: the reachability census of
      2026-08-21/22 found python and postgres executing ZERO undecoded
      instructions, so the next lifter patch should be chosen by what a workload
      DIES on rather than by inventory rank -- which means fewer patches, batched
      harder. Original text below.

- [x] **(original framing, standing constraint) Batch lifter patches before large translations.** A change to `patches/`
      changes the patched base image, whose id is part of the object cache key,
      so it invalidates EVERY cached object. Two 21-minute python translations
      went to predictable misses on 2026-08-13.
      — *source: `2026-08-13 — Two new targets`*

## Performance (from README "Next, in order")

- [ ] **⚠️ TWICE CORRECTED. README item 1 is mostly DONE; what is left is a
      promotion, not a collapse.** Verified against the tree 2026-08-18.

      * `ecv-prepare` merges `llvm-link` + `opt-internalize-globaldce` +
        `namespace-object`. **Default since 2026-08-13** (`stablesplit.go`,
        `ECV_NO_MERGED_PREPARE` is the escape hatch).
      * `prepareAndSplitAndCompile` merges the SPLIT into that too -- "the three
        passes AND the split, against a single parse" -- gated on
        `RAPTORMARK_STABLE_SPLIT` -> `ECV_STABLE_SPLIT`.

      **What promoting it is worth**, measured directly rather than inferred: the
      519 MB `postgres_glibc_fused` `.ns.bc` **parses in 149 s**, and
      `llvm-split -j 80` on it takes **225 s** (the recorded phase was 221.1 s, so
      the reproduction is faithful). So **66% of the split is parsing what the
      previous pass just serialized**; eliminating it plus the ~30 s write is
      ~179 s of that closure's 1,990 s run, ~9%.

      **✅ The blocker is CLEARED as of 2026-08-18.** The six byte-affecting
      switches now reach `TranslateID` (`internal/translate/experimental.go`), so
      two translations of one ELF with different settings no longer collide on
      one key -- which was the stated precondition, and was also a live footgun:
      a `RAPTORMARK_STABLE_SPLIT` run against the shared cache used to poison it.
      A default build's key is unchanged, pinned by literal.

      So what is left is the promotion decision itself: make
      `RAPTORMARK_STABLE_SPLIT` the default. Worth ~9% on the largest closure per
      the measurement above. It is a DEFAULT change with a cache cost, so batch
      it with the `-fPIC` change, which already invalidates everything.

      ⚠️ TWO pricing mistakes preceded this entry, both from the same corpus.
      First: aggregating `*.timing.json` across BOTH pipelines (66 files old,
      44 merged, nothing in a row saying which) gave 770.7 s / 27% for a path
      nobody runs. Second: even the corrected figure described a collapse that
      already exists behind a flag. **Check `phases` for `llvm-link`, and check
      whether the thing is already implemented, before pricing it.**
- [ ] **⚠️ MEASURED 2026-08-21, and the answer is DO NOT DO IT YET.** The
      decision this entry asks for is now arithmetic rather than judgement.
      `.agents-workspace/drivers/idxshift` was written on 2026-08-11 to price
      exactly this and had **never been run**; it has now been run, on
      `busybox-musl.fused` against `raptormark-builder:sweep0821b`:

      | | wall | codegen | partitions from cache |
      |---|---|---|---|
      | index 0 (cold) | 19 s | 12.2 s | 0/80 |
      | **index 1 (shifted)** | **7 s** | **0.2 s** | **76/80** |

      **The partition cache already absorbs the expensive half.** Codegen on the
      shifted index is 0.2 s against 12.2 s -- essentially free -- and what is
      left is ~7 s of lift and serial passes. So an index shift costs a fraction
      of a translation, not a translation.

      Against that, the fix is NOT cache-neutral: `ecv_program_<i>` is baked into
      the object's symbol table, so `Keep` and the fragment text are direct
      `ObjectKey` inputs and **every cached ecvisor object misses once**. Hours,
      to remove ~7 s per shifted program. ❌ Do not spend the cache for this.
      It becomes worth revisiting only if the residual is re-measured on a LARGE
      program and turns out to scale badly -- busybox is a 1.5 MB fused ELF and
      the residual is serial-pass time, which grows with the program.

      ⚠️ Run it with **`RAPTORMARK_PART_CACHE` set**. Without it the driver
      reports 0/80 served and 20 s for the shifted index, which prices the wrong
      thing -- that was the first reading taken here and it inverts the
      conclusion. (The driver's own `stable-split:` line prints the HOST
      `ECV_STABLE_SPLIT`, which is empty even when the pipeline has it, because
      `RAPTORMARK_STABLE_SPLIT` is translated inside the container. Read the
      cache hit rate, not that line.)

      What the entry says below about WHY the coupling exists is confirmed and
      still worth reading. Original text follows.

- [ ] **(original framing) Decouple the registry index from the object.** Name descriptors by
      content hash, so adding one program to a 71-program closure does not miss
      on every object whose index shifts.

      **PARTLY CLOSED, re-verified 2026-08-21. Read this before repricing it.**

      Done, and it is the expensive half: the PARTITION cache absorbs the
      codegen. `internal/builder/partcache.go` keys a partition on its bitcode
      bytes under a compiler salt and nothing else, and
      `builder/ecv-partition.h` was fixed twice to make that pay here
      specifically -- the dead-declaration sweep (an index shift renames
      `ecv_program_name_<i>`, which every partition carried as an unused
      declaration: "55,657 identical IR lines, 6 differing") and the
      sort-by-name canonicalisation. So under `ECV_STABLE_SPLIT` every partition
      except the one holding the fragment is byte-identical across a shift and
      is served from cache.

      Also already content-addressed: the name the RUNTIME reports.
      `EcvProgram.name` is `Program.Name`, which is `translate.ModuleID` -- ELF
      basename plus content hash. The index is not in it.

      Still open, and it is the OBJECT cache: `link.Program.Symbol()` is
      `ecv_program_<Index>`, so `Request.Keep` and the generated fragment both
      carry the index, and both are direct inputs to
      `internal/translate.ObjectKey`. Measured on a synthetic request
      (same ELF, same ModuleID, ecvisor runtime): index 0 keys `1ff47098...`,
      index 1 keys `f107d4bf...`. `--runtime upstream` keys identically at both
      indices, because it has neither `Keep` nor a fragment. The fragment
      differs in exactly four lines, all of them index-derived. So a shift
      re-runs translate-one end to end per program: lift, ecv-prepare, split,
      one partition's codegen, and `wasm-ld -r`. NOT measured --
      `.agents-workspace/drivers/idxshift` exists to measure exactly this and
      has never been recorded as run.

      **The blocker is NOT the ABI.** The old note in `translate.go` said
      `ecv_program_<i>` was "the recovered contract, see
      builder/translate-one.sh". That script is not in the tree, and
      `runtime/src/abi.rs` reads only `ecv_programs`, `ecv_program_count` and
      `ecv_program_size` -- never a per-program symbol name. The comment is
      corrected as of 2026-08-21.

      **The blocker is the cache, and the invalidation is CORRECT rather than
      spurious.** Renaming changes the object's exported symbol, so the bytes
      really do change and every cached ecvisor object must miss once. This is
      therefore a cost decision, not a refactor. Note `internal/link` is
      deliberately absent from `builder.translateSources`, so the rename does
      not move `TranslateID`; the miss arrives through `Keep` and the fragment
      text only.

      To do it, the four build-time consumers of the name:
        1. `link.Program.Symbol()` -> derive from `Name`; drop `%d` from all
           four index-bearing lines of `FragmentC`.
        2. `builder.sideLinkArgs` -- ⚠️ it derives the export from the object's
           POSITION in `--objs`, not from the manifest, so it silently assumes
           `--objs` is in registry order. That assumption has no test and no
           stated invariant TODAY; a content-addressed name would have to come
           from `programs.json` (`link.ReadManifest`), which not every caller
           writes.
        3. `e2e/testdata/embedder.mjs`, which builds `ecv_program_${i}`, and
           `e2e/sidemodule_test.go`, which asserts `ecv_program_0`.
        4. `README.md` step 7 and `MULTIMODULE.md` §5/§8, which quote the name.
- [ ] **One global codegen queue across programs.** Today each program gets its
      own `-P $(nproc)`, so cores idle through every program's tail.
- [ ] **Prune to the reachable closure before lifting.** RE-PRICED 2026-08-14
      and the headline is wrong: a SOUND STATIC keep-list prunes **13-20% of the
      executable range**, not an order of magnitude. Measured with
      over-approximate roots (entry + every function whose address appears as a
      64-bit word) and direct-call edges: bash 452 of 2,270 (20%), postgres 1,916
      of 14,727 (13%), aptget 16 of 39. Address-taken alone is 25% of postgres's
      exe functions before following a single call, and that is the wall.
      The "29,649 lifted where a probe needs 4,812" figure is a DYNAMIC
      observation of one execution over the WHOLE image; it is not a sound static
      bound and should not be quoted as the prunable share.
      MEASUREMENT TRAP: scanning every allocated section for address-taken roots
      makes almost everything look live -- `.ecv.dlsyms` alone supplied 1,670 of
      bash's 1,726 roots, being raptormark's own symbol table. Excluding `.ecv.*`
      moves bash from 7% to 20% prunable. But note the tension: a function
      reachable only via `dlsym` IS live, so a real keep-list must keep the
      dlsym-reachable set for images that use it.
      Still the largest single item and still the only lever on the 80
      per-program partitions -- just worth a few percent of a cold translation
      rather than a factor. Original entry below.
- [ ] **(original framing, and the number that misled) Prune to the reachable closure.** NOW THE LARGEST LEVER
      for a closure's Nth program, and the constraint is precise as of
      2026-08-14: 80 of a closure's ~124 partitions are PROGRAM buckets by
      construction (`nProg = n` in ecv-partition.h when library ranges are set),
      they hold the program's own code, and no cache can serve them -- that is
      what leaves `bin_bash` compiling 84 partitions and 28.68 s of codegen with
      everything else warm. Partition reuse is NOT the lever: it runs at ~90% of
      its structural maximum (37 measured against ~41 achievable; the "of 121"
      denominator counts the 80 that can never be shared).
      Pruning must not vary per program INSIDE a library range, or cached library
      halves and library-scoped partitions both stop matching. Prune inside the
      executable's own range only -- which is exactly where those 80 buckets come
      from, so the constraint and the target coincide. Original entry below.
- [ ] **(original framing) Prune to the reachable closure before lifting.** The largest single
      multiplier, roughly an order of magnitude: `openssl` lifts 29,649
      functions where a probe needs 4,812. It must be a keep-list handed to
      `elflift`, not a thinner symbol table. CONFIRMED NECESSARY 2026-08-10:
      `opt -passes=internalize,globaldce` removes nothing -- measured on dash,
      `merged.bc` 35,051,036 -> `mi.bc` 35,059,916, i.e. the module GREW. The
      per-function `indirectbr` address tables keep every lifted function
      reachable, so no post-lift DCE can help.

## Build speed (measured 2026-08-10, see JOURNAL)

DEDUPED AND SWEPT 2026-08-15. This section had been triplicated: three
byte-identical 296-line copies of the same block, only the last of them current,
and 24 open boxes of which 17 were duplicates or archival prose under a
completed successor. Two copies removed (592 lines, zero unique lines lost),
archival entries closed, stale entries reconciled against the 2026-08-15
measurements in place rather than rewritten, and the 29 completed entries then
moved to `JOURNAL.md` with the rest. 1,177 lines to 7 open items.
If this section grows a second copy of anything again, `diff` the blocks before
editing either -- all three copies here were byte-identical, so the triplication
was invisible to every reader who started at the top and stopped when the text
looked familiar.

OPEN, in the order worth doing (2026-08-15):

  1. The codegen wall is ONE GUEST FUNCTION LIFTED THREE TIMES -- the largest
     priced lever, blocked on one reachability measurement.
  2. `.ecv.funcs` as a REACHABLE SUBSET -- computes the artefact #1 is blocked
     on, so do them together.
  3. `ecv-prepare-split`, 17.32 s of 57.09 s -- the whole warm regime, and
     per-program by construction. Measure its internal split first.
  4. Decide whether the shared-name path stops being opt-in.
  5. Partition SIZE does not predict COST -- explained, and worth only ~3%
     until the wall stops being one function.
  6. `patches/0030` must not ship (a standing constraint, not work).
  7. `RunAll` -- deprioritised, and antagonistic to the caching.

Translation is ~99% of build cost. `internal/builder/timing.go` now writes a
per-phase and per-partition `<module-id>.timing.json` into `--out`;
`ECV_KEEP_SPLIT=1` preserves the partitions. First breakdown, dash.fused on 20
cores: codegen-parts **89.4%**, the four serial whole-module passes 10.6%,
elflift 3.8%, `wasm-ld -r` negligible.

- [x] **`patches/0030-shard-the-lift.patch` is EXPERIMENTAL and must not ship.**
      ✅ **CLOSED 2026-08-22 as a STANDING CONSTRAINT that is satisfied BY
      CONSTRUCTION** -- the classification this section's own summary already
      gives it ("a standing constraint, not work"). Verified rather than assumed,
      because the surface reading is alarming and wrong:

      - The patch **IS applied to the shipping base.** `buildimage.go:223` runs
        `for p in /patches/*.patch; do git apply "$p" || exit 1; done` with **no
        exclusion list**, so 0030 is in `raptormark-elfconv-base-patched:sisd0065`
        like every other patch, and `--shard` is present in the base's
        `lifter/Lift.cpp`.
      - It is **dormant**. 0030 adds an opt-in flag,
        `DEFINE_string(shard, "", ...)`, whose own help says **"Unset means lift
        all"**, and **nothing in the pipeline passes it** -- grepping
        `internal/builder/translateone.go`, `internal/translate/` and
        `builder/*.sh` for `shard` returns nothing.

      So "must not ship" means "must not be ENABLED", and no caller exists to
      enable it. The measured defect below (shards lift 6,567 functions against
      6,570; partial address->function tables) is unreached.

      ❌ **Do NOT try to satisfy this entry by deleting the patch.** Removing a
      file from `patches/` changes the patched base's content, hence its image
      id, hence `BASE_ID`, hence **every** object-cache key -- hours of
      re-translation to remove code that already does nothing. The cheap,
      correct guard is the one that exists: no caller. Original text below.

- [x] **(original framing) `patches/0030-shard-the-lift.patch` is EXPERIMENTAL and must not ship.**
      It proves the 2.5x and locates the ceiling, nothing more. Measured defect:
      shards lift 6,567 functions against 6,570 unsharded because
      `rest_disasm_funcs` diverges, and `addr_fun_name_map` accumulates only the
      shard's own functions so the address->function tables come out partial --
      the shards cannot yet be linked into a working module.
- [ ] **Partition SIZE does not predict partition COST**, so the largest-first
      sort in `splitAndCompile` is ordering on nearly the wrong key. Measured
      across 80 dash partitions: sizes span 1.65x (0.93-1.52 MB), times span
      **2264x** (0.2-461.4 s), Pearson r = 0.589. `llvm-split` balances bytes
      successfully, and that byte balance is what hides the cost spread.
      RE-MEASURED and RE-PRICED 2026-08-15 over six runs of bin_echo: r stays in
      0.600-0.776 while time spread reaches 30,083x, so the claim holds on a
      second fixture -- and the CAUSE is now known rather than merely observed.
      Codegen is superlinear in the largest single function, so bytes cannot
      predict cost; a 0.60 MB partition was the costliest of the 80 in the
      namehash arm while the 1.12 MB partitions were not.
      But the PRICE of the wrong key is only ~3%: the wall exceeded the slowest
      partition by 18.0 s of 524.7 s in the worst of the six runs, and by ~0 in
      the rest. Worth fixing only when the wall is no longer one function.
- [ ] **`.ecv.funcs` is emitted, consumed by nothing, and costs 942 KB per
      image.** Re-verified 2026-08-13: no reference in `patches/` or
      `third_party/elfconv/lifter/`; on the fused postgres image the section is
      0xeb810 bytes, SHF_ALLOC, at 0x7870000, so it also spends the scarce
      156 MiB fused region. Consuming it as a keep-list is now WORSE than inert:
      library-scoped partitions require a library's lifted set to be identical in
      every program, and pruning to what THIS program reaches makes it
      program-specific, breaking every library partition. Decide between dropping
      the producer and finding it a job that does not prune per program. Measured: it lists
      6,151 functions where elflift discovers 6,570 on the same image, the extra
      419 being PLT stubs (`patches/0019`) and gap-filling rest functions
      (`patches/0018`). So a restrictive consumer would drop 419 functions into
      `_ecv_unreached`, and an additive one is inert because the section only
      restates the `.symtab` fusing itself wrote. It becomes useful only as a
      REACHABLE SUBSET, which must also enumerate PLT stubs and gap fillers.
      LINKED 2026-08-15: that reachable subset is the same artefact the codegen
      wall item below is blocked on -- whether an FDE-seeded entry is ever
      branched to. Whoever computes it should settle both at once, and must
      enumerate FDE-derived entries as a fourth category alongside symbols, PLT
      stubs and gap fillers.
- [ ] **Decide whether the shared-name path stops being opt-in.** It still
      reaches no default path. The blocker is that
      `TestEcvisorTwoProgramsLinkWithoutCollision` asserts two programs' objects
      share NO symbols, which sharing inverts by design, so that test needs its
      own expectations before the flag can default on. Running without the
      closure layout must stay a no-op, not a wrong answer.
- [ ] **`ecv-prepare-split` is now the largest term for a closure's Nth
      program**, 17.32 s of 57.09 s, and it is per-program BY CONSTRUCTION: it
      tags every local symbol with the program's id, so no cache can serve it
      across programs. Codegen was fixed, which promoted the serial passes;
      those were fixed, which promoted the lift; the lift is now cached, which
      promotes this. Anything further in the warm regime is here.
      Unexamined: how much of it is the namespacing walk against the partition
      cloning and writing. Measure that split before designing anything.
- [ ] **The codegen wall is ONE GUEST FUNCTION LIFTED THREE TIMES.** Found
      2026-08-15 while closing the A/B above, and it is the largest priced
      build-speed lever now open.
      bin_echo's three most expensive partitions hold, one each:

        __vfscanf_____657180        61,488 IR lines   (the ELF symbol, size 60)
        _ecv_fde_6571c0_____6571c0  61,592 IR lines
        _ecv_fde_657240_____657240  62,371 IR lines

      Those are not three functions. Compared order-independently after
      normalising SSA names and numbering, they share **99.8-100.0%** of their
      instruction lines; a plain diff of the first pair is 280 differing lines
      out of 61,488. `vfwscanf` repeats the pattern at 0x664ac0 / 0x664b00 /
      0x664b80, ~52k lines each. Six functions, ~342k IR lines, of which ~228k
      are duplication -- and each copy IS one of the ~375-475 s partitions that
      constitute the entire codegen wall.
      MECHANISM: `__vfscanf`'s ELF symbol is 60 bytes, so 0x6571c0 and 0x657240
      lie OUTSIDE it. They are separate trace-lift entry points seeded from
      `.eh_frame` FDE start addresses, and remill's trace lifter emits every
      block reachable from an entry into that entry's function, so three entries
      converging on one body produce three whole copies of it.
      SCALE, as an upper bound rather than a claim of pure waste -- some
      `_ecv_fde_` entries are legitimate functions that carry no symbol:
      FDE-seeded entries are 1,616 functions and **68.8%** of bin_echo's IR, and
      2,110 functions and **51.0%** of bash-glibc's.
      WHY IT SHOWS UP ON ECHO AND NOT BASH: concentration, not volume. The two
      modules have nearly the same total IR (3.97M vs 4.08M lines) but echo
      spreads it over 5,425 functions against bash's 7,987, and has **102**
      functions over 10k IR lines against bash's **38**. bash's largest,
      `_ecv_fde_857a80` at 60,373 lines, appears ONCE. Codegen cost is
      superlinear in function size, so equal IR concentrated costs 8.2x.
      ❗ BEFORE DESIGNING A FIX, establish whether the redundant entries are
      REACHED. An FDE start is not evidence that anything branches there; if
      nothing computes those addresses they can be dropped outright, which is
      the same lever as "Prune to the reachable closure before lifting" and
      should be done as one piece of work. If they ARE reachable, dropping them
      is a correctness bug, not a speed win -- an LLVM function cannot be entered
      part-way, so a shared body needs a different emission strategy, not a
      deleted seed. Do not assume; the last two candidate mechanisms for this
      same 5x were both refuted by measurement.
- [ ] **Cross-program concurrency: the machine is still 15% busy, and the old
      implementation is deleted.** `internal/translate/runall.go` and its test
      were removed 2026-08-18 (measurements preserved in `JOURNAL.md`). It had no
      caller, its concurrency was never tested -- only the pure `schedule`
      arithmetic was -- and its rationale had been overtaken. Deleting it was the
      point: dead code with a confident, obsolete header COMPILES, so it reads as
      current.

      **Why the question stays open.** 15.2% machine utilization on a 20-core box
      was a real measurement, and nothing since has raised it -- the caches made
      single translations cheaper rather than making the machine busier.

      **What a correct attempt has to answer first**, and no arrangement of a
      worker pool answers it: concurrent translation is ANTAGONISTIC to the
      caching that replaced it. Programs translated together from cold miss the
      caches together, and each compiles the shared library partitions
      independently -- duplicating exactly what the per-library lift cache and the
      partition cache remove. **A program can hide in another's shadow or reuse
      its output, not both.** Decide that interaction before writing any
      scheduler.

      **What is no longer true**, so nobody re-derives the old numbers: serial
      phases were 62% of a run and are now ~18-21% (`ecv-prepare` merged three
      passes); programs 2..N had a full codegen to hide in and now have 1.10 s
      (the partition cache serves them). The old 1.61x re-estimates to ~6% on a
      three-program closure.

      **Where it would still pay**: a closure whose programs share NO libraries,
      where the caches can do nothing -- and the global codegen queue of README
      item 3, which is a different shape from a per-program worker pool.
      Verified 2026-08-18.
## Runtime portability

- [ ] **⚠️ `builder/_tools/raptormark-builder-tools` is a PREBUILT binary, and a
      raw `docker build` does not rebuild it.** Cost one void gate on
      2026-08-18: a `-fPIC` pipeline change never reached the image, and three
      independent-looking signals (a moved `translate_sh`, a genuinely cold
      1,227 s re-translation, a differing `libecvisor.a`) all said it had --
      because all three are downstream of "the source changed" and none of
      "the binary changed". It also poisoned 45 object-cache entries.

      `raptormark build-image` rebuilds it (`buildimage.go` `buildTools`:
      `CGO_ENABLED=0 GOOS/GOARCH from the base image, go build -trimpath -o
      builder/_tools/raptormark-builder-tools ./cmd/raptormark`). The
      side-build recipe in CLAUDE.md deliberately avoids `build-image` to protect
      the patched base -- correct, and it silently skips the tools build.

      **✅ MITIGATED 2026-08-18, not closed.** `raptormark build-tools --base
      <patched base>` rebuilds just that binary and prints its before/after size
      and mtime, and CLAUDE.md's side-build recipe now calls for it first.
      `TestBuildToolsWritesWhatTheDockerfileCopies` reads the path out of the
      Dockerfile so the command and the image cannot drift apart.

      The two structural fixes were considered and NOT taken: folding the tools
      binary's hash into `TranslateSH` (rejected in `toolsid.go` for a stated and
      still-correct reason -- it would discard a cache worth hours on every
      unrelated toolchain bump), and building the tools in a Docker stage
      (`builder/Dockerfile` keeps the Go toolchain out of the image on purpose).
      So this stays open: the hazard is now one forgotten command rather than one
      forgotten thought, which is better but is not the same as impossible.
      Verified 2026-08-18.

- [ ] **⚠️ wasmedge's default run mode is the INTERPRETER, and AOT is 35x
      faster.** Measured 2026-08-18: 20M iterations of a call-heavy loop take
      4,673 ms interpreted and 133 ms under `--run-mode aot`. A `wasmedge
      compile` artifact is NOT used unless that flag is passed -- running the AOT
      module without it gives timings identical to the plain one, which is how
      the first attempt at the multi-module call benchmark reported interpreter
      numbers as AOT numbers.

      Two consequences worth chasing: (1) **any wasmedge timing in this tree that
      did not pass `--run-mode aot` is an interpreter number**, which includes
      the e2e suite and may include performance figures in JOURNAL entries -- they
      are still valid as comparisons between two interpreted runs, but not as
      absolutes; (2) find out what the containerd/runwasi shim actually does,
      because if it interprets, every guest ships 35x slower than it needs to,
      and if it AOTs, our measurements do not describe the shipping
      configuration. Verified 2026-08-18.

- [x] **Every module is WasmEdge-bound, including ones with no sockets.**
      Measured 2026-08-18 with `e2e/imports_test.go`: a guest that is literally
      `int main(void){return 0;}` links **28 imports, 11 of them WasmEdge's
      socket extension** to `wasi_snapshot_preview1` (`sock_open`, `sock_bind`,
      `sock_listen`, ...). They come from ecvisor being linked in whole, not from
      what the guest reaches, so a socket-free guest is not a wasmtime-runnable
      artifact today even though nothing about it needs sockets. The lever: make
      those imports conditional, so a module that never calls them does not
      demand them. Would make the stock wasmtime shim a real target for the
      simple cases. Verified 2026-08-18.

      **✅ DONE 2026-08-19.** The lever turned out to be a backend seam rather
      than conditional imports: `runtime/src/net` selects a `NetBackend` at
      COMPILE time (a `cfg`-chosen type alias -- NOT a `dyn` or an enum, either
      of which keeps every impl live and puts every import set in the module),
      and `link-all --profile loopback` links a second `libecvisor.a` built with
      `--features net-loopback`.

      Measured: the probe guest goes from **28 imports to 15**, and the
      difference is thirteen names -- the eleven extensions plus
      `fd_fdstat_set_flags` and `sock_accept`, which only the WasmEdge backend
      calls. **`TestLoopbackModuleRunsOnStockWasmtime` runs the module under
      stock `wasmtime` 46.0.1 with no flags**, which is the first time any
      raptormark artifact has run outside WasmEdge.

      Neutralized: reaching the unused backend from a live path put `sock_open`
      back and wasmtime failed with `unknown import:
      wasi_snapshot_preview1::sock_open has not been defined` -- the exact
      historical blocker, reproduced on demand.

      ⚠️ **Not the same as "the shipping profile is portable".** The default
      profile is unchanged and still imports all 28; what exists now is a second
      profile whose network is in-process only. A guest that actually needs the
      network still needs a backend that provides one.
- [ ] **⚠️ THE COST SIDE OF THIS DECISION SHRANK. Re-verified 2026-08-22.**
      The entry below argues for removing `--enable-all` partly because the flag
      "costs the runtime half of the no-proposal guard" -- with every proposal
      on, a module that started needing one would still pass, and
      `TestWasmOptEnablesNoProposal` only checks the flags handed to `wasm-opt`.

      **A runtime no-proposal guard now exists and is green.**
      `TestLoopbackModuleRunsOnStockWasmtime` (`e2e/loopback_test.go:186`) runs a
      real lifted module with `wasmtime run <module>` and **no flags at all** --
      its own comment says "No `--enable-all`: the point is a STOCK host. If this
      needs a flag, the [claim is broken]". It passed in every suite run taken
      today.

      ⚠️ **It covers the LOOPBACK profile, not the default one.** Default-profile
      modules are still only exercised under `wasmedge --enable-all`, so the gap
      is NARROWER, not closed: a proposal creeping into the default profile would
      still go unnoticed. That is what the decision below is now actually about,
      and it is a smaller claim than the entry makes.
      Original text below.

- [ ] **(original framing) DECIDE whether the e2e harness should stop passing `wasmedge
      --enable-all`.** Measured 2026-08-18: the full fast suite is 85/4/0 either
      way, wall 178.1 s vs 178.7 s, and the `component model is enabled` warning
      goes from every run to zero. The flag buys nothing and costs the runtime
      half of the no-proposal guard -- with every proposal on, a module that
      started needing one would still pass, and
      `TestWasmOptEnablesNoProposal` only checks the flags we hand `wasm-opt`.
      The change is one line at each of three sites; it is listed here rather
      than done because it changes what the default suite exercises.
      Verified 2026-08-18.

- [ ] **⚠️ PARTLY SOLVED, and the entry below predates the solution.**
      Re-verified 2026-08-22. The blocker this entry describes -- WasmEdge's
      non-standard socket imports being unconditional, so wasmtime's WASI p1
      rejects instantiation with `unknown import:
      wasi_snapshot_preview1::sock_open` -- was answered on 2026-08-19 by the
      **loopback profile**, which is the first of the two options this entry
      itself offers ("make the socket surface optional").

      `runtime/src/net` picks a `NetBackend` at COMPILE time via a cfg-chosen
      type alias, and `link-all --profile loopback` links a second
      `libecvisor.a` built with `--features net-loopback`. Green in the
      2026-08-22 e2e run:

      | test | result |
      |---|---|
      | `TestLoopbackModuleRunsOnStockWasmtime` | **PASS** |
      | `TestLoopbackProfileImportsNoSocketExtension` | PASS |
      | `TestLoopbackAndWasmEdgeDisagreeExactlyOnSockets` | PASS |
      | `TestLoopbackProfileServesGuestLocalSockets` | PASS |

      ⚠️ **What remains open is narrower than the title.** The DEFAULT profile is
      unchanged and still imports all 28, so the *shipping* artifact is still
      WasmEdge-bound; and a guest that genuinely needs the network still needs a
      backend that provides one -- loopback is in-process only. So the open
      question is "does the default profile become portable, and what supplies
      the network if it does", not "can anything run on wasmtime". That is
      answered.

      `e2e/containerd_test.go`'s per-runtime table still names the original
      blocker and still fails loudly if a DEFAULT-profile guest starts
      completing on wasmtime, which is the right guard to keep. Original text
      below.

- [ ] **(original framing, predates the loopback profile) `containerd-shim-wasmtime` loads the module but cannot run it.**
      `runtime/src/sys.rs:522-528` declares an unconditional
      `#[link(wasm_import_module = "wasi_snapshot_preview1")]` block for
      WasmEdge's *non-standard* socket extensions (`sock_open`, `sock_bind`,
      `sock_connect`, `sock_listen`, `sock_getlocaladdr`, `sock_getpeeraddr`);
      only `sock_accept` is the standardized 3-arg preview1 form. They are
      referenced from live code (`sys.rs:3794`, `3859`, `3877`, `3901`), so the
      imports are in every emitted module and wasmtime's WASI p1 rejects
      instantiation with `unknown import: wasi_snapshot_preview1::sock_open`.
      This dependency long predates the no-proposal work; it was simply
      invisible while wasmtime rejected the module at parse time over
      exception-handling. Either make the socket surface optional (a guest that
      never calls `socket(2)` should not import it) or supply a wasmtime host
      module. `e2e/containerd_test.go`'s per-runtime table names this blocker
      and fails loudly if the guest starts completing on wasmtime, so the test
      reports the day it lifts. Verified 2026-08-09.

## Performance (cheap, unmeasured)

- [x] **`wasm-opt` at `-O0` is now a pure round-trip.** ✅ **CLOSED 2026-08-21 --
      the premise is FALSE and the code says so in a dated comment.** No build
      was needed to settle this; it was settled by reading
      `internal/builder/linkall.go`, which is what this file's own header asks
      for before acting on an entry.

      `wasmOptArgs` carries `CORRECTION 2026-08-15`, six days AFTER this entry's
      "Verified 2026-08-09": the `-O0` step is **not** a round-trip. Binaryen does
      report "no passes specified, not doing any work", but the module still
      shrinks **5.5% (127,354,502 -> 120,298,637** on the openssl fixture)
      because it re-encodes wasm-ld's padded LEBs. So the entry's "skipping it
      outright would cost nothing" is wrong by 7 MB on that fixture.

      And the thing this entry asks to be BUILT already exists: `ECV_WASM_OPT=0`
      (`wasmOptEnabled`) skips the pass entirely, and `finalise` renames the
      pre-module into place instead. It is opt-in rather than default precisely
      because the correction above makes it a size/time trade rather than a free
      deletion.

      Nothing to do. ⚠️ If someone re-raises this, the answer is the 5.5%
      measurement, not another `wasm-objdump` comparison -- the entry asked for a
      diff to confirm identity, and identity is already refuted by size.
      Original text below.

- [x] **(original framing, premise refuted) `wasm-opt` at `-O0` is now a pure round-trip.** With
      `--translate-to-exnref` gone (`internal/builder/linkall.go:96`), the
      finalise pass re-emits the name section and nothing else, for ~30 s on the
      openssl fixture. Skipping it outright when `ECV_WASM_OPT_LEVEL` is `-O0`
      and names are kept would cost nothing — but confirm the round-trip really
      is identity (compare `wasm-objdump` output, not just sizes) before
      removing a pass that currently normalizes the module. Verified 2026-08-09.

## Product surface

- [ ] **No end-to-end driver.** `cmd/raptormark` covers the build steps
      (`build-image`, `translate-one`, `link-all`), but the pipeline that strings
      them together — discovery, fuse, translate, link — still runs only from the
      `e2e/` suite. There is no `raptormark build <image>` or `raptormark run`.

## Runtime performance and diagnostics

- [ ] **Localize the remaining nginx throughput serialization.** RE-MEASURED
      2026-08-09; the old figures (178 req/s implied vs 28 measured) are obsolete
      and the profile has changed shape entirely. Now: ~190 req/s measured (200
      requests in 1.05 s at 25-way) against ~10 ms single-request latency, so the
      gap is smaller but still roughly an order of magnitude. The scheduler is no
      longer the suspect — of a 130 ms five-request window, 40 ms is idle host
      poll BETWEEN requests and 58 ms is guest execution, with ~25 replay frame
      re-entries per request. Next step is to separate lifted-code execution from
      full-replay stack reconstruction; that needs a way to time replay, which
      does not exist yet.
- [x] **Two prctls that EVERY ruby run reaches still return EINVAL, and Linux
      returns 0 for both.** ✅ **CLOSED 2026-08-22: both are ruled and
      implemented**, and the rulings sit at the arms in `runtime/src/sys.rs`.

      - **`PR_SET_THP_DISABLE` (41) / `PR_GET_THP_DISABLE` (42): accepted and
        STORED.** The flag withholds a kernel optimisation, and ecvisor has no
        page tables, no fault handler and no huge pages -- "do not give me huge
        pages" is a request that is already satisfied and can never stop being.
        The value is stored per-thread-group (`context::set_group_thp_disable`)
        for the same reason `PR_SET_DUMPABLE`'s is: `PR_GET_THP_DISABLE` exists
        and must answer with what the SET stored.
      - **`PR_SET_VMA` + `PR_SET_VMA_ANON_NAME`: accepted and NOT stored.** The
        name's only observable effect on Linux is the `[anon:NAME]` printed into
        `/proc/<pid>/maps`, and ecvisor has no `/proc` at all -- so the name is
        not merely unused, it is unreadable, and there is no `PR_GET_VMA_*` to
        read it back either (measured: `prctl(0x53564d42)` is EINVAL). A
        per-range name table nothing could read would be state pretending to be
        a facility. Every rule the guest CAN still observe through the return
        value is enforced, in Linux's order: sub-option, then the name, then the
        range.

      ⚠️ **Neither has PR_SET_DUMPABLE's shape, and assuming it would have been
      wrong twice.** Measured on Linux 6.17/aarch64:

      - `PR_SET_THP_DISABLE` does NOT validate arg2 at all -- it stores its
        TRUTHINESS (`if (arg2) set_bit else clear_bit`), so `2`, `3` and `-1`
        all return 0 and all make the GET read 1. What it DOES validate is
        arg3/arg4/arg5, which are reserved and must be zero; the GETTER reserves
        arg2 as well. Two different rules, one syscall.
      - `PR_SET_VMA_ANON_NAME`'s name is a POINTER, capped at 79 characters
        (the kernel's `ANON_VMA_NAME_MAX_LEN` is 80 and counts the NUL), and its
        character rule accepts SPACE and rejects DEL -- the opposite of the
        natural guess for both -- while rejecting `[ ] \ ` $` because the name
        is printed as `[anon:NAME]`. The length is page-ROUNDED UP, not refused
        (`len` 1 names a whole page), a zero length returns 0 without consulting
        any mapping (even at an unmapped address), and the errnos are four
        distinct ones: EINVAL, EFAULT, ENOMEM, EBADF.

      ⚠️ **Two stated divergences remain, both because the bump arena cannot
      answer the question:** naming an unmapped range INSIDE the arena returns 0
      where Linux gives ENOMEM (the arena cannot tell a live mapping from a hole
      -- the same limit `NR_MREMAP` states for growing in place), and there are
      no file-backed mappings for Linux's EBADF to distinguish. Out-of-arena
      ranges DO get ENOMEM, because that one is honestly answerable.

      Host guards: 9 tests, `context.rs` (4, the THP flag) and `arena.rs` (5,
      the name and range rules), each neutralized. ⚠️ One of them,
      `a_range_four_gib_long_is_not_inside_a_384_mib_arena`, CANNOT observe the
      wasm32 half of its own claim -- `usize` is 64-bit on the host, so
      re-writing the function in `usize` does not make it fail; that is recorded
      at the test, and `context::dumpable_arg_permitted`'s 64-bit test has the
      same limit. E2E `e2e/prctlmm_test.go` runs the same guest under ecvisor
      and natively; against the pre-fix `raptormark-builder:prctl` image 34
      guest checks fail with errno 22 while the native baseline still passes.

      Original text below.

      **Two prctls that EVERY ruby run reaches still return EINVAL, and Linux
      returns 0 for both.** Found 2026-08-22 by scanning the fused fixtures for
      prctl call sites while closing `PR_SET_DUMPABLE` (below). Filed as its own
      item because it was first recorded INSIDE that now-closed entry, where open
      work is invisible.

      | prctl | call site | frequency |
      |---|---|---|
      | `PR_SET_THP_DISABLE` (41) | `ruby_setup+0x6c`, `prctl(41, 1, ...)` | every ruby startup |
      | `PR_SET_VMA` (`0x53564d41`) + `PR_SET_VMA_ANON_NAME` | `ruby_annotate_mmap`, `heap_page_allocate_and_initialize` | every GC heap page |

      Both measured returning **0** on this kernel (aarch64, 6.17). Both fall to
      `NR_PRCTL`'s `_ => EINVAL` catch-all, so this is the same class as
      `PR_SET_DUMPABLE`: a DIVERGENCE from Linux by default rather than a ruling,
      and therefore not a decision anyone owes.

      ⚠️ **Not urgent, and say why**: ruby ignores both results, so nothing fails
      today. What it costs is a per-GC-page wrong answer in a guest we intend to
      support, and a diagnostic that reads as a defect to whoever meets it next.
      The honest answer for both is very likely "accept as a no-op with a stated
      reason" -- ecvisor has no transparent huge pages to disable and no
      `/proc/*/maps` for an anonymous VMA name to appear in -- but VERIFY the
      accepted argument values against the kernel first, the way
      `PR_SET_DUMPABLE` was: that one accepts **only 0 and 1**, and `2` is
      EINVAL, which contradicts the obvious guess.

      ⚠️ And use the FULL `u64` argument in any validation. `usize` is 32-bit on
      wasm32, so a rule written on `arg as usize` folds `0x1_0000_0001` onto 1 --
      the same trap that `dumpable_arg_permitted` and `sigaction_permitted` are
      both written to avoid.

      Unreached in our runs, recorded so nobody re-scans: nginx's
      `PR_SET_KEEPCAPS` (transparent-proxy config only) and the
      libcap/libcap-ng/setpriv sites in the postgres, busybox and apt closures
      (`PR_CAPBSET_READ/DROP`, `PR_GET/SET_SECUREBITS`, `PR_SET_NO_NEW_PRIVS`,
      `PR_SET_MM`, `PR_CAP_AMBIENT`, a `PR_GET_DUMPABLE` in libcap-ng).
      — *source: the `PR_SET_DUMPABLE` closure below*

- [x] **⚠️ TWO OF THREE ARE TRIAGED; only `PR_SET_DUMPABLE` is still an
      undecided default.** ✅ **CLOSED 2026-08-22: the third one is ruled and
      implemented.** `prctl(PR_SET_DUMPABLE, v)` now validates `v` the way Linux
      does (exactly `SUID_DUMP_DISABLE`/`SUID_DUMP_USER`, over the full 64-bit
      argument), records it, and `PR_GET_DUMPABLE` reads it back; the ruling and
      its reason sit at the arm in `runtime/src/sys.rs`. The reason is that
      ecvisor writes no core dumps and offers no ptrace, so the flag has no
      observable effect either way -- accepting it is truthful, and ⚠️ it
      claims NO core-dump facility (`WCOREDUMP` stays unclaimed).

      Linux was MEASURED, not assumed (Linux 6.17, aarch64 host): `0`/`1`
      return 0; `2`, `3`, `-1` and `0x1_0000_0001` are EINVAL; `PR_GET_DUMPABLE`
      returns the stored value AS the syscall return; fork inherits it; execve
      resets it to 1; and a worker thread's SET is visible to its siblings
      (per-MM), which is why `set_group_dumpable` writes the whole thread group.

      Host guards in `context.rs` (4 tests, all neutralized): the accepted-value
      rule, the 32-bit-truncation trap, set/get round-trip, and the thread-group
      write. E2E `e2e/dumpable_test.go` runs the same guest under ecvisor and
      natively; against the pre-fix image every check fails with errno 22.

      ⚠️ **Two OTHER prctls a real guest in this tree reaches still get EINVAL
      by catch-all, and Linux returns 0 for both** (found by scanning the fused
      fixtures for prctl call sites, so these are call sites that exist in the
      binaries we actually run):

      - **`PR_SET_THP_DISABLE` (41), ruby.** `ruby_setup+0x6c` calls
        `prctl(41, 1, ...)` on first initialization -- so EVERY ruby run hits it.
      - **`PR_SET_VMA` (0x53564d41) with `PR_SET_VMA_ANON_NAME`, ruby.**
        `ruby_annotate_mmap` and `heap_page_allocate_and_initialize` call it, so
        it recurs for every GC heap page. Measured: this kernel returns 0.

      Neither is implemented -- ruby ignores both results, and neither was in
      this entry's scope. Everything else found is unreached in our runs:
      nginx's `PR_SET_KEEPCAPS` (transparent-proxy config only), and the
      libcap/libcap-ng/setpriv sites in the postgres, busybox and apt closures
      (`PR_CAPBSET_READ/DROP`, `PR_GET/SET_SECUREBITS`, `PR_SET_NO_NEW_PRIVS`,
      `PR_SET_MM`, `PR_CAP_AMBIENT`, and a `PR_GET_DUMPABLE` in libcap-ng).

      Original text below.

      **⚠️ TWO OF THREE ARE TRIAGED; only `PR_SET_DUMPABLE` is still an
      undecided default.** Re-verified against `runtime/src/sys.rs` 2026-08-22.
      The entry asks to "determine whether to implement, suppress, or document
      each result" -- two now have a determination recorded AT the code:

      - **`io_setup` -> ENOSYS, deliberately.** The const sits under a comment
        headed "Deliberately unimplemented, listed so they read as decisions
        rather than gaps": it is Linux AIO, nginx logs a notice and falls back to
        blocking I/O, and ENOSYS is how it must be told (`sys.rs:238-241`).
      - **`initgroups` -> resolved.** `NR_SETGROUPS => set_ret(0)` with the
        reason stated: supplementary groups are not modeled, `setgroups` accepts
        and discards and `getgroups` reports an empty set, "a truthful answer to
        'which supplementary groups am I in' given we never joined any"
        (`sys.rs:716-721`). The `ENOENT` this entry reports is gone.
      - ❌ **`prctl(PR_SET_DUMPABLE)` -> EINVAL, but by CATCH-ALL, not by
        ruling.** The `NR_PRCTL` arm names `PR_GET_NAME`, `PR_SET_PDEATHSIG` and
        `PR_SET_NAME`, each with a reason, and everything else falls to
        `_ => set_ret_err(EINVAL)` (`sys.rs:879-896`). So this one is still
        literally what the entry describes.

      The tractable option, and its precedent is three lines above it:
      `PR_SET_PDEATHSIG` is accepted as a no-op precisely because postgres
      treats EINVAL there as fatal. `PR_SET_DUMPABLE` could join it -- this
      runtime produces no core dumps and is not ptrace-able, so the flag has no
      observable effect either way -- which turns a per-worker diagnostic into
      silence. ⚠️ It is a guest-VISIBLE behaviour change (EINVAL -> 0) on a
      syscall nothing currently depends on, so it wants a deliberate ruling
      rather than a quiet edit. Original text below.

- [x] **(original framing) Triage non-fatal nginx worker diagnostics.** ✅ CLOSED
      2026-08-22 with the entry above: all three results now have a
      determination recorded at the code -- `io_setup` ENOSYS deliberately,
      `setgroups` accepted-and-discarded, `PR_SET_DUMPABLE` accepted and stored.
      Original text: Each worker can report
      `io_setup()` `ENOSYS` and `prctl(PR_SET_DUMPABLE)` `EINVAL`; musl worker 2
      also reported `initgroups` `ENOENT`. Determine whether to implement,
      suppress, or document each result.
      — *source: `Session summary: where nginx stands, 2026-08-09`*
- [x] **Re-price the proposed wazero debugging harness before building it.**
      ✅ **CLOSED 2026-08-22: re-priced, and the answer is DO NOT BUILD IT.**
      The entry asked for a re-price rather than a build, and every input to that
      price has since moved against it:

      - **Its symbolization premise landed elsewhere.** `link-all` keeps the wasm
        NAME SECTION (`-g`), and elfconv names each lifted function
        `<sym>_____<idx>_<hexvma>`, so ONE name resolves both the guest symbol
        and its guest VMA -- a wasm-level trap is already directly comparable
        with ecvisor's own `fn=0x...` diagnostics.
      - **The two investigations that motivated it were answered by timestamped
        traces**, which need no new engine: `ecv_trace!`/`ecv_probe!` plus a
        host-side timestamper turn the existing stderr stream into a profile with
        no rebuild.
      - **A second host arrived and it is not wazero.** The Node host and the
        browser profile (`web/`) give a real non-WasmEdge execution path with a
        debugger attached, which is most of what the harness was for. The
        `wazero` hits left in `runtime/src/sys.rs` and `net/wasmedge.rs` are
        comments about runtime behaviour, not a harness.

      Nothing was ever built, which is the outcome the entry was steering
      toward. ⚠️ If a THIRD engine is ever wanted, re-price it against the Node
      host rather than against the 2026-08-09 situation this entry describes.

- [x] **(original framing) Re-price the proposed wazero debugging harness before building it.** Its
      symbolization premise has already landed and timestamped traces answered
      the two investigations that motivated it.
      — *source: `Session summary: where nginx stands, 2026-08-09`*

## The inlined call history (elfconv patch 0060), 2026-08-16

Everything below concerns a feature that is **doubly gated and off by default**:
a module must be BUILT with `translate-one --inline-call-history` and ecvisor RUN
with `RAPTORMARK_ECV_INLINE_CH=1`. The default path is measured free and the full
slow suite is green both ways, so none of this blocks anything.

- [ ] **DECIDE whether the opt-in is worth keeping at all.** The measured price,
      after the budget was raised far enough for the largest closure to finish:

      | | measured |
      |---|---|
      | runtime, call-heavy | -23% |
      | runtime, realistic | -2.8% |
      | translation time | **+39% to +111%**, closure-dependent |
      | module size | +10% |

      Doubling the project's dominant build cost for 2.8% on server-shaped work
      is a poor trade. -23% on INTERPRETER-shaped guests is defensible, and that
      class (python, ruby) is in the README's scope -- but that case has never
      been measured on an actual interpreter, only on a synthetic call loop.
      Measure python before deciding, not the microbenchmark.
      — *source: `2026-08-16 -- Final gate-on validation`*

      ⚠️ **MEASURED 2026-08-19, and the interpreter case DOES NOT HOLD.**
      `python:3-slim` fused, one ELF translated both ways, five interleaved
      rounds, ranges not means, startup subtracted pairwise:

      | guest | default | inline-CH | verdict |
      |---|---|---|---|
      | C call loop (control) | [2862, 2896] ms | [2251, 2275] ms | **-21.4%, bands SEPARATE** |
      | python call-heavy | [53581, 53863] ms | [53833, 54164] ms | **+0.5%, bands OVERLAP** |
      | python realistic | [21122, 21611] ms | [21007, 21319] ms | -0.6%, bands overlap |

      The control is the point: the same machine, builder, harness and hour
      resolve -21.4% on the original microbenchmark, so the harness can see the
      effect and python does not have it. The mechanism acts on the guest BL;
      an interpreter is call-shaped at the PYTHON level, where one call is
      thousands of guest instructions containing few BLs. Build cost on python
      was also worse than recorded here: **+18.6% module size** (not +10%) and
      +26.4% translation.

      So the entry above can now be decided on evidence rather than on a
      microbenchmark: the only workload class that justified the opt-in does not
      benefit from it. **The decision is still the user's to take.** See
      JOURNAL.md, `2026-08-19`, and `.agents-workspace/fixtures/pybench/`.
- [ ] **The adopt/publish invariant has no enforcement, and that is the defect.**
      Correctness requires every Rust touch of `call_history` to be bracketed by
      `adopt_call_history_depth` / `publish_call_history`. Nothing in the type
      system or the compiler checks it. THREE holes were found in three different
      places (replay pop, gate-without-build-marker, bring-up before the first
      publish), the design was declared complete after the first, and the third
      was caught only by a guard kept on a hunch after it had refuted the
      hypothesis it was written for. A green suite is not evidence there is no
      fourth. If this feature is kept, make the invariant structural -- e.g. a
      borrow-guard type that adopts on construction and publishes on drop, so a
      bare `ctx.call_history` access does not compile.
      — *source: `2026-08-16 -- Three holes, one shape`*
- [x] **Measure the +10% module size against the edge-runtime budget.**
      ✅ **ANSWERED 2026-08-22: it cannot be decisive, because the BASE artifact
      already misses the budget by a large multiple.** The entry's premise is
      correct -- `README.md` does say "Fine for a server host; too big for
      tightly bounded edge runtimes" -- and that is exactly why the delta does
      not matter. Measured sizes on this tree:

      | artifact | default | with inline-CH |
      |---|---|---|
      | C call loop (smallest real module) | 4.67 MiB | 5.01 MiB (+7.4%) |
      | `python:3-slim` | 32.54 MiB | 38.58 MiB (+18.6%) |
      | postgres 4-program closure | **361.03 MiB** | not built |

      Edge script budgets are single-digit MB. The SMALLEST artifact here is
      already at that scale before the feature is enabled, and a real guest is
      **3x to 70x** over it. A 7-19% delta cannot move a threshold that the base
      artifact misses by an order of magnitude, so **module size supplies no
      evidence either way for the inline-CH keep/drop decision above.** Decide
      that entry on the runtime measurements (which showed no benefit on the one
      workload class that justified it) and on build cost (+26.4% translation on
      python), not on size.

      ⚠️ This closes "does +10% move a real threshold", NOT "is the artifact
      small enough for an edge runtime" -- it plainly is not, which is the
      standing README limit and a different problem (the lift expansion ratio
      `wasm ≈ 6.89 × .text + 2.37 MB`, not this feature). If a SPECIFIC edge
      target with a SPECIFIC budget is ever chosen, re-check against that number
      rather than this reasoning.

- [x] **(original framing) Measure the +10% module size against the edge-runtime budget.** The
      README already calls the artifact too big for tightly bounded hosts;
      whether +10% moves any real threshold is unexamined.

      ⚠️ **The +10% is not a constant.** Measured 2026-08-19 on one ELF each:
      the C call loop pays +7.4% (4.67 -> 5.01 MiB) and `python:3-slim` pays
      **+18.6%** (32.54 -> 38.58 MiB). The penalty tracks call-site density, so
      the workload with the most call sites per byte pays most -- and, per the
      entry above, collects none of the runtime benefit.

## Found by strengthening the postgres query, 2026-08-19

- [x] **`FNMUL` is undecoded, and it is on postgres's PLANNER path.**
      ✅ **CLOSED 2026-08-21 by verification.** `patches/0064-fnmul-and-fnmadd.patch`
      exists and names `fnmul` 25 times; the "Next postgres milestones" entry
      above independently records the planner path clearing with it
      (`PG-RESULT rc=0 const=1 seqscan=1 join=1 order=1`, same values and same
      `lsn=0/14EC758` as the native run).

      ⚠️ **The inventory table below is still LIVE and is why this entry stays
      readable.** Closing it closes `fnmul` (9 sites, 0.3%), NOT the surface:
      `st1` 686, `sli` 574 and `fcvt` 212 remain, and the two ⚠️ rulings under
      the table are the durable part -- site count ranks coverage-per-effort and
      NOT what unblocks a workload (0063 cleared the largest family, 706 `tbl`,
      and moved nothing observable; 0064 cleared 11 sites and unblocked the
      planner), and a reachability pass is the missing half. Remaining families
      are tracked under `## Instruction coverage after patches 0063/0064`.
      Original text below.

- [x] **(original framing) `FNMUL` is undecoded, and it is on postgres's PLANNER path.** Encoding
      `0x1e7e8800` (`fnmul d0, d0, d30`) at `0x9bd3f4`, inside
      `get_variable_numdistinct` just before it tail-calls `clamp_row_est`.
      **7 sites** in the fused postgres. FNMUL appears nowhere in `patches/` or
      the decoder, so the lifter emits `__ecv_warning` and executing it is fatal:

          fatal: no lifted instruction at 0x9bd3f4 was executed (__ecv_warning)

      ⚠️ That message names a missing BLOCK and the cause is a missing
      INSTRUCTION. It cost an hour of misattribution to patch 0062, which
      changes block discovery, because the wording matches that failure exactly.

      **Why nothing found it before.** The recorded postgres milestone is
      `SELECT 6*7`, which is constant-folded and never plans over a relation.
      The query that reaches it is `SELECT count(*) FROM pg_class` -- a seq scan
      whose row estimate goes through the statistics code. Any future postgres
      validation should plan over a real relation for this reason.

      ✅ **Inventory RUN 2026-08-19** (cold cache, verbose, `dup/pgcl4` closure,
      `.agents-workspace/drivers/undecinv/undecinv.sh`): 11,401 reported sites,
      8,451 of them `enc=0` padding, **2,950 real across 398 encodings**.

      ✅ **`tbl` DONE 2026-08-19** by `patches/0063-tbl-three-and-four-register-tables.patch`.
      Cold remeasurement on `raptormark-builder:tbl0063`: **706 -> 0**, real
      sites 2,950 -> 2,244, with the `enc=0` padding control unchanged at 8,451.
      Guarded by `e2e/tbltable_test.go` against a native oracle; fails on the
      pre-patch builder. `tbx` (6 sites) is deliberately NOT included -- it needs
      `Vd` as a read operand and a different sema shape.

      | sites | encs | family |
      |---|---|---|
      | ~~706~~ **0** | 10 | ~~`tbl`~~ ✅ |
      | **686** | 68 | `st1` |
      | **574** | 59 | `sli` |
      | 212 | 54 | `fcvt` |
      | 126 / 120 | 9 / 6 | `trn1` / `trn2` |
      | 97 | 33 | `uzp2` |
      | 75 | 52 | `fabd` |
      | 69 / 68 | 10 / 12 | `st4` / `ld4` |
      | **9** | 6 | **`fnmul`** |

      The top three are **67% of the real surface**. `fnmul` is **0.3%**.

      The same three families lead an independent inventory taken from the
      cryptography image in another session (`st1` 736, `tbl` 706, `sli` 535),
      so these are lifter-wide gaps and a patch pays off on every glibc target.

      ⚠️ **Site count ranks coverage-per-effort, NOT what unblocks a workload.**
      `fnmul` is 9 sites and sits on the planner's hot path; `tbl` is 706 sites
      and may be reached by nothing postgres executes. Deciding what to implement
      needs both numbers and this inventory supplies only one. A reachability
      pass -- which undecoded sites does a real workload actually execute -- is
      the missing half.
      — *source: `2026-08-19 -- The undecoded inventory for the postgres closure`*

## Ruby and the JIT boundary, 2026-08-19

- [x] **DECIDE whether to adopt `patches/0062` and `patches/0063` into the
      shipping base.** ✅ **ALREADY ADOPTED -- verified 2026-08-22 on the IMAGE,
      not on the tag name.** This is not a decision anyone still owes; it was
      taken and the cost was paid. `raptormark-elfconv-base-patched:sisd0065` is
      the base every builder in use layers onto, and it carries all four of
      0062-0065:

      | patch | marker checked in the base image | found in |
      |---|---|---|
      | 0062 | the gate reads `if (br_bb && seeded == 0 && entry_read) {` -- the POST-patch form, with the `!entry_is_landing_pad` proxy REMOVED | `BC/TraceLifter.cpp:1205` |
      | 0063 | `TryDecodeTBL_ASIMDTBL_L3_3` / `_L4_4` | `Arch.cpp`, `Decode.h`, `Semantics/SIMD.cpp` |
      | 0064 | `FNMUL` | `Arch.cpp`, `Decode.h`, `Semantics/BINARY.cpp` |

      ⚠️ **`entry_is_landing_pad` alone does NOT prove 0062 is applied** -- the
      symbol PREDATES the patch, which only removes it from that one condition.
      Check the CONDITION, not the identifier. Getting this wrong reads as
      "adopted" on an unpatched base.

      The entry's validation table below stands and its warning was borne out:
      the census of 2026-08-22 found postgres executing ZERO undecoded
      instructions on this base, so the "does not regress it" finding held at
      scale. Original text below.

- [x] **(original framing) DECIDE whether to adopt `patches/0062` and `patches/0063` into the
      shipping base.** Both are written, side-built, and validated as far as
      this machine can validate them. Adopting means a `BaseID` change and
      therefore a
      full re-translation of every cached object.

      | guest | reaches the changed branch | result |
      |---|---|---|
      | ruby | yes | **FIXED** -- runs, checksums match the native oracle |
      | python | yes | runs, checksums identical, timings inside pre-patch bands, **+20 bytes** |
      | nginx | no (0 functions) | **byte-identical object AND module** |
      | E2E suite | no (0) | 90/4/0, regression control only |
      | **postgres** | **no, measured** | ✅ **does not regress it** |

      ✅ **postgres validated 2026-08-19.** The closure translates (24m39s,
      297.57 MiB), initdb completes in full, and the recorded milestone query
      returns 42 with `lsn=0/14EC758` -- the same WAL position as the native run
      and as the 2026-08-12 record.

      ⚠️ The census predicted <=1,603 affected postgres functions. **Measured: 0.**
      Both builders produce **122,411 lifted functions** and modules differing by
      63 bytes; the per-object delta is a constant 42 bytes across objects of
      17/143/248 MB, i.e. metadata. The census proxy for `seeded == 0` ("no
      `bti j` in the function") over-counts badly, because `CollectBTIJumpPads`
      seeds from pads beyond the function's own extent. ruby is the only fixture
      where 0062 does anything at all.

      A harder query set (planner over real relations) DOES fail -- on
      `fnmul`, undecoded -- but **fails identically on the pre-patch builder**,
      so it is pre-existing and tracked separately above.

      Guarded by `e2e/pacjumptable_test.go`, which fails on the pre-patch
      builder with the original `out of bounds memory access`.

- [x] **`ruby:3-slim` builds but does NOT run.** ✅ **FIXED 2026-08-19 by
      `patches/0062-jump-table-sweep-not-gated-on-pac.patch`.** All three guests
      run and their checksums match the native oracle. What remains is the
      ADOPTION decision below, not the diagnosis. Original entry kept for the
      evidence trail.

      Nothing in this tree had ever
      established that it does, and README's image survey reads as though it
      were fine. Fused 10.98 MB, 6m17s translate, 44.67 MiB module; dies after
      all 25 `_dl_init` constructors with an out-of-bounds `i64.store` to guest
      `0xffffffc0` inside `rb_method_definition_set` (guest VMA `0x918724`),
      reached via `rb_define_private_method` -> `rb_add_method_cfunc` ->
      `rb_method_entry_make`. Ruby is registering its built-in methods.

      The uninitialized-TLS hypothesis was CHECKED AND REFUTED: neither the ruby
      nor the working python fused image carries a `PT_TLS` header, and both
      carry three `.ecv.tls` sections. Do not restart from there.

      ⚠️ **ROOT CAUSE FOUND the same day, confirmed on three independent
      signals.** `RAPTORMARK_ECV_DEBUG=1` gives exactly one non-PLT `bbmiss`:

          [bbmiss] fn=0x918724 bb=0x918e80 nblocks=344 has_catchall=true
                   near=[918ec0, 918ec4, ... 918f00]

      `0x918e80` is a landing pad of the compact jump table at `0x918848`
      (`ldrh` + `adr` + `add ..., sxth #2` + `br`) and the lifter never recovered
      it as a basic block. The branch falls to the `UINT64_MAX` catch-all and
      thence to `__remill_jump`, which re-enters the containing function -- which
      is precisely what the crash stack shows, alternating
      `rb_method_definition_set` / `__remill_jump` for its whole printed length.
      The re-entry arrives with `sp == 0`, so the prologue's
      `sub sp, sp, #0x60` + `stp x29, x30, [sp, #32]` stores eight bytes at
      `-0x40` = **`0xffffffc0`**, matching the reported address, width and
      instruction kind exactly.

      **The fix is a lifter patch** (block discovery for this jump-table form),
      hence a `BaseID` change, hence a full re-translation of every cached
      object -- so per the "batch lifter patches" entry above it should ride with
      other lifter work rather than be spent alone. It also does not follow that
      this is ruby's only gap; python needed several, found one at a time.

      ⚠️ **NARROWED TO ONE CLAUSE.** Patch 0025 already implements the fix and
      already documents this exact failure (it was written for nginx's
      `_ecv_fde_df5954`). It is gated on

          if (br_bb && seeded == 0 && entry_read && !entry_is_landing_pad) {

      and `entry_is_landing_pad` counts `paciasp`/`pacibsp`. Ruby's function
      starts with `paciasp`, so the guard fails and the symtab sweep never runs
      -- while the function contains **zero `bti j`**, so there are no landing
      pads and `seeded == 0` (the clause that should admit it) holds.

      The first hypothesis, "ruby is PAC-only", is REFUTED: ruby has 2,320
      `bti c` and 886 `bti j`. It is MIXED -- 0.51 `bti j` per 1,000
      instructions vs python's 2.52 -- because glibc is branch-protected and
      libruby is not. `entry_is_landing_pad` asks "is this BINARY branch
      protected" as a proxy for "did the BTI scan already cover this FUNCTION",
      and `seeded == 0` in the same condition already asks the real question.
      On a mixed binary the two disagree.

      ⚠️ Before adopting: dropping the proxy admits the sweep for every
      `br`-bearing function that seeded nothing, on EVERY guest. Measure seeded
      blocks, module size and translation time on a known-good fixture
      (nginx or python) as well as on ruby -- the change is one clause, its
      blast radius is not.

      Artifacts kept, so the next attempt starts at the crash rather than at a
      6-minute translation: `.agents-workspace/fixtures/rbbench/`. Function
      indices in a wasmedge trap stack resolve with
      `.agents-workspace/drivers/wasmnames.py`.
      — *source: `2026-08-19 -- ruby:3-slim does not run`*

- [ ] **⚠️ Ruby's OTHER startup mapping is FATAL on failure, and it has ~74 MiB
      of headroom.** Found 2026-08-22 while answering the 384 MiB question
      below. Filed separately because it was first recorded INSIDE that
      now-closed entry, where open work is invisible -- the second time in this
      session a live risk was written into a `[x]` box.

      `Init_default_shapes` makes TWO mappings. The 384 MiB one (the redblack
      ancestor cache) is refused and ruby degrades gracefully. The one issued
      **immediately before** it is `SHAPE_BUFFER_SIZE * sizeof(rb_shape_t)` =
      **20,971,520 bytes**, and on failure ruby calls **`rb_memerror()`** -- it
      does not degrade, it dies at startup.

      It succeeds today, at arena bump `0x101a0000..0x115a0000`, leaving roughly
      **74 MiB** of the private mmap window. So ruby currently boots on margin,
      not on comfort. ❗ Anything that raises arena pressure before this point --
      a larger fused image, more shared memory, an earlier guest allocation --
      turns a working ruby into one that cannot start, and the failure will look
      like a raptormark bug rather than a budget.

      ⚠️ **The general finding, which is bigger than ruby.** Natively that
      384 MiB reservation is nearly free: lazily committed, measured **RSS 28 kB
      of 384 MiB** in `/proc/self/smaps`. Under ecvisor a mapping IS address
      space in a fixed linear memory, so **a reservation and a real allocation
      cost exactly the same**. Any guest that reserves address space it never
      touches pays full price here. Ruby is simply the first one measured doing
      it -- and it asks for exactly `MEMORY_ARENA_SIZE`, so that request can
      never succeed at the current arena size no matter how the window is
      arranged.

      Not urgent, and say why: nothing fails today, and the degraded path costs
      **2.7x on ivar reads for objects with >= 10 ivars** and nothing measurable
      at 14 ivars. What this entry asks for is a decision about MARGIN, which is
      the operator's -- or the lazy-reservation problem solved generally, which
      is design work nobody has scoped.
      — *source: the `Init_default_shapes` investigation below*

- [x] **Ruby's 384 MiB startup mapping is refused, and the size is a confusing
      coincidence.** ✅ **ANSWERED 2026-08-22. It is `Init_default_shapes`
      (`shape.c:1218`), Ruby 3.4's object-SHAPE tree — not the GC heap and not
      the fiber pool.** The mapping is the redblack ANCESTOR CACHE:

          REDBLACK_CACHE_SIZE x sizeof(redblack_node_t)
        = (SHAPE_BUFFER_SIZE * 32) x 24
        = (0x80000 * 32)          x 24
        =  16777216               x 24  =  402653184

      Every factor is a COMPILE-TIME constant — no `sysconf(_SC_PAGESIZE)`, no
      `RUBY_GC_HEAP_INIT_SLOTS`, no `RUBY_FIBER_*`, no env var. It is 384 MiB on
      any host at any page size. Taken from the shipped binary's own
      `.debug_macro`, not from memory: `libruby.so.3.4.10` in `ruby:3-slim` is
      unstripped with `debug_info`.

      **The coincidence is only a coincidence.** The number is fixed inside
      ruby's binary and reproduces byte-identically on a native aarch64 Linux
      host with no ecvisor present. Nothing ecvisor does can move it, and
      `MEMORY_ARENA_SIZE` did not put it there.

      **⚠️ Do not confuse it with the fiber pool.** `fiber_pool_allocate_memory`
      is a DIFFERENT mapping in the same startup — 21233664 bytes
      (32 x 663552, `MAP_STACK`), and it succeeds. Wrong by 19x.

      **The ENOMEM is harmless; the fallback is a named, bounded cost.**
      `shape.c:1252` sets `shape_cache = NULL` and `cache_size =
      REDBLACK_CACHE_SIZE`, which makes `redblack_new` return `LEAF` forever, so
      `rb_shape_get_iv_index` always takes the linear walk up the shape parent
      chain instead of the O(log n) lookup. Correctness unaffected. Measured
      natively by failing exactly that one mmap: **2.7x slower** (0.009 -> 0.024
      s, three runs each) for ivar reads on 500-ivar objects with the inline
      cache defeated, and **no observable difference** at 14 ivars. It only
      applies to objects with >= 10 ivars, and it scales with chain depth.

      ⚠️ The OTHER mapping in the same function — `SHAPE_BUFFER_SIZE` x
      `sizeof(rb_shape_t)` = 20971520 (20 MiB), issued immediately before —
      calls `rb_memerror()` if it fails. It currently SUCCEEDS (arena bump
      `0x101a0000..0x115a0000`). If arena pressure ever grows enough to refuse
      that one, ruby dies at startup.

      **Corollary worth keeping.** Ruby asks for a mapping exactly the size of
      the whole arena, so this request can never succeed under a 384 MiB
      `MEMORY_ARENA_SIZE`. Natively it is nearly free — lazily committed, RSS
      **28 kB of 384 MiB** in `/proc/self/smaps`. Under ecvisor a mapping is
      address space in a fixed linear memory, so a reservation and a real
      allocation cost the same. That asymmetry, not the numeric collision, is
      the durable finding. Making the mapping succeed remains a decision nobody
      has taken; on this evidence it buys an ivar-lookup optimisation for
      wide-object workloads at the price of the entire arena.
      — *evidence: `JOURNAL.md`, `2026-08-22 -- Ruby's 384 MiB startup mapping
      is `Init_default_shapes`' redblack cache`; probes kept in
      `.agents-workspace/tmp/rbmmap/`*

- [x] **(original framing) Ruby's 384 MiB startup mapping is refused, and the
      size is a confusing
      coincidence.** `mmap region exhausted (want 402653184 bytes, bump
      0x115a0000, 0 hole(s), shm_top 0x16000000) -> ENOMEM`. Ruby handles the
      ENOMEM and continues, so it is not the crash — but 402653184 is exactly
      `MEMORY_ARENA_SIZE`, which invites a wrong reading. Establish what ruby is
      actually asking for (GC heap reservation, most likely) before someone
      connects the two numbers.
      — *source: `2026-08-19 -- ruby:3-slim does not run`*

- [ ] **⚠️ README's image survey puts `ruby:3-slim` on the wrong side of the JIT
      line, and the line itself may be in the wrong place.** Verified on the
      image: YJIT is COMPILED IN (`RbConfig YJIT_SUPPORT: "yes"`) and one flag
      away (`ruby --yjit -v` reports `+YJIT`, `RubyVM::YJIT.enabled?` is `true`
      under it), merely off by default. The survey lists ruby as "interpreted +
      6 native extensions" while marking `node:22-slim` and temurin **JIT** and
      out of scope.

      The scope argument -- "a runtime that emits aarch64 as it runs has no
      machine code to lift ahead of time" -- is about what the GUEST DOES, so
      one fused artifact is in scope or out of it depending on argv. A per-image
      column cannot express that. Decide whether the table gains a note, a
      column, or a different axis.

      ⚠️ **AMENDED 2026-08-22 -- the axis question is now sharper, and README's
      SCOPE SENTENCE is not what stops ruby.** Measured below: with `--yjit`
      ruby never reaches YJIT at all (an undecoded NEON `orr` while PARSING the
      flag), and armed without argv it dies on a 128 MiB `PROT_NONE` reservation
      the 96 MiB arena window cannot hold. Neither wall is "there is no machine
      code to lift ahead of time" -- one is a decoder gap and one is an address
      budget. So the survey's JIT column is currently recording a conclusion the
      evidence does not support for ruby, for or against. ❗ Do not rewrite
      README from this; the scope claim is the operator's. What this changes is
      that a note would now have something CONCRETE to say.
      — *source: `2026-08-19 -- ruby:3-slim is a JIT image one flag away`;
      amendment from `2026-08-22 -- What a JIT guest does under ecvisor`*

- [ ] **What a JIT guest actually does under ecvisor -- MEASURED 2026-08-22, and
      it is still not the question README answers.** Left OPEN deliberately:
      YJIT dies TWICE before emitting a single byte, so "what happens when a
      guest emits aarch64 at run time" remains untested. What IS now known:

      | wall | trigger | failure | loud? |
      |---|---|---|---|
      | 1 | any `--yjit*` in argv | SIGILL, undecoded `0f04141c` `orr v28.2s, #0x80` at guest `0x87ab18` in `proc_options` | yes, `[BUG] Illegal instruction`, exit 127 |
      | 2 | `RubyVM::YJIT.enable` or `RUBY_YJIT_ENABLE=1` | 4x `ENOMEM` on a 128 MiB `PROT_NONE` reservation -> `<internal:yjit>:51: [BUG] mmap failed` | yes, exit 127 |

      **Wall 1 is a decoder gap, not a scope fact.** The instruction is the
      vectorised `FEATURE_SET(opt->features, FEATURE_BIT(yjit))` -- `feature_yjit`
      is bit 7, `1U << 7 == 0x80`, and `ruby_features_t`'s `{mask, set}` pair lets
      GCC do both words in one `ORR (vector, immediate)`.
      `TryDecodeORR_ASIMDIMM_L_HL`/`_L_SL` are stubs returning `false`. **argv
      cannot arm YJIT under ecvisor at all** -- `--yjit-exec-mem-size=8` and
      `--jit` hit the same address.

      **Wall 2 is arithmetic, not pressure.** `MMAP_START_VMA 0x1000_0000` ..
      `MMAP_END_VMA 0x1600_0000` is a **96 MiB** window; YJIT's default
      `--yjit-exec-mem-size` is **128 MiB**. It can never fit however idle the
      guest is. Same asymmetry as the `Init_default_shapes` 384 MiB cache: under
      ecvisor a lazily-committed reservation costs what an allocation costs.

      ❌ **It is NOT a refused `PROT_EXEC`** -- `NR_MPROTECT => state.set_ret(0)`
      is unconditional, so YJIT's 47 W^X toggles would have silently succeeded.
      ❌ It is NOT a jump into unlifted bytes. ❌ YJIT does not disable itself
      gracefully. Neutralized against a native `LD_PRELOAD` oracle that refuses
      the same mmap: identical message and backtrace with no ecvisor present, so
      the loudness is ruby's.

      **What is still owed**, and it needs a decision before it needs work:
      making YJIT actually emit requires passing wall 1 (a lifter patch, hence a
      `BaseID` change) AND wall 2 (arena size or executable mappings, which
      README explicitly declines). Whether that is worth doing purely to observe
      the next failure is the operator's call.
      — *evidence: `JOURNAL.md`, `2026-08-22 -- What a JIT guest does under
      ecvisor`; artifacts in `.agents-workspace/fixtures/rbbench/ruby-rbprctl.wasm`
      and `.../rbbench/yjit-2026-08-22/`*

- [x] **(original framing) What a JIT guest actually does under ecvisor is
      UNTESTED.** A `--yjit`
      sidecar was run and failed at the same instruction as every other ruby
      sidecar, i.e. long before YJIT could matter. README's claim is still an
      assertion. Do not record that run as evidence about JIT.
      — *source: `2026-08-19 -- ruby:3-slim does not run`*

- [ ] **⚠️ Ruby under ecvisor needs `--disable-gems`, and nothing said so.**
      Found 2026-08-22. `ruby -I ... /script.rb` without it dies at
      `*** longjmp causes uninitialized stack frame ***` -- glibc's
      `__longjmp_chk` -> `__fortify_fail` -> abort, while RubyGems loads --
      followed by `[ecvisor] fatal: guest trap ... at PC 0x1621ae0
      (__remill_error)`. With `--disable-gems` the same script runs to
      completion and matches the native checksum.

      Every preserved 2026-08-19 sidecar carries
      `--disable-gems -I /usr/local/lib/ruby/3.4.0 -I .../aarch64-linux` and
      none of them says why, so a sidecar built the obvious way fails in a way
      that reads exactly like a builder regression. It cost an hour and a wrong
      hypothesis this session; the refutation was running the NEW sidecar
      against the KNOWN-GOOD 2026-08-19 module and watching it fail there too.

      Not diagnosed further -- nobody has localised which of RubyGems' startup
      `setjmp`/`longjmp` pairs ecvisor mis-serves, or whether it is the machine
      stack bounds `__longjmp_chk` consults.
      — *evidence: `JOURNAL.md`, `2026-08-22 -- What a JIT guest does under
      ecvisor`, "Incidental" section*

## Multi-module modules (`.agents/docs/MULTIMODULE.md`), 2026-08-15

- [x] **~~DECIDE whether raptormark is willing to own a wasm embedder.~~
      ✅ DECIDED 2026-08-18: SUPPORT BOTH.** Kept for the evidence trail; the
      analysis below is still accurate, only its framing as an open question is
      not. ⚠️ This entry was re-raised as open on 2026-08-19 after it had been
      settled -- if you find yourself asking the question again, it is answered
      here. Remaining C4 work is ENGINEERING: real-program scale through the
      §8 harness, and keeping the side path under test so it cannot rot.

      This is
      the single question the whole multi-module analysis reduces to: the module
      shapes work and were built and run, but no host on the shipping path
      instantiates more than one module. Proposal 1 (supervisor as its own
      module) is ENTAILED by proposal 2 (programs as modules), not independent of
      it. See MULTIMODULE.md §4-5.

      ⚠️ **It is no longer an EITHER/OR.** Measured 2026-08-18 (§5 amendment):
      one set of `-fPIC` objects links BOTH ways -- ordinary link for the stock
      shim (319 imports, no `__memory_base`, byte-comparable to the non-PIC
      build) and `--experimental-pic -shared` for the embedder path -- so the
      expensive half of the pipeline is shared and only the final link forks.
      What supporting both actually costs is the supervisor in two shapes (Rust
      work behind a cfg), two link paths under test, and one wasm-ld crash on
      `-shared --export-all`. The decision to make is therefore "is the split
      worth its own maintenance", not "which future do we give up".

      ⚠️ **AMENDED 2026-08-18 (MULTIMODULE.md §9): the protocol has now been
      RUN.** `e2e/testdata/embedder.mjs` drives the full §8 sequence -- reserve,
      place, relocate, register, start -- over real artifacts, placing TWO side
      modules disjointly against a supervisor module, and the lifted guest
      prints the same line the flat module prints under wasmedge. Eight
      mutations, eight caught. So the decision no longer carries the risk of
      "and find out whether it works"; it is now only "is a shipping embedder
      worth owning". Node is a DEVELOPMENT host and cannot become the answer: it
      supplies none of WasmEdge's 11 socket imports, and `node:wasi` aborts the
      process outright on the first WASI call after the guest grows memory,
      which every raptormark guest does.
- [ ] **⚠️ The cross-module call penalty CANNOT be measured first.** Attempted
      2026-08-18 (MULTIMODULE.md §6); this entry used to say "measure before
      committing", and that is not achievable. wasmedge cannot instantiate two
      modules (re-verified on 0.17.1), and V8 -- which can -- INLINES the
      cross-module import, returning 0.98x of a direct call.

      What the attempt did establish: **under AOT an intra-module call is free
      because it is inlined** (0.15 ns/call on wasmedge, 0.27 on V8), and a call
      costs something only in the interpreter (~27 ns direct, ~33 ns indirect).
      So the split's running cost is not a call-overhead multiplier -- it is the
      LOSS OF INLINING across the boundary, which is a larger effect and one that
      only exists once the boundary does. The decision cannot be de-risked by
      measurement; it has to be made and then measured.
- [ ] **⚠️ PROSPECTIVE, and there is now an automatic guard that would fire.**
      Re-verified 2026-08-22.

      ⚠️ **CORRECTION, same day: an earlier version of this note said the 22 line
      "is not installed and cannot ship anything today". That is WRONG**, and it
      was wrong because it inspected only the builder in current use. LLVM 22 is
      a FIRST-CLASS, BUILT line:

      - `build-image --llvm` is `enum:"16,22"` (`internal/builder/buildimage.go:23`)
        and `README.md:453` documents `build-image --llvm 22`.
      - The images exist on this machine: `raptormark-builder:llvm22` and
        `:llvm22-v2` (2026-07-31), `raptormark-elfconv-base:llvm22-v2`
        (2026-08-12). `:llvm22-v2` carries **both** `/usr/lib/llvm-16` and
        `/usr/lib/llvm-22` with `ECV_LLVM_VER=22`.

      **Why 16 is still the default**, from the code rather than from memory:
      `builder/Dockerfile:92` -- "16 = the pinned elfconv line; 22 = the LLVM-22
      base ... ECV_LLVM_VER is exported so translate-one picks the matching
      llvm-link/opt/split/clang (**bitcode must match**)". elfconv is a submodule
      pinned clean at upstream and built against LLVM 16, so the LIFTER sets the
      line and everything downstream must agree with it. `buildimage.go:57`
      states the policy: "`latest` follows the pinned LLVM-16 line only. An
      explicit --tag (or the llvm22 line) is a side build and **must not move
      it**."

      So the 22 line was built and kept deliberately as a side build. What was
      never done is the check this entry names -- and that, not availability, is
      the blocker.

      ⚠️ Also relevant before anyone measures: the `llvm22` builder is from
      **2026-07-31** and its base from 08-12, so it PREDATES `patches/0062-0065`.
      Any comparison against a current `:sisd0065`-based module differs by the
      patch series as well as by the toolchain, and attributing a difference to
      LLVM would be wrong.

      **What has changed is the safety net.** When this was written, the Wasm 2.0
      claim rested on `TestWasmOptEnablesNoProposal`, which only checks the flags
      handed to `wasm-opt` -- it cannot see a proposal that clang emitted.
      `TestLoopbackModuleRunsOnStockWasmtime` now runs a real lifted module under
      `wasmtime run` with **no flags at all**, and it is green. A toolchain that
      started emitting `nontrapping-float-to-int`, `bulk-memory` or
      `reference-types` into the module would fail that test on the day it
      landed, without anyone remembering this entry.

      ⚠️ That guard covers the LOOPBACK profile only. A proposal reaching the
      DEFAULT profile alone would still pass, which is the same limit recorded
      against the `--enable-all` decision. So: keep this entry, but the failure
      mode it guards against is now loud rather than silent for most of the
      surface. Original text below.

- [ ] **(original framing) The LLVM 22 line's Wasm feature set is wider than the LLVM 16 line's.**
      Noted while probing, NOT introduced by any of this work: clang-22 defaults
      showed `nontrapping-float-to-int`, `bulk-memory`, `reference-types`,
      `multivalue`, `bulk-memory-opt`, `call-indirect-overlong`. That was a
      trivial file, not the real pipeline's output — but the Wasm 2.0 claim is
      load-bearing for the stock-shim property, so the 22 line needs its own
      check before it ships anything.
      — *source: `2026-08-15 -- P0.2 toolchain probes`*

## Found during the patch 0061 postgres validation, 2026-08-17

- [x] **CLOSED 2026-08-22 -- this is the measurement the ruling was made on.**
      Bounded snapshots became the default and the variable named below was then
      REMOVED the same day, so there is no unbounded path left to select. See the
      decided entry near the top of this file. ⚠️ The original entry that follows
      names a variable that no longer exists; it is history, not an instruction.
- [x] **`RAPTORMARK_ECV_BOUNDED` is not the default and that means "no guest-side
      clients".** Measured 2026-08-17 on the same module, one variable: unbounded
      dies with `memory allocation of 402653184 bytes failed` (exactly
      `MEMORY_ARENA_SIZE`) the moment psql forks as a fourth process, while
      bounded runs all four and the client connects on the first try. `arena.rs`
      already states a `Full` snapshot "caps concurrency at one guest-side
      client". This is evidence FOR README item 3 rather than a new finding, but
      it reframes the decision: the question is not whether bounded is worth its
      switch cost, it is whether shipping without clients is acceptable.
- [x] **DONE 2026-08-23 -- the reclaim defect is fixed and the five-program
      module reaches `BOOT: DONE`.** `shm_file_reclaimable` (`context.rs`) now
      holds a `SharedKind::File` region only while its path resolves AND some
      mapping of it was ever WRITABLE; `NR_MMAP` reads `prot` (it never had) and
      `NR_MPROTECT` marks overlapping regions. Last reclaim before
      `BOOT: postmaster` is back to `shm_top 0x16000000, 0 hole(s)`.
      ⚠️ **The suggested fix below -- scope the rule to `/dev/shm/...` -- was
      REJECTED, with a counterexample**: PostgreSQL's `mmap` DSM backend maps
      `$PGDATA/pg_dynshmem/...` `MAP_SHARED` with the same cross-process
      expectations, and a path prefix does not cover it.
      ⚠️ **The expected AFTER values in that entry are wrong for a run where
      locale WORKS.** `collations=879` and
      `libc_collations=C C.utf8 POSIX en_US en_US.utf8`, not 876 / `C POSIX`;
      reproducing the old numbers would have meant locale was still broken.
      Guards: 6 host tests in `context::shm_file_reclaim_tests`, plus
      `e2e/shmreclaim_test.go`'s `TestSharedFileMappings*`. Full evidence in
      `JOURNAL.md`, "file-backed `MAP_SHARED`: the name is not the rule". The
      original entry follows.
- [x] **The blocker moved, and then it was FIXED. Closed 2026-08-23.** The
      shared-window reclaim defect this entry names was diagnosed and repaired
      the same night: `shm_try_reclaim` now uses
      `shm_file_reclaimable(writable, path_exists) = !(writable && path_exists)`,
      so a read-only `MAP_SHARED` of a rootfs file is reclaimed when its last
      mapper goes while `/dev/shm` POSIX segments keep their name-based
      protection. The five-program run then reached `BOOT: DONE` with
      `shm_top` back at `0x16000000, 0 hole(s)` (from `0x10fd0000, 1 hole(s)`).

      ⚠️ The **writability** discriminator was chosen over the path prefix this
      entry's successor originally proposed, and the reason is worth keeping: a
      `/dev/shm/...` prefix has a concrete counterexample -- PostgreSQL's `mmap`
      DSM backend maps `$PGDATA/pg_dynshmem/...` `MAP_SHARED` with identical
      cross-process expectations and the prefix does not cover it. Writability
      is not a proxy; it is the rule's own premise ("bytes were written through
      this region and live only here") being false.

      Original text below, kept because its cost findings are the durable part.

- [x] **(original framing) STILL OPEN, and the blocker moved. Attempted 2026-08-23; the exec-map
      half is DONE and PROVED, a shared-window reclaim defect is what stops it.**
      Full evidence in `JOURNAL.md`, "`/usr/bin/locale` as a fifth postgres
      program". Do NOT re-derive the fuse options or the cost estimate:

      * The closure-wide layout SURVIVES five programs, unchanged --
        `36 libraries, top 0x7e40248 (ceiling 0x0A000000, 78.9% used)`, identical
        `RAPTORMARK_LIB_RANGES`, and the four existing fused images byte-identical.
        `locale` `DT_NEEDED`s only `libc.so.6` and `ld-linux-aarch64.so.1`.
      * Therefore it is CHEAP, not a 35-40 minute cold re-translation: programs
        0-3 were cache hits at 0s, only `usr_bin_locale.fused` translated
        (15m30s on a host at load ~139), link 1m51s, module 372.76 MiB.
        `locale` must be appended LAST -- index is part of the cache key.
      * The `pgcl4` fuse options, recorded nowhere and recovered by reproducing
        its four images byte-for-byte:
        `-libdirs /usr/lib/postgresql/17/lib -extra /usr/lib/postgresql/17/lib/dict_snowball.so,/usr/lib/postgresql/17/lib/plpgsql.so`
      * With `/usr/bin/locale=4` in the exec map, `Exec format error` and
        `no usable system locales were found` are GONE, and the program genuinely
        works: `locale -a` returns `C / C.utf8 / POSIX / en_US.utf8`, rc=0 --
        what `postgres:17` reports natively.

      ❌ **What stops it**: once glibc's locale path runs, the postmaster dies
      with `could not map anonymous shared memory` (78,618,624 bytes).
      `shm_try_reclaim` (`runtime/src/context.rs:4316`) will not reclaim a
      `SharedKind::File` segment while its path still resolves in the VFS -- a
      rule written for `/dev/shm/PostgreSQL.<n>`, which gets unlinked. glibc
      `MAP_SHARED`s three ORDINARY, PERMANENT rootfs files
      (`/usr/lib/locale/locale-archive` 3,080,192 B, `/etc/locale.alias` 65,536 B,
      `gconv/gconv-modules.cache` 65,536 B), so they are never reclaimed and pin
      `shm_window.top` at `0x10fd0000` for the life of the module. The private
      mmap region is `[MMAP_START_VMA, shm_window.top)`, so it collapses from
      96 MiB to 15.8 MiB. Debug-enabled A/B at the identical line:
      4 programs `shm_top 0x16000000, 0 hole(s)`; 5 programs
      `shm_top 0x10fd0000, 1 hole(s)`. **A reclaim defect, not an address budget.**

      ⚠️ NOT the fifth program: the same module with `/usr/bin` off the boot PATH
      (so `popen("locale -a")` misses) completes end to end and reproduces every
      value -- while `/usr/bin/locale -a` by absolute path still works in that
      same run. ⚠️ `RAPTORMARK_ECV_NO_FILE_SHM=1` is NOT a workaround: initdb
      degrades to `max_connections 25` / `shared_buffers 400kB` and the bootstrap
      dies; it does not drop postgres to sysv DSM as its comment claims.

      **Next step is a runtime decision, not more pipeline work**: scope the
      "path still resolves -> do not reclaim" rule to the POSIX shm namespace
      (`/dev/shm/...`) so a read-only `MAP_SHARED` of a rootfs file is reclaimed
      when its last mapper goes. Deliberately NOT done here -- it changes shared
      segment lifetime semantics and belongs to whoever owns that call.
      Original entry follows.
- [x] **`/usr/bin/locale` is not in the exec map.** ✅ **DONE 2026-08-23,
      demonstrated end to end** -- and it uncovered a runtime defect on the way,
      which is the more valuable half.

      Five-program closure (postgres, initdb, dash, psql, **locale**), fused
      closure-wide, run through the real `boot.sh`:

      - `sh: 1: locale: Exec format error` and `no usable system locales were
        found` are **GONE**.
      - `/usr/bin/locale -a` returns `C / C.utf8 / POSIX / en_US.utf8`, rc=0 --
        matching what `postgres:17` reports natively, so the warning did not
        vanish by the code path being skipped.
      - `BOOT: DONE`, with `1 | raptormark | 10`, `3 | WASM | 4`,
        `rows=2 sum=4`, `pg_database=3`, `pg_authid=16`.

      ⚠️ **Two expected values CHANGED, and the change is the proof.** The run
      reports `collations=879` (not the pre-fix 876) and
      `libc_collations = C C.utf8 POSIX en_US en_US.utf8` (not `C POSIX`),
      because initdb now finds system locales and registers three more libc
      collations. **Reproducing the old values would have meant locale was still
      broken.** A comparison against a BEFORE run has to expect the values it
      was measuring the absence of.

      ✅ **The closure-wide layout survived a fifth program at no cost.**
      `locale` `DT_NEEDED`s only `libc.so.6` and `ld-linux-aarch64.so.1`, both
      already present, so the fuse produced a byte-identical
      `SHARED layout: 36 libraries, top 0x7e40248 (78.9% used)`, the four
      existing fused images hashed unchanged, and programs 0-3 were **object
      cache hits at 0s**. Only `usr_bin_locale.fused` translated (15m30s).
      ❌ So "adding a program to a closure forces a cold re-translation of all of
      them" is FALSE when the new program adds no library -- a prediction made
      when this was dispatched, and wrong.

      **What it exposed**: mapping `locale` made glibc's loaders `MAP_SHARED`
      three permanent rootfs files, which `shm_try_reclaim` could never reclaim,
      pinning `shm_window.top` and collapsing the private mmap region 96 MiB ->
      15.8 MiB until the postmaster could not start. That was a real defect in
      the shared-segment reclaim rule, is fixed, and is recorded above. ⚠️ The
      closure itself lives in `.agents-workspace/tmp/pgloc5/` and is a
      DEMONSTRATION, not a shipped recipe -- there is no committed postgres
      closure definition in this tree to adopt it into.
      Original text below.

- [x] **(original framing) `/usr/bin/locale` is not in the exec map**, so initdb logs
      `sh: 1: locale: Exec format error` and warns `no usable system locales were
      found`. Harmless under `--no-locale` and postgres continues, but a complete
      postgres image should map it. Same class as the `dict_snowball`/`plpgsql`
      closure gap, which is already recorded as solved via `fuse.Options.Extra`.

## Diagnostics that read guest text from the arena, 2026-08-19

- [x] **DONE 2026-08-21 — dropped.** Re-audited to the producer before deleting,
      and the entry holds end to end: the arena is filled ONLY by
      `Arena::load_data_sections`, from `EcvProgram`'s `data_*` tables, and
      elfconv's `MainLifter::SetDataSections`
      (`third_party/elfconv/lifter/MainLifter.cpp:213`) `continue`s on
      `SEC_TYPE_CODE`/`SEC_TYPE_UNKNOWN`, where `Loader.cpp:371` assigns
      `SEC_TYPE_CODE` to anything carrying bfd's `SEC_CODE`. No patch in
      `patches/` touches either site, and none of the ten `.ecv.*` sections
      carries text. So the bytes are NOT obtainable on this path.
      `insn=` and its `arena.slice(bb_vma, 4)` read are gone from
      `block_address`; the format string moved to a pure `bbmiss_message` helper
      (the only part of that diagnostic a host test can reach — `block_address`
      needs a live `ContextInner`) carrying the reasoning, guarded by
      `bbmiss_message_tests::does_not_claim_an_encoding_it_cannot_read`.
      No consumer parsed the field. Original entry follows.
- [x] **`bbmiss`'s `insn=` field has never carried a real encoding.**
      ✅ **DONE 2026-08-21 -- field dropped, and the reason is now proved at the
      PRODUCER rather than inferred from the runtime.** Guest `.text` is not
      merely absent from the arena, it is excluded by construction:
      `SetDataSections` (`third_party/elfconv/lifter/MainLifter.cpp:213`) opens
      its loop with `continue` on `SEC_TYPE_CODE`, which
      `Binary/Loader.cpp:371` assigns to any section carrying bfd's `SEC_CODE`,
      and `grep -rlE 'SetDataSections|SEC_TYPE_CODE' patches/` is EMPTY so the
      fork does not change it. None of the ten `.ecv.*` sections carries text
      either. **Do not re-derive this by reading `context.rs`; the answer is not
      there.** A future diagnostic wanting a real encoding needs a new producer,
      not a different read.

      The format string moved to a pure `bbmiss_message` (`context.rs:1746`) so
      it is testable at all -- `block_address` needs a live `ContextInner` with a
      384 MiB arena and real lifted function pointers -- guarded by
      `does_not_claim_an_encoding_it_cannot_read` (`:1783`), neutralized by
      re-adding the field. No consumer anywhere parses `insn=`.

      ⚠️ This checkbox was reported as ticked on the day and was NOT: four agents
      edited this file concurrently and the mark was lost. The CODE landed; only
      the mark was missing. Re-verify a checkbox against the tree, not against a
      report. Original text below.

- [x] **(original framing) `bbmiss`'s `insn=` field has never carried a real encoding.** All 138
      `bbmiss` lines from a ruby run print `insn=0x00000000`, and the address in
      one of them holds a real instruction. `context.rs`'s `block_address` reads
      the guest word with `self.arena.slice(bb_vma, 4)`; **the arena does not
      contain guest .text**, so the read yields zeros.

      Found by copying that read into `__ecv_warning`'s new diagnostic, where it
      printed `undecoded instruction 0x00000000 at 0x40053c` for an address
      holding `1e688800 fnmul d0, d0, d8`. The copy has been removed -- a
      fabricated `0x00000000` is worse than no encoding, because it is a valid
      aarch64 word (`udf #0`) and reads as an answer.

      Either drop `insn=` from `bbmiss` or source the bytes from somewhere that
      has them (the fused ELF is not available at run time; `EcvProgram`'s
      `data_*` tables are data sections, not text). Until then, ignore the field.
      — *source: `2026-08-19 -- patch 0064`*

## Instruction coverage after patches 0063/0064, 2026-08-19

- [ ] **✅ THE REACHABILITY PASS EXISTS AND HAS BEEN RUN. Answer: ZERO.**
      The "missing half" this entry asks for at the bottom is now built and
      measured (2026-08-21/22). `RAPTORMARK_ECV_UNDEC_CENSUS=1` enumerates every
      undecoded site a workload EXECUTES, in one run, instead of one per
      ~30-minute lift.

      | workload | undecoded sites EXECUTED |
      |---|---|
      | `python:3-slim` -- startup, realistic, callheavy | **0** |
      | postgres -- initdb + postmaster + DDL/DML/aggregates/catalog seqscans | **0** |

      The postgres run COMPLETES and its values are correct (`WASM` uppercased by
      the UPDATE, `sum=4` after the DELETE, `count(pg_database)=3`,
      `count(pg_authid)=16`, checkpoint `lsn=0/1511820`), over REAL catalog
      relations -- the bar this file sets for a postgres validation.

      **So the table below ranks by a number that does not predict capability,
      and now there is evidence rather than suspicion.** `st1` 686 + `sli` 574 +
      `fcvt` 212 = **1,472 static sites reached by NEITHER workload.**
      ❌ Do not spend a `BaseID` change and hours of re-translation on them.

      Site count has now failed three times: `tbl` 706 -> nothing observable;
      `fnmul` 9 -> unblocked the planner; `st1`/`sli`/`fcvt` 1,472 -> unreached.
      **Choose the next lifter patch from what a workload DIES on, then use the
      census to get the WHOLE list rather than the first one.** That is the
      instrument's actual job: it does not tell you what to implement on a guest
      that already works.

      ⚠️ Lower bound, per-input and per-path -- "these two workloads do not reach
      those families", not "nothing does". And ⚠️ **a census is UNSOUND by
      construction**: skipped instructions mean results after the first skip are
      garbage, so trust only the `addr=` lines. A census taken on
      `raptormark-builder:census` or older reports ONE site and must not be
      trusted -- that build died at the next syscall; `:census2` is the first
      correct one.
      — *see JOURNAL 2026-08-21/22*

- [ ] **⭐ FIRST REACHABILITY-PROVEN LIFTER TARGET: `orr` ASIMD immediate, on
      ruby's argv path.** This is the target the census methodology was built to
      produce, and it is the first one. ⚠️ It is a DECISION, not work anyone
      should just take -- a lifter patch changes `BaseID` and invalidates the
      6.2 GB object cache, so it wants batching per the standing constraint.

      Found 2026-08-22 by running, not by inventory. Guest `0x87ab18` in
      `ruby-glibc.fused`, encoding `0f04141c` = `orr v28.2s, #0x80`, inside
      `proc_options` immediately after a `strncmp(arg, "yjit", 4)`. It is the
      vectorised `FEATURE_SET(opt->features, FEATURE_BIT(yjit))`: `feature_yjit`
      is bit 7, so `1U<<7 == 0x80`, and GCC ORs both `u32` words of
      `ruby_features_t{mask,set}` in one vector op.
      `TryDecodeORR_ASIMDIMM_L_HL` / `_L_SL` (`Decode.cpp:17329`, `:17367`) are
      stubs returning `false`, so **every `--yjit*` spelling SIGILLs**
      (`[BUG] Illegal instruction`, exit 127). Verified against
      `--yjit-exec-mem-size=8` too, because the disassembly branches on a `-`
      suffix -- it traps at the identical address.

      **Why this one and not `st1`/`sli`/`fcvt`.** The 2026-08-22 census measured
      python and postgres executing ZERO undecoded instructions, so the 1,472
      static sites in the three largest families are unreached by any workload
      here. This one is reached by a real guest on a real command line and kills
      it. Site count has now failed to predict capability three times; EXECUTION
      has not failed once.

      ⚠️ It unblocks argv parsing only, NOT YJIT. The second wall behind it is an
      address budget (128 MiB reservation into a 96 MiB window) and no decoder
      patch touches it -- see the JIT entry. Implementing this makes
      `ruby --yjit` reach a *different* fatal error, which is progress in
      knowledge and not in capability. Decide it on that basis.
      — *source: `2026-08-22 -- what a JIT guest actually does`*

- [ ] **The query path is blocked by SINGLE sites, not by the big families.**
      Attributing the 2,244 remaining undecoded sites to their containing
      functions: 66 named functions, 391 sites, and **212 of those are
      `__mulhc3`/`__divhc3`** -- complex half-precision arithmetic postgres
      cannot reach. `st1` (686) and `sli` (574) live in OpenSSL/ICU/string code.

      What is actually on the query path, one site each, with encodings:

      | function | enc | insn |
      |---|---|---|
      | `ExecInitAgg` | `0x5eff8421` | `add` (scalar SISD) |
      | `hash_agg_set_limits` | `0x7eff858c` | `sub` (scalar SISD) |
      | `tbm_create` | `0x5f7ae5ff` | `scvtf` |
      | `brincostestimate` | `0x1e43e42e` | `ucvtf` |
      | `float8_timestamptz` | `0x5ee0e800` | `fcmlt` |
      | `ShowGUCOption` | `0x7efed79c` | `fabd` |
      | `json_lex` | `0x6e3b2fff` | `uqsub` |
      | `perform_spin_delay` | `0xd5033fdf` | `isb` |

      ⚠️ **Present is not executed.** `ExecInitAgg` and `hash_agg_set_limits`
      were predicted to stop the 2026-08-19 run and did not -- `count(*)` never
      reached their sites. Do not treat this table as a blocker list; treat it as
      what to check FIRST when a query does stop.

      ⚠️ **Site count is the wrong ranking for capability.** 0063 cleared the
      largest family (706 `tbl`) and moved nothing observable; 0064 cleared 11
      sites and unblocked the planner.
      A reachability pass -- which undecoded sites does a real workload actually
      execute -- is **the missing half**.
      — *source: `2026-08-19 -- patch 0064 unblocks the postgres PLANNER path`*

      🔧 **The INSTRUMENT exists as of 2026-08-21; the RUN has not happened.**
      `RAPTORMARK_ECV_UNDEC_CENSUS=1` makes `__ecv_warning` record an executed
      undecoded site and RETURN instead of aborting, deduped by unique address
      and capped at 4096. The site list is
      `grep -o 'addr=0x[0-9a-f]*'` over the `[undec_census]` lines. Host-side
      pieces in `runtime/src/diag.rs`, the branch in `runtime/src/intrinsics.rs`,
      12 guards in `diag::undec_census_tests`.

      ⚠️ **The mode is UNSOUND and its output is a site list and nothing else.**
      Skipping an instruction means its effect is never applied, so wrong
      answers, hangs and later crashes are all EXPECTED under it. Never diagnose
      a second defect from a census run. The runtime prints a banner saying so
      when armed.

      ⚠️ **The instrument reported ONE site per run until 2026-08-21, and it did
      not say so.** `deliver_pending_signals` armed SIGILL's default action
      (`Pending::Exit(132)` + `suspended`) BEFORE returning 0, so the census arm
      returned into a condemned process and the run ended at the guest's next
      syscall. ✅ **FIXED**: `__ecv_warning` now decides its disposition BEFORE
      posting, via `EcvContext::delivers_to_handler(SIGILL)`, and in census mode
      with no handler posts nothing at all. Do NOT trust a census taken on
      `raptormark-builder:census` or anything older -- a list of one that says
      nothing about truncation reads as a complete list. Use `:census2` or later.
      Guarded by `e2e/undeccensus_test.go`, which now requires TWO distinct sites
      separated by a syscall and a clean exit.
      — *source: `2026-08-21 -- The armed-SIGILL exit is fixed by DECIDING before POSTING`*

      Next step: relink a postgres module against the new runtime (no re-lift --
      this is a `runtime/`-only change; use `raptormark-builder:census2`) and run
      the census over a query that plans over a real relation, e.g.
      `SELECT count(*) FROM pg_class`, then rank the next lifter patch off the
      EXECUTED set.

## Clocks and unrecorded limits, 2026-08-21

- [x] **The guest's `CLOCK_MONOTONIC` is served from the WALL clock, so it is not
      monotonic.** ✅ **FIXED 2026-08-21.** `mono_nanos()` and `to_mono()` in
      `runtime/src/context.rs`; every deadline in `sys.rs` and `context.rs` moved
      onto the monotonic base; `clock_gettime` now reads its `clockid` through
      `clock_base()` instead of ignoring it; `clock_getres` (114) added.
      `ecv_next_deadline_ms` became `ecv_next_deadline_in_ms` and returns a
      DELAY, because a monotonic instant means nothing next to `Date.now()`.
      Guarded by `TestGuestTimersSurviveAWallClockStep` (`e2e/clock_test.go`),
      which steps the host's wall clock by an hour at the first idle;
      neutralized, and all three claim assertions fired while both harness
      checks stayed green. Original text below.

      `now_nanos()` (`runtime/src/context.rs:327`) is
      `SystemTime::now()`, and its own doc comment says both clocks come from it:

      ```rust
      /// Guest futex timeouts are absolute against CLOCK_REALTIME/CLOCK_MONOTONIC;
      /// both are served from this clock, which is the same source
      /// `clock_gettime` reports to the guest.
      ```

      A guest timer therefore follows NTP steps, a laptop suspend, and — in a
      browser — a throttled or backgrounded tab. Verified against the tree
      2026-08-21; unchanged by any of the browser work.

      ⚠️ **The plumbing itself WORKS and is not what this item is about.**
      `clock_time_get` is implemented (`web/src/wasi/preview1.ts:128`), nginx logs
      correct UTC timestamps in a tab, sleeps idle and resume, and 60 s epoll
      timeouts behave. `clock_res_get` is never imported, so there is nothing to
      add there. The defect is the clock BASE, not the wiring.

      ⚠️ **Two clock bases are already in play, and that part is handled.** The
      shim answers its own `CLOCK_MONOTONIC` from `performance.now()` (genuinely
      monotonic) while the runtime derives every deadline from REALTIME. The
      shim's `poll_oneoff` compares REMAINING time rather than raw deadlines and
      says why in a comment, so this is an inconsistency to know about rather
      than a live bug. Do not "fix" it by making them share a base without
      reading that code first.

      A fix means giving the runtime a separate monotonic source and measuring
      deadlines against it, while `clock_gettime(CLOCK_REALTIME)` keeps reporting
      wall time. ⚠️ **It needs a test that STEPS THE CLOCK**, or it certifies
      nothing: every existing timer test runs on a clock that never jumps, and
      would pass equally before and after. Nothing covers this today.
      — *source: the browser port's "honest limits", re-verified 2026-08-21*

- [x] **Three of the browser plan's "honest limits" were never recorded
      anywhere.** ✅ **DONE 2026-08-21.** All three are now in `README.md` under
      `## Status` -> `### Honest limits`, each naming the code that makes it true
      so a reader can check rather than trust: the xorshift64* `getrandom`
      (`context.rs` `rng_state`/`rand_bytes`), `sendmsg`/`recvmsg` ENOSYS on a
      host socket (`sys.rs` `sys_sendmsg`/`sys_recvmsg`), and the compute-bound
      freeze. The fourth row, wall-clock deadlines, is recorded there as FIXED
      rather than as a limit.

      ⚠️ One correction to the table below, found while writing it: the
      compute-bound freeze was NOT recorded "nowhere". `README.md`'s scheduler
      section already said "a guest that computes forever holds the module
      forever, and that is deliberate". What was genuinely missing is the
      per-host CONSEQUENCE -- under wasmedge it holds the module, in a browser it
      freezes the page thread, i.e. the tab -- which is what makes it a limit
      someone deploying would want stated. The new entry says that explicitly.
      Original text below.

- [x] **(original framing) Three of the browser plan's "honest limits" were never recorded
      anywhere.** The plan has a section titled *Honest limits to record in the
      README*. Checked 2026-08-21 — the root `README.md` mentions none of these:

      | Limit | Where it is true | Recorded |
      | --- | --- | --- |
      | ~~Deadlines are wall-clock~~ | ~~`context.rs:327`~~ | **NO LONGER TRUE — fixed 2026-08-21, see the entry above** |
      | Guest `getrandom` / `/dev/urandom` is a NON-cryptographic xorshift64* | `context.rs:1191`, `:1375-1389` | nowhere |
      | `sendmsg`/`recvmsg` on a host socket return `ENOSYS` | `sys.rs:4170`, `:4266` | nowhere |
      | A compute-bound guest with no syscalls freezes its thread | README's own "no interrupt to build preemption on" | nowhere |

      All four were verified in the tree on 2026-08-21, so these are facts about
      the code and not stale intentions. The first has since been FIXED rather
      than recorded, which is the better outcome and leaves three.

      ⚠️ **The getrandom one is the sharp one.** Any guest doing its own crypto —
      a TLS handshake, a session key, a password salt — draws from a deterministic
      xorshift64*. It is independent of the browser work and predates it; it just
      has never been written down where someone deploying this would see it.

      ⚠️ `sendmsg`/`recvmsg` matter more than they look: curl and some Go and Rust
      runtimes use them on ordinary TCP sockets, so this is not an exotic corner.
      — *source: browser port plan, "Honest limits to record in the README"*

## Found while measuring the clock, 2026-08-21

- [x] **The Node host's `netv1` backend spins on a real LISTENING socket.**
      ✅ **FIXED 2026-08-21.** ⚠️ **AND THE DIAGNOSIS BELOW IS WRONG** -- kept
      rather than rewritten, because being wrong in a specific, checkable way is
      what made it findable. It is not `accept4` returning EAGAIN against a
      falsely-ready listener. `listen(2)` itself FAILED: nginx calls it twice
      (`ngx_configure_listening_sockets` re-listens to apply the backlog, which
      Linux allows), the backend created a second `net.createServer()` that could
      not bind, overwrote the working server with the failed one and set a sticky
      socket error -- and an errored socket reports as permanently readable.
      Fixed in `opListen` (idempotent, and a failed attempt is dropped so a retry
      still works), plus `errno_of` no longer collapsing the bind family into
      EIO, which is why the guest saw `(5: I/O error)` instead of the address
      being in use. Guarded by `TestNginxServesRealSocketsUnderTheNodeHost`,
      `web/src/node/sockets.test.ts` and the `errno_of` unit test; all
      neutralized. Original text below.

      `node bin/run.ts --module public/nginx.wasm --rootfs public/nginx.img
      --reentrant --net-v1` boots nginx, which binds and listens, and then loops:
      `epoll_pwait` reports the listener readable, `accept4` returns EAGAIN,
      nginx logs the failure, repeat. Measured **572 318 iterations in 90 s**
      (`epoll_pwait` 572 318 / `accept4` 572 316 / `write` 572 323).

      ⚠️ **This is a real defect, not merely a bad benchmark.** It is the
      PostgreSQL `ServerLoop` shape the WasmEdge backend's `ready()` has a long
      comment about -- a listener that looks perpetually readable -- reappearing
      in a different backend. Nothing depends on it today because the browser
      profile serves inbound through the service worker rather than a socket, so
      `NodeSockets` has never had to report listener readiness correctly.

      ⚠️ It has already cost something: this loop produced the
      "`clock_gettime` is 1 144 644 of ~3.4M syscalls" figure that framed both the
      vDSO and cached-clock investigations. A serving nginx makes **6 clock reads
      per request**. Fixing it also gives the tree a REAL nginx-over-real-sockets
      harness under Node, which is a much better benchmark target than driving
      Chromium.
      — *source: `2026-08-21 -- CORRECTION: the cached clock, built, measured, and rejected`*

- [x] **`TestClockBenchIsolatesTheHostClockRead` cannot measure a clock that
      advances per READ.** ✅ **FIXED 2026-08-21** (host-side timing added; the
      bench itself has NOT been re-run, see below). It times both loops with
      `clock_gettime`, so a control loop of 200 000 `getpid` calls bracketed by
      two clock reads reports **0 ns/call** under any per-read clock. Fine
      against the shipping runtime and against a rate error; wrong against the
      next clock experiment. If one happens, time it from the HOST --
      `bin/run.ts` already knows how, that is what `HOST-AFTER-STEP-MS` is.
      — *source: same entry*

      The fix is `--stamp PREFIX` in `web/bin/run.ts` plus `web/src/stamp.ts`:
      the guest prints `BENCH-MARK <NAME>` and flushes, `fd_write` is a
      synchronous import, so the host reads `performance.now()` at the instant
      the guest got there and emits `HOST-STAMP-<NAME>-US: <n>`. Both loops are
      now bracketed by markers and the test reports the HOST numbers; the
      in-guest pair is kept beside them as a cross-check, because the
      disagreement is the diagnosis. `hostNsPerCall` refuses a zero span rather
      than reporting 0 ns/call. The test is still an INSTRUMENT -- no threshold
      was added.

      ⚠️ **Not verified against a lifted guest.** The differential was taken
      with a hand-written `wasm32-wasip1` probe under `bin/run.ts` (a cached
      clock that never advances): `BENCH-GETPID-NS 0` from inside, 26 936 us /
      200 000 = 134 ns/call from the host stamps, same run. That capture is
      pinned by `TestHostStampLinesParseAsClockValues` (no Docker, no node).
      `RAPTORMARK_CLOCK_BENCH=1` itself was never run -- it needs a builder
      image tag nobody supplied.

## Browser host and re-entrant runtime follow-ups, 2026-08-21

- [x] **Model default signal actions.** SIGKILL, SIGSTOP, and unhandled terminating signals do not perform their default action even though posting reports success.
      — *source: `2026-08-21 -- Worker restart: giving a guest in a tab an outside`*

      ✅ **RULED ON AND PARTLY IMPLEMENTED 2026-08-21**, entirely in
      `runtime/src/context.rs`. Term and Core terminate the thread group with
      status `128 + signo`, reusing the `Pending::Exit` teardown `exit_group`
      already performs; SIGKILL is honoured unconditionally (not blockable, and
      its recorded disposition is refused where it is consumed, since
      `rt_sigaction` still accepts one); Ign is deliberately left PENDING
      because `fd_ready` reports a signalfd readable out of `signals.pending`
      and PostgreSQL's latch is a signalfd over a SIG_DFL signal.
      `terminating_signal` is asked in three places: at a delivery boundary, in
      `retire_after_suspend`'s `Pending::Block` arm (a task that parks after
      taking a fatal signal would otherwise never die), and in `pick_next` right
      after `load_current` (the kill point every about-to-run task passes).
      ⚠️ **Stop is NOT implemented and that is the ruling, not an omission** --
      see the journal entry for why faking it would be worse than the nothing we
      do now. +22 host tests, 197 total; nine neutralizations recorded.
      — *see `2026-08-21 -- Default signal actions: a ruling, and the three places that act on it`*

- [x] **Report a signal death as one in `wait4`.** ~~A process killed by a signal
      is now a `Zombie(128 + signo)`, so `sys_wait4` builds
      `(code & 0xff) << 8` and the parent sees `WIFEXITED` with status
      `128 + signo` rather than `WIFSIGNALED` with the signal number.~~
      DONE 2026-08-22. `ProcStatus::Zombie` and `Pending::Exit` now carry an
      `ExitReason` (`Exited(code)` / `Killed(signo)`), `reap_zombie` returns it,
      and `sys_wait4` writes `ExitReason::wait_status()`. The shell's
      `128 + signo` rendering survives only where it belongs -- `status_code()`,
      the MODULE's exit code when init dies -- so init killed by SIGTERM still
      exits 143. ⚠️ Guest-visible change: a parent that read `WEXITSTATUS ==
      128 + signo` now reads `WIFSIGNALED` / `WTERMSIG`. Confirmed against a
      native baseline in `e2e/waitstatus_test.go`, whose phase 3 is the
      discriminator no renumbering can satisfy: `_exit(143)` and a SIGTERM death
      produced the SAME `0x8f00` under the old encoding.
      — *see `2026-08-22 -- A signal death is a signal death in wait4`*

- [x] **`rt_sigaction` and `rt_sigprocmask` accept SIGKILL and SIGSTOP.** Linux
      answers EINVAL for the first and silently drops both from the mask in the
      second. The *behaviour* is already correct -- `context.rs` refuses the
      recorded disposition where it is consumed -- so this is the error code and
      the `oldact` round-trip only. Both functions live in `sys.rs`.
      — *source: same entry*

      ✅ **DONE 2026-08-21.** Both rulings are now pure functions in
      `runtime/src/context.rs` -- `sigaction_permitted(signum, installing)` and
      `sigprocmask_next(how, prev, newset)` -- because `lib.rs` gates `mod sys`
      to `wasm32` and a `#[test]` written beside either syscall would never run.
      Same precedent as `arena::madvise_zeroes`. The consumption-site refusals
      in `deliver_pending_signals` and `terminating_signal` are UNCHANGED and
      remain the actual safety net; this only stops the syscalls lying about it.

      The correction the original entry did not carry: the EINVAL is conditional
      on `act`. Linux's `do_sigaction` tests `act && sig_kernel_only(sig)`, so
      `sigaction(SIGKILL, NULL, &old)` SUCCEEDS and reports SIG_DFL. Refusing
      the query too would break the `for (sig = 1; sig <= NSIG; sig++)`
      disposition sweep programs use at startup.

      Two orderings moved with it, both matching the kernel's copy-out-on-success
      wrappers: a refused `rt_sigaction` leaves `oldact` untouched, and a bad
      `how` leaves `oldset` untouched. Previously both wrote the old value first
      and then returned EINVAL.

      Still open, same class, both harmless today because the consumption-site
      net catches them: `rt_sigsuspend` installs its mask verbatim, and handler
      delivery does `blocked |= act.mask` -- Linux drops SIGKILL/SIGSTOP in both
      places (`sigdelsetmask` in `sigsuspend`, and in `signal_delivered`). What
      it costs is the value a handler would read back from `rt_sigprocmask`
      while it runs.

- [ ] **Run the browser suite under Firefox and WebKit.** Only Chromium has been measured; module-service-worker, storage, and lifecycle behavior remain unverified elsewhere.

      ⚠️ **ATTEMPTED 2026-08-21, and the blocker is PROVISIONING, not code.**
      `playwright.config.ts` already supports both engines via
      `RAPTORMARK_BROWSERS=firefox,webkit` -- no edit is needed, which is what
      that config's comment promises. State on this machine:

      - **WebKit is DOWNLOADED but cannot launch.** `~/.cache/ms-playwright`
        holds `webkit-2336`, and every webkit spec fails at **0-1 ms** with
        `Host system is missing dependencies to run browsers` (it names
        `libavif16`, `gstreamer1.0-libav`). The fix is
        `sudo npx playwright install-deps webkit` -- **root**, so it was not run.
      - **Firefox is not installed** (`~/.cache/ms-playwright` has chromium,
        chromium_headless_shell, ffmpeg, webkit). ~90 MB download; deliberately
        not pulled.

      ✅ **Chromium is now GREEN and that is new**: all 14 specs pass -- boot,
      detail, cache, inbound, keepalive, relay, swrestart, and nginx
      serve/concurrent/files/workers/reload/restart. Two things had to be fixed
      first, both recorded in the journal: a STALE `web/dist/raptormark.js`, and
      `npx` not being on the non-interactive `PATH`.

      ❌ Do NOT pass `RAPTORMARK_BROWSERS=webkit` on this machine until the deps
      are installed -- it turns a 121/0/7 run into 110/11/7, and all eleven
      failures are the same missing-libraries message rather than anything about
      raptormark.
      — *source: `2026-08-21 -- The service worker in TypeScript, then merged into one entrypoint`*

- [x] **Move compute-bound browser guests off the page thread or document the limitation canonically.** A guest leg with no scheduling boundary freezes its hosting thread.
      — *source: `2026-08-20 — A translated guest in a browser tab (M5, part 3)`*

      ✅ **CLOSED 2026-08-21 on the SECOND branch only, and that distinction is
      the point.** The limitation is now recorded canonically in `README.md`
      under `## Status` -> `### Honest limits`, with the per-host consequence
      spelled out -- under wasmedge it holds the module, in a browser it freezes
      the page thread, i.e. the tab -- and tied back to the scheduler section
      that explains why (nothing is time-sliced; there is no interrupt to build
      preemption on).

      ⚠️ **Nothing was moved off the page thread.** The entry was written as an
      either/or and the cheap branch was taken deliberately, so a
      compute-bound browser guest still freezes its tab exactly as before. If
      the ENGINEERING option is wanted -- a worker, or a scheduling boundary the
      guest cannot avoid -- that is a new entry and a real design question, not
      a leftover of this one.

- [ ] **Add browser transport capabilities only when a workload requires them.** Current limits are no connection reuse, response streaming, relay UDP, inbound relay listener, or demonstrated guest TLS path.
      — *source: browser networking entries from 2026-08-20 through 2026-08-21*

- [ ] **Narrow `Service-Worker-Allowed: /` if `internal/serve` becomes general-purpose.** The broad scope fits a server dedicated to `web/`, not a reused server.
      — *source: `2026-08-21 -- The bundle back under dist/, and the header that makes it possible`*

- [x] **Add focused unit coverage for `load.ts`, `netv1.ts`, `files.ts`, and `sockets.ts`.** All are reachable from fakes now that Vitest is present. Prioritize `load.ts`, which owns the compiled-module cache and has a silent failure mode.

      ✅ **CLOSED 2026-08-21 by re-verification, not by new work.** All four
      exist and the prioritized one is the largest: `web/src/browser/load.test.ts`
      (228 lines), `web/src/wasi/netv1.test.ts` (256),
      `web/src/wasi/sockets.test.ts` (188), `web/src/wasi/files.test.ts` (132),
      plus `web/src/node/sockets.test.ts` (129) which the `netv1` listener fix
      of the same day added. ⚠️ Counted, not audited -- this closes "there is no
      focused coverage", which is what the entry claimed, and does NOT certify
      that `load.ts`'s silent failure mode is among the cases covered.
      — *source: `2026-08-21 -- Session close, fourth amendment: the checks that were inexpressible`*
