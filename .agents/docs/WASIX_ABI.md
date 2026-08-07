# The WASIX ABI, as measured

Ground truth for wasmer **7.3.0**, gathered 2026-08-24 from the shipped binary
and from `github.com/wasmerio/wasmer` at tag `v7.3.0`. Written down because
recovering it cost a dozen fetches and several wrong turns, and because two
mistakes this tree made in one day came from INFERRING an ABI instead of reading
it.

⚠️ **Arity does not give you order.** `wasmer` will tell you a signature if you
import it with the wrong type -- the error names the type it provides -- but the
type is a list of `i32`s and says nothing about which parameter is which. That
is exactly how `dlopen` was first written here with `err_buf` and
`ld_library_path` swapped, sending every diagnostic to address 0.

⚠️ **The error wording is inverted from the intuitive reading**: "Expected" is
what YOUR module declared, "but received" is what wasmer provides.

```
$ wasmer run probe.wat
incompatible import type.
Expected  Function(FunctionType { params: [I32],      results: [I32] })  <- mine
but received Function(FunctionType { params: [I32 x8], results: [I32] }) <- wasmer's
```

## Namespace

`wasix_32v1` (and `wasix_64v1` for the 64-bit memory model). **140 exports.**
Ordinary WASI preview1 stays in `wasi_snapshot_preview1`.

## Dynamic linking

| import | signature |
|---|---|
| `dlopen` | `(path, path_len, flags, err_buf, err_buf_len, ld_library_path, ld_library_path_len, out_handle) -> errno` |
| `dlsym` | `(handle, name, name_len, err_buf, err_buf_len, out_ptr) -> errno` |

`dlclose` and `dlerror` are **not** imports; errors come back in `err_buf`.

A `dlopen` call from an instance the runtime does not consider dynamically
linked returns errno **79**. ⚠️ 79 is `unknown`, the LAST variant of the errno
enum and a catch-all -- `dlsym` with a bogus handle returns it too. It is not a
diagnosis. The real message is in `err_buf`, which is empty only if you passed
the buffer in the wrong position.

What makes an instance dynamically linked: the MAIN module carries a `dylink.0`
custom section. wasmer then requires it to import `env.memory` (shared,
129..65536 pages), `env.__indirect_function_table`, `env.__stack_pointer`,
`env.__memory_base`, `env.__table_base`, and to export `__tls_base`.

⚠️ Every `env` import must be a FUNCTION. wasmer supplies the three standard PIC
globals itself and refuses any other imported global -- which is why raptormark
needs elfconv patch 0067 to stop lifting `env.__ecv_unwinding`.

## Sockets

40 socket and port functions. The ones a `NetBackend` needs:

| import | signature |
|---|---|
| `sock_open` | `(af, ty, proto, ret_fd) -> errno` |
| `sock_bind` | `(fd, addr_ptr) -> errno` |
| `sock_listen` | `(fd, backlog) -> errno` |
| `sock_connect` | `(fd, addr_ptr) -> errno` |
| `sock_recv` | `(fd, ri_data, ri_data_len, ri_flags, ro_data_len, ro_flags) -> errno` |
| `sock_send` | `(fd, si_data, si_data_len, si_flags, ret_data_len) -> errno` |
| `sock_recv_from` | `(fd, ri_data, ri_data_len, ri_flags, ro_data_len, ro_flags, ro_addr) -> errno` |
| `sock_send_to` | `(fd, si_data, si_data_len, si_flags, addr_ptr, ret_data_len) -> errno` |
| `sock_addr_local` | `(fd, ret_addr) -> errno` |
| `sock_addr_peer` | `(fd, ret_addr) -> errno` |

Also present: `sock_accept`, `sock_accept_v2`, `sock_shutdown`, `sock_status`,
`sock_pair`, `sock_send_file`, `sock_get_opt_flag` / `_size` / `_time`,
`sock_set_opt_flag` / `_size` / `_time`, `sock_join_multicast_v4` / `_v6`,
`sock_leave_multicast_v4` / `_v6`, `resolve` (DNS), and thirteen `port_*` calls
for interface, route and DHCP configuration.

