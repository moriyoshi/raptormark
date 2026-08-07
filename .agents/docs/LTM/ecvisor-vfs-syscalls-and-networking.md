# Ecvisor VFS, Syscalls, and Networking

## Summary

Ecvisor provides the Linux-like syscall and filesystem surface required by fused userspace programs. Nginx bring-up established a concrete network-serving baseline on both musl and glibc and exposed the difference between printing a version and serving concurrent requests.

## Key Facts

- The rootfs is an RFS sidecar; ecvisor does not inherit a host filesystem.
- Missing syscalls must return deliberate Linux-compatible results or implementations, not accidental success.
- A remill intrinsic can resolve to a silent default stub because final linking permits undefined symbols.
- Nginx requires socket, polling, process, credential, file, and signal behavior beyond startup.
- Both single-process and four-worker nginx serve real HTTP requests on musl and glibc.

## Details

The initial ecvisor path had no filesystem provider because `internal/rootfs` was missing. Once restored, successive real-program runs revealed RELR, startup-table, dynamic-symbol, and syscall gaps. Socket guest binaries recovered from artifacts helped isolate the network surface before testing all of nginx.

Fourteen required syscalls were implemented together after tracing real nginx behavior. Further gaps between version output and service included descriptors, address handling, readiness polling, process operations, and worker lifecycle details. The final master/worker fixture demonstrated that all four workers can accept traffic rather than one worker handling every request.

The `brk` path illustrated a linker hazard: `--allow-undefined` plus remill defaults can turn a missing runtime intrinsic into a no-op. `llvm-nm` on the lifted object must show runtime-provided intrinsics as undefined (`U`), not locally defined (`T`).

### Timed waits and non-blocking descriptors

Blocking behavior follows the guest descriptor, not the non-blocking host socket
that ecvisor uses internally. `accept4(..., SOCK_NONBLOCK)` must preserve that
flag, and `recv`, `send`, and `accept` must return `EAGAIN` for a non-blocking
guest rather than parking the process. A blocking descriptor may suspend and
resume on readiness.

`epoll_pwait` takes a millisecond timeout, including negative for infinite. A
finite wait keeps one absolute deadline across spurious readiness wakes; rearming
the full relative timeout on every wake turned nginx's requested five seconds
into repeatable twenty-second waits. The idle scheduler polls host readiness and
the earliest deadline in the same operation so neither wake source starves the
other.

Host socket errors are mapped by operation. In particular, WASI `INPROGRESS`,
`AGAIN`, and `ALREADY` during `connect` are resumable states, not
`ECONNREFUSED`. Forwarded socket option numbers also require translation between
Linux and WasmEdge namespaces; passing Linux `SOL_SOCKET` values directly once
made `SO_REUSEADDR` silently inert.

### Guest-local sockets and shared files

Named AF_UNIX sockets are implemented inside ecvisor because both endpoints and
the rendezvous path live in the guest VFS. `bind` publishes a real
`NodeKind::Socket`; unlinking removes the name without destroying an active
listener. Connected endpoints reuse `SocketPair`, and abstract names are bounded
by `addrlen`, not a terminating NUL. PostgreSQL additionally required aarch64
`ppoll` and `send`/`recv` dispatch that recognizes guest-local endpoints rather
than only host sockets.

Regular-file contents are shared by path through the context-global
`open_files` table. Refcounts follow `fork`, `dup`, and `close`, and the last
close flushes to the tmpfs upper layer. `O_TRUNC`, path truncation, and rename
must update the shared object.

The descriptor POSITION is shared too, and an earlier version of this paragraph
said it was not. ⚠️ That claim was stale in both directions: it described a gap
the tree had already closed, and it deferred to `TODO.md`, which carries no such
item -- so a reader chasing it would have found the work in neither place.
`FdKind::Mem.off` indexes the context-global `file_offsets` table, which IS the
open file description, so `dup` and `fork` share one offset the way Linux does
by sharing one `struct file`, while a second `open` of the same path joins the
same `file` and takes a NEW `off`. It was previously a `pos: usize` held per
descriptor, which made `dup2(fd, 5)` re-read the same bytes from both and left a
shell's forked `read` child not advancing the parent's offset
(`runtime/src/context.rs`, the `Mem` variant, whose doc comment carries the
reasoning).

