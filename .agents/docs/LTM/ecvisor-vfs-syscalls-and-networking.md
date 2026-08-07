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
must update the shared object. Descriptor position is still incorrectly private
where Linux shares the open-file description across `dup` and `fork`; that
remaining gap is tracked in `TODO.md`.

Some non-fatal worker diagnostics remain: `io_setup` reports `ENOSYS`, `prctl(PR_SET_DUMPABLE)` reports `EINVAL`, and one musl worker has emitted `initgroups` `ENOENT`. They are tracked as follow-up rather than claimed as service blockers.

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