⚠️ **`resolve` means a WASIX backend need not answer DNS in-runtime**, unlike
`net::loopback` and `net::browser` which both do.

Sockets are ordinary WASI file descriptors, so `fd_close` closes one and
`poll_oneoff` is how readiness is observed.

### The address encoding

From `lib/wasix/src/net/mod.rs::read_ip_port`, which is the authority:

```
__wasi_addr_port_t {
    tag:  u8      // Addressfamily
    octs: [u8]    // immediately after the tag
}

Inet4:  octs[0..2] = port (u16, native endian = little)   octs[2..6]  = IPv4 bytes
Inet6:  octs[0..2] = port                                  octs[2..18] = IPv6 bytes
```

⚠️ **The port comes FIRST, before the address bytes.** That is the opposite of
`sockaddr_in`, where the family precedes the port precedes the address. A codec
written from POSIX habit will bind to a plausible-looking wrong endpoint rather
than fail.

❗ **CORRECTION, measured 2026-08-24 -- the block above is wrong twice.** `octs`
is NOT immediately after the tag, and the two directions do NOT share an
endianness. See "The address encoding, measured" below, which supersedes this.
The reading above is what `read_ip_port` alone says; it is half the ABI.

Enum values, from `lib/wasi-types/src/wasi/bindings.rs`:

```
Addressfamily:  Unspec=0  Inet4=1  Inet6=2  Unix=3
Socktype:       Unknown=0 Stream=1 Dgram=2  Raw=3  Seqpacket=4
```

---

# The socket half, measured 2026-08-24

Everything below was measured against wasmer 7.3.0 for the `net-wasix`
backend, by the two techniques this document prescribes: a deliberately wrong
import type for the arity, and the syscall's own source at tag `v7.3.0` for the
order. Where a claim could be settled by RUNNING something, it was, and the
probe is named.

## ❗ `--net` is required, and its absence is not a link error

Without it `sock_open` **succeeds** and `sock_bind` returns errno **58**
(`NOTSUP`). A socket you can create and cannot bind, from a run that reports no
error until the first bind.

```
$ wasmer run       socknet.wat   ->  open=0 fd=6 bind=58 listen=28(INVAL)
$ wasmer run --net socknet.wat   ->  open=0 fd=6 bind=0  listen=0
```

## The signatures, as wasm sees them

`.agents-workspace/wasmer/socksigs.sh` measures these; the parameter NAMES come
from `lib/wasix/src/syscalls/wasix/<name>.rs`, because arity does not give order.

| import | wasm type | parameters |
|---|---|---|
| `sock_open` | `(i32 x4) -> i32` | `af, ty, proto, ro_sock` |
| `sock_bind` | `(i32 x2) -> i32` | `sock, addr` |
| `sock_listen` | `(i32 x2) -> i32` | `sock, backlog` |
| `sock_connect` | `(i32 x2) -> i32` | `sock, addr` |
| `sock_accept` | `(i32 x4) -> i32` | `sock, fd_flags, ro_fd, ro_addr` |
| `sock_accept_v2` | `(i32 x4) -> i32` | identical -- see below |
| `sock_recv` | `(i32 x6) -> i32` | `sock, ri_data, ri_data_len, ri_flags, ro_data_len, ro_flags` |
| `sock_send` | `(i32 x5) -> i32` | `sock, si_data, si_data_len, si_flags, ret_data_len` |
| `sock_recv_from` | `(i32 x7) -> i32` | `... ro_flags, ro_addr` |
| `sock_send_to` | `(i32 x6) -> i32` | `sock, si_data, si_data_len, si_flags, addr, ret_data_len` |
| `sock_addr_local` | `(i32 x2) -> i32` | `sock, ret_addr` |
| `sock_addr_peer` | `(i32 x2) -> i32` | `sock, ro_addr` |
| `sock_shutdown` | `(i32 x2) -> i32` | `sock, how` |
| `sock_status` | `(i32 x2) -> i32` | `sock, ret_status` |
| `sock_set_opt_flag` | `(i32 x3) -> i32` | `sock, opt, flag` -- a VALUE, not a pointer |
| `sock_get_opt_flag` | `(i32 x3) -> i32` | `sock, opt, ret_flag` |
| `sock_set_opt_size` | `(i32, i32, **i64**) -> i32` | `sock, opt, size` -- `Filesize` is u64 |
| `sock_get_opt_size` | `(i32 x3) -> i32` | `sock, opt, ret_size` |
| `sock_set_opt_time` | `(i32 x3) -> i32` | `sock, opt, time` -- pointer to `OptionTimestamp` |
| `sock_get_opt_time` | `(i32 x3) -> i32` | `sock, opt, ret_time` |