⚠️ Not every `pos` is that bug: `FdKind::Dir.pos` is a getdents64 read position
and is legitimately per-descriptor.

The three non-fatal nginx worker diagnostics are all RULED, and each ruling sits
at its arm in `runtime/src/sys.rs` rather than in a comment elsewhere.
⚠️ An earlier version of this paragraph listed them as open follow-up and was
stale; it contradicted the `prctl` statement further down this same file.

- **`io_setup` -> `ENOSYS`, deliberately.** It is Linux AIO, nginx logs a notice
  and falls back to blocking I/O, and `ENOSYS` is how it must be told.
- **`setgroups` -> accepted and discarded**, `getgroups` reports an empty set.
  Supplementary groups are not modelled, so that is a truthful answer to "which
  supplementary groups am I in" given we never joined any. The `initgroups`
  `ENOENT` a musl worker once emitted is gone.
- **`prctl(PR_SET_DUMPABLE)` -> accepted and stored.** See the `prctl` update
  below.

The distinction worth carrying: a syscall reaching `_ => EINVAL` by CATCH-ALL is
a divergence from Linux by default, not a decision. Turning one into a ruling is
a guest-VISIBLE behaviour change and wants to be deliberate.

## Files

- `runtime/src/sys.rs`: Syscall implementations.
- `runtime/src/vfs/`: Virtual filesystem and RFS reader.
- `internal/rootfs/`: Sidecar producer.
- `e2e/net_test.go`: Host-network and nginx coverage.
- `e2e/timers_test.go`: Finite, zero, and readiness-driven epoll waits.
- `e2e/nonblock_test.go`: Guest `O_NONBLOCK` behavior.
- `e2e/uds_test.go`: Named and abstract AF_UNIX behavior.
- `e2e/sharedfile_test.go`: Cross-process shared file contents.

## Test Coverage

The env-gated E2E suite includes socket-level guests, single-process nginx, abrupt disconnects, and four-worker request distribution. Both Alpine/musl and Bookworm/glibc closures have served HTTP requests.

The timer and non-blocking guards were neutralized against pre-fix runtime
images. PostgreSQL's guest-side `psql` path exercises AF_UNIX, `ppoll`, and
guest-local send/recv together; the shared-file guard makes processes write
different data so fork cannot manufacture agreement.

## Pitfalls

- `wasmedge` does not inherit host environment variables; pass diagnostics explicitly.
- Track the actual background process rather than a wrapper that exits immediately.
- A clean link does not prove the intended intrinsic implementation was selected.
- A host non-blocking socket does not imply a non-blocking guest descriptor.
- Do not reset a relative timeout after a spurious wake; retain its absolute deadline.
- Do not collapse resumable socket states into a permanent Linux errno.

## Consolidated Update: Memory-Related Syscalls

The former `ENOSYS` cases have distinct contracts: `membarrier` succeeds under the single-thread-at-a-time scheduler; `sysinfo` reports linear-memory capacity; `madvise` zeroes `MADV_DONTNEED` and `MADV_REMOVE` ranges but treats ordinary advice as a no-op; and `mremap` shrinks in place or moves growth under `MREMAP_MAYMOVE`, never guessing that adjacent bump-arena space is free. `e2e/mmadvise_test.go` compares with native behavior and checks contents, not only status.

## Consolidated Update: Host-Neutral Networking and Browser I/O

Socket handlers use compile-time-selected WasmEdge, loopback, or browser backends. Epoll waiters re-scan sets after readiness changes; single-socket operations wake only for their own readiness or can leak `EAGAIN`.

