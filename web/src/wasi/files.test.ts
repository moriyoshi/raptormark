import assert from 'node:assert/strict';
import { test } from 'vitest';

import { E, FILETYPE } from './abi.ts';
import { Files, PREOPEN_FD } from './files.ts';

const enc = (s: string) => new TextEncoder().encode(s);
const dec = (b: Uint8Array) => new TextDecoder().decode(b);

function withSidecar(bytes = enc('sidecar-bytes')) {
  const f = new Files('/');
  f.add('rootfs.img', bytes);
  return f;
}

/**
 * ⚠️ BASENAME MATCHING IS DELIBERATE AND IS NOT A WILDCARD. Rust's std strips
 * the preopen prefix before the call, so what arrives depends on how the guest's
 * configured path and the preopen line up -- `/rootfs.img`, `rootfs.img` and
 * `./rootfs.img` are all the same request. Matching the basename absorbs that.
 *
 * The second half is the half that matters: an UNPUBLISHED name must still get
 * ENOENT. "Return the one file we have" would satisfy every assertion above and
 * would hand the sidecar to any stray open the guest makes.
 */
test('a sidecar is found under every spelling of its path, and only its own', () => {
  const f = withSidecar();
  for (const path of ['rootfs.img', '/rootfs.img', './rootfs.img', '/a/b/rootfs.img']) {
    const r = f.pathOpen(PREOPEN_FD, path);
    assert.equal(r.errno, E.SUCCESS, `${path} should resolve to the sidecar`);
  }
  assert.equal(
    f.pathOpen(PREOPEN_FD, '/etc/passwd').errno,
    E.NOENT,
    'an unpublished name must not receive the one file that is published',
  );
  assert.equal(f.pathOpen(PREOPEN_FD, '').errno, E.NOENT);
});

test('opening through anything but the preopen is EBADF', () => {
  const f = withSidecar();
  assert.equal(f.pathOpen(PREOPEN_FD + 1, 'rootfs.img').errno, E.BADF);
  assert.equal(f.pathOpen(0, 'rootfs.img').errno, E.BADF);
});

/**
 * ⚠️ AN EMPTY READ AT EOF IS WHAT TERMINATES `std::fs::read`. Returning EBADF or
 * looping forever there is how a sidecar load hangs, and `load_sidecar` is the
 * guest's very first host interaction -- so it hangs before printing anything.
 */
test('reads advance and stop, rather than repeating the last chunk', () => {
  const f = withSidecar(enc('0123456789'));
  const fd = f.pathOpen(PREOPEN_FD, 'rootfs.img').fd;

  assert.equal(dec(f.read(fd, 4).data), '0123');
  assert.equal(dec(f.read(fd, 4).data), '4567');
  assert.equal(dec(f.read(fd, 4).data), '89', 'a short final chunk, not a padded one');

  const eof = f.read(fd, 4);
  assert.equal(eof.errno, E.SUCCESS, 'EOF is success with no bytes, not an error');
  assert.equal(eof.data.length, 0);
});

/**
 * ⚠️ TWO OPENS MUST NOT SHARE A POSITION. A single cursor per FILE rather than
 * per DESCRIPTOR reads correctly for the one-open case every fixture exercises,
 * and silently returns the wrong half of the sidecar the moment anything opens
 * it twice.
 */
test('two descriptors on one file have independent positions', () => {
  const f = withSidecar(enc('AAAABBBB'));
  const a = f.pathOpen(PREOPEN_FD, 'rootfs.img').fd;
  const b = f.pathOpen(PREOPEN_FD, 'rootfs.img').fd;
  assert.notEqual(a, b, 'each open gets its own descriptor');

  assert.equal(dec(f.read(a, 4).data), 'AAAA');
  assert.equal(dec(f.read(b, 4).data), 'AAAA', 'the second descriptor starts at zero');
  assert.equal(dec(f.read(a, 4).data), 'BBBB');
});

test('stdin is at EOF rather than blocking', () => {
  // A blocking stdin would deadlock the non-blocking driver: nothing can ever
  // deliver the bytes, because the host is inside the guest's call.
  const f = withSidecar();
  const r = f.read(0, 16);
  assert.equal(r.errno, E.SUCCESS);
  assert.equal(r.data.length, 0);
});

test('writes reach the output hook, and files are read-only', () => {
  const f = withSidecar();
  const seen: [number, string][] = [];
  f.onWrite = (fd, bytes) => seen.push([fd, dec(bytes)]);

  assert.deepEqual(f.write(1, enc('out')), { errno: E.SUCCESS, written: 3 });
  assert.deepEqual(f.write(2, enc('err')), { errno: E.SUCCESS, written: 3 });
  assert.deepEqual(seen, [
    [1, 'out'],
    [2, 'err'],
  ]);

  const fd = f.pathOpen(PREOPEN_FD, 'rootfs.img').fd;
  assert.equal(f.write(fd, enc('nope')).errno, E.BADF, 'nothing in ecvisor writes a host file');
});

test('stat distinguishes the preopen, the std streams and a real file', () => {
  const f = withSidecar(enc('12345'));
  assert.equal(f.stat(PREOPEN_FD).filetype, FILETYPE.DIRECTORY);
  assert.equal(f.stat(1).filetype, FILETYPE.CHARACTER_DEVICE);

  const fd = f.pathOpen(PREOPEN_FD, 'rootfs.img').fd;
  const st = f.stat(fd);
  assert.equal(st.filetype, FILETYPE.REGULAR_FILE);
  // ⚠️ The SIZE is what `std::fs::read` preallocates from. A zero here turns a
  // whole-file read into a growing-buffer read that still works, so a wrong
  // value is invisible until it is wrong in the other direction.
  assert.equal(st.size, 5);

  assert.equal(f.stat(9999).errno, E.BADF);
});

test('closing is idempotent for the std fds and the preopen, but not for a file', () => {
  const f = withSidecar();
  for (const fd of [0, 1, 2, PREOPEN_FD]) {
    assert.equal(f.close(fd), E.SUCCESS, `fd ${fd} must survive a close`);
    assert.equal(f.close(fd), E.SUCCESS, `fd ${fd} must survive a second close`);
  }
  const fd = f.pathOpen(PREOPEN_FD, 'rootfs.img').fd;
  assert.equal(f.close(fd), E.SUCCESS);
  assert.equal(f.close(fd), E.BADF, 'a double close of a real descriptor is an error');
  assert.equal(f.read(fd, 4).errno, E.BADF, 'and it is really gone');
});