⚠️ **`sock_set_opt_size` is the only one taking an i64.** Declaring it as three
i32s is an instantiation failure, which is at least loud; the reverse mistake --
declaring a pointer-taking call as a value-taking one -- is not.

### ❗ `wasix_32v1.sock_accept` IS `sock_accept_v2`

`lib/wasix/src/lib.rs` binds the name twice, and not to the same function:

```
502:  "sock_accept"    => sock_accept::<Memory32>       // wasi_snapshot_preview1
646:  "sock_accept"    => sock_accept_v2::<Memory32>    // wasix_32v1
647:  "sock_accept_v2" => sock_accept_v2::<Memory32>    // wasix_32v1
```

So the preview1 spelling takes three parameters and the WASIX spelling takes
four, and the fourth returns the peer address. Reading the 3-parameter
`pub fn sock_accept` in `sock_accept.rs` and declaring it that way is a mistake
the arity check does catch -- but only if you declare it before you assume it.

## The address encoding, measured

This supersedes the "octs immediately after the tag" reading above.
`lib/wasi-types/src/types.rs`:

```
__wasi_addr_port_t {           // 20 bytes
    tag:      Addressfamily,   // u8   @0
    _padding: u8,              //      @1   "C will add a padding byte here
                               //            which must be set to zero
                               //            otherwise the tag will corrupt"
    u.octs:   [u8; 18],        //      @2
}
```

**Absolute offsets: tag @0, padding @1, port @2..4, address @4..8 (v4) or
@4..20 (v6).** The one-byte `_padding` is the whole difference between binding
where you meant to and binding somewhere plausible.

### ⚠️ THE PORT'S ENDIANNESS IS ASYMMETRIC, AND THIS IS THE TRAP

| direction | function | port |
|---|---|---|
| we WRITE (`sock_bind`, `sock_connect`, `sock_send_to`) | `read_ip_port` -> `u16::from_ne_bytes` | **little**-endian |
| we READ (`sock_addr_local`, `sock_addr_peer`, `sock_recv_from`, `sock_accept`) | `write_ip_port` -> `port.to_be_bytes()` | **big**-endian |

Measured rather than inferred, because "the source says so" and "the shipped
binary does so" are different claims. `.agents-workspace/wasmer/socknet.wat`
binds to port 8080 = `0x1f90` with the bytes written LITTLE-endian, then reads
the assignment back:

```
wrote  ... 01 00 90 1f 7f 00 00 01 ...     tag=Inet4, port=90 1f, 127.0.0.1
read   ... 01 00 1f 90 7f 00 00 01 ...     port comes back BYTE-SWAPPED
```

Both bytes of 8080 differ, so this cannot be a coincidence in either direction:
the bind landed on 8080 (the readback names it) having been given `90 1f`, and
the readback spells the same number `1f 90`. A codec that byte-swaps
symmetrically is wrong in exactly one direction, and the direction it is wrong
in still produces a working-looking bind.

## ❗ `poll_oneoff`: a clock timeout of ZERO means WAIT FOREVER

`lib/wasix/src/syscalls/wasi/poll_oneoff.rs`:

```rust
if clock_info.timeout == 0 {
    time_to_sleep = Duration::MAX;            // and the sub is NOT recorded
} else if clock_info.timeout == 1 {
    time_to_sleep = Duration::ZERO;           // THIS is the immediate probe
    clock_subs.push(...);
} else { /* the actual duration, absolute if SUBSCRIPTION_CLOCK_ABSTIME */ }
```