Browser DNS uses synthetic `240.0.0.0/4` addresses. HTTP inbound uses real request bytes and proper framing. Repeated `listen(2)` updates backlog and bind errors retain specific errno values. See `web-embedder-and-browser-networking.md`.

## Consolidated Update: `mmap`, `wait4`, `prctl`, and Shared Files

`mmap` rejects a zero length with `EINVAL` and rejects overflow while rounding to the 64 KiB guest page with `ENOMEM`. Every overflowing length rounds to exactly zero, so the pre-rounding zero check and checked alignment are independent. The pure decision lives in `arena::mmap_round_len` because `sys.rs` is wasm32-only and cannot be exercised by host tests.

Process exit state distinguishes `ExitReason::Exited(i32)` from
`ExitReason::Killed(u32)`, which carries the SIGNAL NUMBER and never
`128 + signo`. ⚠️ An earlier version of this sentence called them
`ExitReason::Code` and `ExitReason::Signal`; neither name has ever existed, so
grepping for one finds nothing and reads as the feature being absent.

The two renderings are separate accessors and are NOT interchangeable, which is
the bug the type was introduced to fix: `wait_status()` packs a word for
`waitpid`'s macros (`(code & 0xff) << 8` for an exit, `sig & 0x7f` for a signal
death), while `status_code()` is the single small integer a shell or `$?` sees,
and only that one uses `128 + signo` -- so init killed by SIGTERM still exits
143. This prevents `_exit(143)` and a SIGTERM death from becoming bit-identical,
which is exactly what the old encoding produced (`0x8f00` either way).
⚠️ Guest-visible change: a parent that read `WEXITSTATUS == 128 + signo` now
reads `WIFSIGNALED` / `WTERMSIG`. `PR_SET_DUMPABLE`, `PR_SET_THP_DISABLE`, and `PR_SET_VMA_ANON_NAME` implement the Linux behavior reached by PostgreSQL and Ruby, including range validation and overlap-based anonymous VMA naming.

File-backed `MAP_SHARED` reclamation follows writability, not pathname. A read-only region cannot contain writes that need preservation and is reclaimable even while its backing path exists. A region ever mapped or protected writable retains its shared bytes while the name exists; unlinking makes it reclaimable. `ShmFile.writable` is monotonic and is updated by mapping reuse and overlapping `mprotect`.

## Consolidated Update: The Guest Clock Is Monotonic

`CLOCK_MONOTONIC` was served from the WALL clock, so it was not monotonic. Both
clocks came from one `now_nanos()` built on `SystemTime::now()`, and the doc
comment on it said so. A guest timer therefore followed NTP steps, a laptop
suspend, and -- in a browser -- a throttled or backgrounded tab.

Fixed with a separate monotonic base: `mono_nanos()` and `to_mono()` in
`runtime/src/context.rs`, every deadline in `sys.rs` and `context.rs` moved onto
it, `clock_gettime` reading its `clockid` through `clock_base()` instead of
ignoring it, and `clock_getres` (114) added.
`clock_gettime(CLOCK_REALTIME)` still reports wall time.

`ecv_next_deadline_ms` became **`ecv_next_deadline_in_ms` and returns a DELAY**,
not an instant, because a monotonic instant means nothing next to the host's
`Date.now()`. That rename is part of the re-entrant host surface; see
`web-embedder-and-browser-networking.md`.

⚠️ **A timer fix needs a test that STEPS THE CLOCK, or it certifies nothing.**
Every existing timer test ran on a clock that never jumps and would have passed
equally before and after. `TestGuestTimersSurviveAWallClockStep`
(`e2e/clock_test.go`) steps the host's wall clock by an hour at the first idle;
neutralized, and all three claim assertions fired while both harness checks
stayed green.

⚠️ **Two clock bases are in play in the browser and that part is deliberate.**
The web shim answers its own `CLOCK_MONOTONIC` from `performance.now()` while
the runtime derives deadlines from its own base, and the shim's `poll_oneoff`
compares REMAINING time rather than raw deadlines and says why in a comment. Do
not "fix" that by making them share a base without reading that code first.
