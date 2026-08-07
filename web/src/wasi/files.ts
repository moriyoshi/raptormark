import { E, FILETYPE } from './abi.ts';

/**
 * The file surface a raptormark module needs, which is far smaller than WASI's.
 *
 * ecvisor does not use the host filesystem: the guest rootfs is an in-memory
 * RAPTORFS image (`runtime/src/vfs/rfs.rs`) and every guest `openat`/`read`/
 * `getdents64` is served from it. The host is touched exactly ONCE, by
 * `load_sidecar()` (`runtime/src/entry.rs:320`), which does a `std::fs::read`
 * of `$RAPTORMARK_ROOTFS` -- falling back to `out.rootfs.img`, `rootfs.img`,
 * `/out.rootfs.img` -- before any guest instruction runs.
 *
 * That single read is the ONLY reason `path_open`, `fd_prestat_get`,
 * `fd_prestat_dir_name`, `fd_filestat_get` and `fd_close` appear in the
 * module's import list at all. So this class is a one-file server, not a
 * filesystem, and it should stay that way: anything richer is answering a
 * question nobody asked.
 *
 * Note what is NOT here. There is no `fd_seek` -- Rust's `std::fs::read` sizes
 * the buffer with `fd_filestat_get` and then reads forward -- and no
 * `fd_readdir`, `path_filestat_get`, or any write path. Confirmed against a
 * real module: `WebAssembly.Module.imports()` on a linked artifact lists 28
 * names and none of those.
 */

const STDIN = 0;
const STDOUT = 1;
const STDERR = 2;
/** The single preopened directory. WASI preopens conventionally start at 3. */
export const PREOPEN_FD = 3;

type OpenFile = { data: Uint8Array; pos: number };

export class Files {
  /** basename -> contents. Normally exactly one entry, the rootfs sidecar. */
  private readonly contents = new Map<string, Uint8Array>();
  private readonly open = new Map<number, OpenFile>();
  private nextFd = PREOPEN_FD + 1;

  /** The preopened directory's name, as reported by `fd_prestat_dir_name`. */
  readonly preopenName: string;

  /** Called with (fd, bytes) for every write to stdout/stderr. */
  onWrite: (fd: number, bytes: Uint8Array) => void = () => {};

  constructor(preopenName = '/') {
    this.preopenName = preopenName;
  }

  /** Publishes `bytes` under `name`, e.g. `add('rootfs.img', img)`. */
  add(name: string, bytes: Uint8Array): void {
    this.contents.set(name, bytes);
  }

  /**
   * Resolves a WASI path to a published file.
   *
   * Matching is by BASENAME, deliberately. The guest side asks for whatever
   * `load_sidecar` was configured with -- an absolute `/rootfs.img`, a bare
   * `rootfs.img`, or a `./`-prefixed form -- and Rust's std strips the preopen
   * prefix before the call, so the exact string that arrives depends on how the
   * path and the preopen line up. Matching the basename absorbs all of that.
   *
   * It is NOT a wildcard: an unpublished name still gets ENOENT, so a stray
   * open cannot silently receive the sidecar. That distinction is the reason
   * this is not just "return the one file we have".
   */
  private resolve(path: string): Uint8Array | undefined {
    const base = path.replace(/\/+$/, '').split('/').pop() ?? '';
    return this.contents.get(base);
  }

  pathOpen(dirfd: number, path: string): { errno: number; fd: number } {
    if (dirfd !== PREOPEN_FD) return { errno: E.BADF, fd: 0 };
    const data = this.resolve(path);
    if (!data) return { errno: E.NOENT, fd: 0 };
    const fd = this.nextFd++;
    this.open.set(fd, { data, pos: 0 });
    return { errno: E.SUCCESS, fd };
  }

  close(fd: number): number {
    if (fd === STDIN || fd === STDOUT || fd === STDERR) return E.SUCCESS;
    if (fd === PREOPEN_FD) return E.SUCCESS;
    if (!this.open.delete(fd)) return E.BADF;
    return E.SUCCESS;
  }

  /** Size and filetype for `fd_filestat_get`. */
  stat(fd: number): { errno: number; size: number; filetype: number } {
    if (fd === STDIN || fd === STDOUT || fd === STDERR) {
      return { errno: E.SUCCESS, size: 0, filetype: FILETYPE.CHARACTER_DEVICE };
    }
    if (fd === PREOPEN_FD) {
      return { errno: E.SUCCESS, size: 0, filetype: FILETYPE.DIRECTORY };
    }
    const f = this.open.get(fd);
    if (!f) return { errno: E.BADF, size: 0, filetype: FILETYPE.UNKNOWN };
    return { errno: E.SUCCESS, size: f.data.length, filetype: FILETYPE.REGULAR_FILE };
  }

  isPreopen(fd: number): boolean {
    return fd === PREOPEN_FD;
  }

  /**
   * Reads up to `max` bytes, advancing the position. Returns an empty array at
   * EOF, which is what terminates `std::fs::read`'s loop.
   *
   * stdin reads EOF unconditionally. A blocking stdin would deadlock the
   * non-blocking driver, and no fixture needs it.
   */
  read(fd: number, max: number): { errno: number; data: Uint8Array } {
    if (fd === STDIN) return { errno: E.SUCCESS, data: new Uint8Array(0) };
    const f = this.open.get(fd);
    if (!f) return { errno: E.BADF, data: new Uint8Array(0) };
    const n = Math.min(max, f.data.length - f.pos);
    const data = f.data.subarray(f.pos, f.pos + n);
    f.pos += n;
    return { errno: E.SUCCESS, data };
  }

  write(fd: number, bytes: Uint8Array): { errno: number; written: number } {
    if (fd === STDOUT || fd === STDERR) {
      this.onWrite(fd, bytes);
      return { errno: E.SUCCESS, written: bytes.length };
    }
    // Read-only by design: nothing in ecvisor writes to a host file.
    return { errno: E.BADF, written: 0 };
  }
}