⚠️ **This inverts preview1 as `runtime/src/net/wasmedge.rs` uses it.** That
backend's `ready` builds a zero-timeout clock subscription precisely to get an
immediate return, and copying it would hang the guest on its first
`epoll_pwait` -- with no error, no import failure and nothing to grep for.

Measured, not read: `socknet.wat` (timeout 1) exits 0 with `nevents=1`;
`socknet0.wat`, identical but for the timeout, has to be killed.

```
timeout 12 wasmer run --net socknet0.wat   ->  rc=124
```

✅ **preview1's `poll_oneoff` DOES observe a `wasix_32v1` socket fd.** Both runs
above import it from `wasi_snapshot_preview1`, and both saw the listening
socket. So a WASIX backend needs `sock_*` from `wasix_32v1` and can keep taking
`poll_oneoff`, `fd_close` and `fd_fdstat_set_flags` from preview1, which is
where the rest of ecvisor already gets them.

`Subscription` (48 bytes) and `Event` (32 bytes) are the preview1 layouts
unchanged -- `userdata` u64 @0, `type` u8 @8, union @16; `Clockid` is u32.

## ✅ Non-blocking is `fd_fdstat_set_flags(fd, 4)`

It takes on a socket, and the socket paths consult it:
`sock_accept.rs:129` reads the LISTENER's flags, `sock_send.rs:102` and
`sock_connect.rs:72` read the socket's.

Measured with `.agents-workspace/wasmer/socknb.wat` -- bind, listen, set the
flag, then accept with nothing pending:

```
socknb.wat        ->  rc=0,   accept = errno 6 (AGAIN)
socknb_neut.wat   ->  rc=124  (same file, flags=0: it blocks forever)
```

The neutralization is the point. Without it, "accept returned 6" is a number in
a buffer; with it, the flag is what produced it.

## The socket options

`Sockoption` is `#[repr(u8)]` with implicit discriminants:

```
 0 Noop            7 MulticastLoopV4   14 OobInline        21 ConnectTimeout
 1 ReusePort       8 MulticastLoopV6   15 RecvBufSize      22 AcceptTimeout
 2 ReuseAddr       9 Promiscuous       16 SendBufSize      23 Ttl
 3 NoDelay        10 Listening         17 RecvLowat        24 MulticastTtlV4
 4 DontRoute      11 LastError         18 SendLowat        25 Type
 5 OnlyV6         12 KeepAlive         19 RecvTimeout      26 Proto
 6 Broadcast      13 Linger            20 SendTimeout
```

An option is not free to use any of the three setters. Which one it takes is
fixed, and the wrong one is `EINVAL`:

| setter | options it accepts |
|---|---|
| `_flag` | `OnlyV6`, `ReusePort`, `ReuseAddr`, `NoDelay`, `KeepAlive`, `DontRoute`; `Broadcast`, `MulticastLoopV4/V6` on UDP; `Promiscuous` on raw |
| `_size` | `RecvBufSize`, `SendBufSize`, `Ttl`, `MulticastTtlV4` -- and `LastError` on GET only |
| `_time` | `RecvTimeout`, `SendTimeout`, `ConnectTimeout`, `AcceptTimeout`, `Linger` |

✅ **`NoDelay` exists**, so `TCP_NODELAY` is expressible here. It is not under
WasmEdge, which has no TCP level at all -- see `net/wasmedge.rs:144`. A backend
that can express it must not inherit that limitation.

⚠️ **`sock_get_opt_size(LastError)` is `SO_ERROR`, and it answers in WASI
numbering** (`socket.last_error().map(|a| u16::from(a) as Filesize)`). Handing
that to a guest unchanged reports a WASI errno as a Linux one. libpq queries
`SO_ERROR` on every connection and believes the answer.

## Also relevant

* `SockProto` is `#[repr(u16)]` and follows the Linux `IPPROTO_*` numbering:
  `Ip=0`, `Tcp=6`, `Udp=17`. `sock_open` rejects `Tcp` with a non-`Stream` type
  and `Udp` with a non-`Dgram` one, so `0` is the argument that always works.
* `Socktype`: `Unknown=0 Stream=1 Dgram=2 Raw=3 Seqpacket=4`. ⚠️ Not WasmEdge's
  numbering, which has `Dgram=1 Stream=2`.
* `SdFlags` is `pub type SdFlags = u8` -- a bitmask, as under WasmEdge.
* `Bool` is `#[repr(u8)]`, `False=0 True=1`.

# The process half, measured 2026-08-24

WASIX has a fork. `wasix_32v1` exports `proc_fork`, `proc_fork_env`, four
`proc_exec*`, three `proc_spawn*`, `proc_join`, `proc_snapshot`, and
`stack_checkpoint` / `stack_restore`. The question this section answers is not
whether it exists -- it does, and it was measured WORKING -- but whether ecvisor
could use it.

**It works, and ecvisor still cannot use it.** Those are separate findings and
the second does not rest on the first.

## What `proc_fork` requires, measured by supplying each piece until it worked

`.agents-workspace/wasmer/fork.wat` began as a bare guest calling `proc_fork` and was
extended one requirement at a time. Each step produced a DIFFERENT failure,
which is the only reason the list is trustworthy -- a probe that fails the same
way throughout is measuring one thing and attributing it to another.

| the module has | outcome |
|---|---|
| nothing | exit **79** (`Unknown`) -- no exported `__stack_pointer` |
| + exported mutable `__stack_pointer` global | exit **45** (`Noexec`) -- `asyncify_start_unwind` export missing |
| + the five `asyncify_*` exports | `RuntimeError: The memory is not shared` |
| + a `shared` memory | exit **202**: `proc_fork` returned `Success`, child pid **2** |

So the entry requirements are exactly three: an exported mutable
`__stack_pointer`, the five Binaryen asyncify exports, and a **shared** linear
memory. Given all three, the call returns and a child process exists.

⚠️ The asyncify exports in the final probe are STUBS that only track a state
global. They sufficed because the probe's guest has nothing on its stack worth
unwinding. Do not read the exit 202 as "stub asyncify is enough for a real
guest" -- it is enough to prove the three requirements are the complete gate,
and nothing beyond that.

## ❗ Requirement 2 is one ecvisor deliberately removed

`proc_fork` does its work through `unwind::<M,_>` / `rewind::<M,_>`
(`lib/wasix/src/syscalls/mod.rs`), which call five exports on the guest
instance: `asyncify_start_unwind`, `asyncify_stop_unwind`,
`asyncify_start_rewind`, `asyncify_stop_rewind`, `asyncify_get_state`.

A raptormark module has **none of them**, and not by oversight.
`internal/builder/linkall.go` records that wasm-opt's `--asyncify` is *mutually
exclusive with elflift's `--fork_emulation`* -- an asyncified `call_indirect`
traps on return while the unwind state is set -- and that dropping the pass is
what removed the last non-2.0 proposal from the emitted module, which is what
lets a stock released runwasi shim run it. Measured on two real pipeline
outputs (`pipebuild2/app.wasm`, 40 MB; `warm-merged/bash.wasm`, 18 MB): zero
occurrences of any `asyncify_*` name.

⚠️ **The failure is a process death, not an errno.** With the export missing,
`unwind` logs at WARN and returns `Err(WasiError::Exit(Errno::Noexec))`. Nothing
is written to the guest's `pid_ptr` and `proc_fork` never returns, so a guest
cannot detect this and fall back:

```
WARN wasmer_wasix::syscalls: failed to unwind the stack because
     the asyncify_start_unwind export is missing
EXIT=45
```

45 is WASIX's `Noexec`, **not** Linux's `ENOEXEC` (8). And the WARN only appears
with `RUST_LOG` set -- the default is a bare exit 45.

⚠️ Requirement 1 fails at **79**, which is also the errno `--profile wasix`'s
`dlopen` returns. Unrelated; do not read one as evidence of the other.

## ⚠️ Requirement 3 is the threads proposal

A `shared` memory is the threads proposal. For the SHIPPING profile that is out
of bounds (`TestWasmOptEnablesNoProposal`), but `--profile wasix` targets wasmer
specifically and is explicitly allowed past Wasm 2.0 -- the same allowance
`load-wasix` needs. So this one is a cost, not a wall.

## ❗ `proc_fork` refuses dynamically linked modules outright

The second line of the function body:

```rust
wasi_try_ok!(ctx.data().ensure_static_module().map_err(|_| {
    warn!("process forking not supported for dynamically linked modules");
    Errno::Notsup
}));
```

`static_module_instance_handles()` returns `None` whenever the module was
loaded as a dynamic-linking group, "since dynamically linked modules are made
up of multiple instances" (`state/env.rs:936`). So `proc_fork` and `--side-out`
are mutually exclusive **in wasmer**, independently of anything raptormark does.

It also declines to fork from any non-main context under the context-switching
feature, and from inside an active vfork.

## ❗ The layers do not compose: it would fork the SUPERVISOR

**This is the wall.** Everything above is a cost -- exports to add, a proposal
to opt into, a link mode to avoid. This one does not go away by building
differently.

Ecvisor multiplexes **every** guest process and thread inside **one** Wasm
instance, over one linear memory, with a cooperative scheduler
(`.agents/docs/LTM/ecvisor-process-and-thread-model.md`). `proc_fork` forks a
Wasm *instance*: `memory.copy()` of the whole linear memory, a new store, a new
task, re-entered at `_start`.

So a guest `fork(2)` reaching `proc_fork` would duplicate the entire supervisor
-- every guest process in the arena, the fd table, the scheduler run queue, the
shared-segment registry -- and produce two supervisors each believing it owns
all of them. What the guest asked for was one more entry in `ctx.processes`.

`sys_clone` already does exactly that, and cheaply: `ctx.fork_current()`
snapshots the caller's bounded dirty set (measured median 2 MiB against a
384 MiB arena on PostgreSQL) and enqueues a replay-start process. `proc_fork`'s
unit is the 384 MiB.

## ✅ `proc_spawn` IS A MODULE LOADER, and it needs none of the three

Measured 2026-08-24 with `spawn.wat` + `child.wasm`: a WASIX guest **spawned a
second wasm module and ran it**.

```
CHILD-RAN
EXIT=202          # errno Success, child pid 2
```

`CHILD-RAN` is the CHILD's own stdout. That is the observation that matters: an
errno of Success proves the call was accepted, a line from the child proves a
second instance was created and executed guest code. The parent has **no
asyncify exports, no shared memory, and no `__stack_pointer` export** -- none of
`proc_fork`'s requirements apply, because `proc_spawn3_impl` never calls
`unwind`. It resolves the name against the guest's OWN filesystem
(`find_executable_in_path(&state.fs, ..)`, `proc_spawn3.rs:111`) and hands it to
`bin_factory.spawn`.

❗ **This falsifies `MULTIMODULE.md` §4's blocker, for wasmer.** That section
says "A wasm module cannot instantiate another wasm module. There is no engine
inside ecvisor, and WASI preview1 exposes no module-loading call." True of
preview1; **not true of WASIX**, and it needs no embedder and no CLI flag -- a
stock `wasmer run` is enough. See the amendment there.

⚠️ It is `posix_spawn`, NOT fork. The child starts at `_start` with a fresh
linear memory. Nothing of the parent's address space survives, so it cannot
implement `fork(2)` and cannot serve §2's program-modules design either, which
requires the program module to import the SUPERVISOR's memory.

`child.wasm` is built by `mkchild.py` -- hand-assembled, because there is no
wat2wasm on the host, in the builder image or in the wasmer image, and
`bin_factory` needs a real file rather than a `.wat`.

## ❗ WASIX HAS NO SHARED MEMORY BETWEEN PROCESSES. NONE.

Of the 139 functions in `wasix_32v1` there is **not one** `shm`, `mmap`, or
memory-sharing call:

```
grep -o '"[a-z_0-9]*" =>' lib.rs | grep -i 'shm\|mmap\|memory\|share\|map'   # empty
```

This is the fact that decides every multi-instance design. Two WASIX processes
share a filesystem, pipes and sockets; they cannot share a byte of memory. So a
process-per-instance ecvisor could not implement POSIX `shm`, `MAP_SHARED`, or
SysV segments -- which is to say it could not run PostgreSQL, whose
`shared_segments` model is in `.agents/docs/LTM/ecvisor-process-and-thread-model.md`.

The current design does not have this problem precisely because every guest
process lives in one linear memory.

## What this does NOT rule out

* **`stack_checkpoint` / `stack_restore`.** Also asyncify, same requirements.
* **A future ecvisor that is not the kernel.** The layering argument is about
  the current architecture, not about WASIX. If ecvisor ever became one
  supervisor per guest process, `proc_fork` forks exactly one guest process and
  becomes the RIGHT primitive -- the requirement list above is then a price, not
  an objection. What that architecture runs into instead is the section above:
  no shared memory, ever.

## Reproducing any of this

`.agents-workspace/wasmer/` has the container and every probe named above;
`raptormark-wasmer:7.3.0` is the image. ⚠️ It was moved there from
`.agents-workspace/tmp/wasmertest/`, which is the directory `CLAUDE.md` says is
wiped without warning -- and this harness is the only way to re-measure any of
the above.

```
docker run --rm -v "$PWD:/w" -w /w raptormark-wasmer:7.3.0 \
  bash -lc 'bash socksigs.sh'
docker run --rm -v "$PWD:/w" -w /w raptormark-wasmer:7.3.0 \
  bash -lc 'wasmer run --net socknet.wat > /tmp/o.bin; od -A d -t x1 /tmp/o.bin'
docker run --rm -v "$PWD:/w" -w /w raptormark-wasmer:7.3.0 \
  bash -lc 'RUST_LOG=warn wasmer run --net fork.wat; echo "EXIT=$?"'        # 45
docker run --rm -v "$PWD:/w" -w /w raptormark-wasmer:7.3.0 \
  bash -lc 'RUST_LOG=warn wasmer run --net fork_stub.wat; echo "EXIT=$?"'   # not shared
docker run --rm -v "$PWD:/w" -w /w raptormark-wasmer:7.3.0 \
  bash -lc 'RUST_LOG=warn wasmer run --net fork_shared.wat; echo "EXIT=$?"' # 202
python3 mkchild.py child.wasm            # on the HOST -- the image has no python
docker run --rm -v "$PWD:/w" raptormark-wasmer:7.3.0 \
  bash -lc 'cd /w && wasmer run --net --volume /w:/ spawn.wat; echo "EXIT=$?"'
                                                        # CHILD-RAN, then 202
```

⚠️ `spawn.wat` needs `--volume`, unlike every other probe here: the module it
spawns is looked up in the GUEST's filesystem, so without a preopen there is
nothing to find. wasmer warns that mounting on `/` "breaks WASIX modules'
filesystems"; harmless for a probe with one file, and worth remembering before
copying this line into anything real.

`fork_stub.wat` is `fork.wat` plus stub asyncify exports and exists as
`fork.wat`'s NEUTRALIZATION: without it, "proc_fork always fails for some
reason" and "proc_fork fails because asyncify is missing" are the same exit 45.
It moves the outcome to a different diagnostic, which is what makes fork.wat's
attribution stand. `fork_shared.wat` is that plus a shared memory, and is the
one that returns.

`fork.wat` exits `100 + errno` if `proc_fork` ever returns one, so any code
below 100 is wasmer killing the process instead. The offset exists because
wasmer's `WasiError::Exit(errno)` puts the errno in the exit code too, so
without it a returned `Noexec` and a killing `Noexec` would both read as 45.

The original throwaway harness is also still there. The container is debian-slim
plus `get.wasmer.io`; wasmer is deliberately not a build dependency and is on
neither the host nor the builder image.

To measure a signature: import the function with a deliberately wrong type and
read the "but received" half. To learn the ORDER, fetch the syscall's source --
`lib/wasix/src/syscalls/wasix/<name>.rs` -- because the order is not in the type.
