# TODO

Open work items. Normally populated by the `good-sleep` skill, which extracts
to-do items out of `.agents/docs/JOURNAL.md` as it consolidates entries into
`.agents/docs/LTM/`.

**This file holds OPEN items only, as of 2026-08-23.** Completed entries are
moved verbatim to `JOURNAL.md` under dated "Completed TODO entries" sections,
grouped by the section they came from. Nothing is summarised or dropped: moves
are verified block-by-block, and in-file cross-references ("the item below",
"original entry below") are deliberately left unrewritten, so some point at
neighbours that stayed here. When you finish an item, move it to `JOURNAL.md`
rather than checking a box here.

**Markers.** ⭐ on an entry means **a decision with unusually wide blast radius
— not work to pick up casually**. Both current uses say so in their own words:
the speculative-reservation entry is "the cross-cutting question the four
entries around it are each one face of", and the `orr` ASIMD one is "a DECISION,
not work anyone should just take -- a lifter patch changes `BaseID` and
invalidates the 6.2 GB object cache". ⚠️ Written down 2026-08-25 because the
convention was undocumented and had been inferred from two samples, then
repeated as though it carried defined authority.

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

- [ ] **⭐ NO IMAGE'S ENTRYPOINT SCRIPT CAN RUN. `raptormark run` on postgres:17
      runs APT.** Found 2026-08-27 by building postgres:17 end to end; mechanism
      corrected 2026-08-28 after the operator asked whether the entrypoint is
      resolved against the WORKING DIRECTORY. It is, and that is the whole thing.

      ```
      [ecvisor] ran 77 _dl_init constructor(s)
      WARNING: docker-entrypoint.sh does not have a stable CLI interface. ...
      E: Invalid operation postgres
      ```
      apt's error text; apt is program 0. The documented program-0 fallback
      (`execmap.rs`, four incidents).

      **1. Resolution is cwd-relative, with NO PATH lookup.**
      `entry.rs` does `programs.resolve(&vfs, &cwd, argv[0])`, and
      `Programs::hash_for` tries an exact exec-map key, then
      `vfs.resolve(cwd, path)`. Docker resolves a bare exec-form entrypoint with
      `execvp`, i.e. against PATH. The runtime implements a different rule.

      | image | entrypoint | cwd | resolves? |
      |---|---|---|---|
      | postgres:17/18, node:22-slim, php:8.3-cli | bare, script in `/usr/local/bin` | `/` | ❌ |
      | redis:7-alpine | bare | **`/data`** -> `/data/docker-entrypoint.sh` | ❌ |
      | nginx:latest/alpine | `/docker-entrypoint.sh` (absolute) | `/` | ✅ |

      ❗ **5 of 7 surveyed images miss.** nginx works only because its Dockerfile
      happens to write the path with a leading slash. This is not a postgres
      problem; it is every image using the idiomatic bare form.

      **2. A resolved path would NOT be enough, and this is the bigger half.**
      `boot` resolves registry PROGRAMS. A script is not one. And `execve`
      returns ENOEXEC for scripts, because `sys.rs:5505` says plainly: "a script
      -- shebang support is a later addition".

      **3. So `internal/image/image.go`'s `Script` doc is FALSE.** It says
      "Scripts are NOT registry programs -- the runtime's shebang handling execs
      the *interpreter* and feeds it the script file". The runtime says it has no
      shebang handling. The entire scripts-as-exec-targets design rests on a
      capability that does not exist.

      ⚠️ **Consequence, and it reframes the closure work.** Discovery walks
      scripts and pulls in what they invoke -- correct, and today's shell parsing
      made it much better. But the guest cannot EXECUTE any of those scripts. So
      `raptormark run` cannot reproduce `docker run` for any image whose
      entrypoint is a script, which is nearly all of them.

      ⭐ **The fork:**
        1. **Implement shebang support** (boot and execve): resolve `#!`, exec
           the interpreter with the script path as argv[1], script bytes from the
           sidecar. Faithful, and it makes the closure's scripts real. Also what
           `image.go` has claimed all along.
        2. **Accept that argv[0] must name a program**: the pipeline writes the
           resolved entry PROGRAM into the boot record. postgres would run
           directly, skipping PGDATA setup and `exec gosu postgres`. Cheap, and
           dishonest about what it is.
      ✅ Either way the sidecar is host-side: `rootfs.Build` is minutes, not the
      7h25m the 1.65 GB module cost. Only option 1 needs a runtime rebuild.
      ⚠️ Also fix the PATH half regardless -- a bare argv[0] should be resolved
      against PATH, host-side, where discovery already does exactly that.

- [x] **✅ FIXED 2026-08-26: shell scripts are now PARSED, and the closure was
      missing programs the entrypoint chain actually runs.** `internal/image/
      shellscan.go`, using `mvdan.cc/sh/v3/syntax`.

      **What was broken.** `bareWords` was the only mechanism a script had, and
      the other extractor, `absolutePaths`, requires a NUL terminator -- a shell
      script is text, so **no literal absolute path in a script was ever
      followed.** `nginx:alpine` reaches its four `/docker-entrypoint.d/` scripts
      only through `$f`, whose value comes from `find`, so they were never
      opened. The one literal available is the DIRECTORY, and it appears as an
      ARGUMENT to `find`, not in command position.

      **Measured effect** (`.agents-workspace/drivers/headroom -fuse`):

      | image | before | after | the additions |
      |---|---|---|---|
      | nginx:latest | 6 | **29** | mountpoint, envsubst, basename, dirname, sha1sum, md5sum, seq, getconf, dpkg-query, … |
      | nginx:alpine | 3 | **6** | envsubst, getconf, apk |
      | redis:7-alpine | 3 | 3 | — |
      | php:8.3-cli | 6 | 6 | — |
      | debian:trixie-slim | 1 | 1 | — |
      | postgres:17 | 71 | 71 | — |

      ❗ **The additions are REAL, checked against the scripts rather than
      assumed.** `grep` over nginx:latest's `/docker-entrypoint.d/*.sh` counts
      `mountpoint` 9x, `envsubst` 6x, `basename` 3x, `dirname` 2x, and one each
      of `sha1sum`, `seq`, `md5sum`, `getconf`, `dpkg-query`. All three of
      nginx:alpine's are invoked too (`envsubst` at `20-…:53`, `getconf
      _NPROCESSORS_ONLN` at `30-…:168`, `apk manifest nginx` at `10-…:50`).
      **Every one was an `execve` with no exec-map entry at run time**, i.e. the
      silent fallback to program 0.

      **Why a parser and not a regex.** Word boundaries, and one case decides it:
      `PATH=/usr/local/bin:/usr/bin` is ONE word that does not start with `/`. A
      regex finds `/usr/bin` there and offers it for directory fan-out, which
      admits every executable in the image. Also: comments are not words, and
      `case "$f" in *.sh)` is a pattern, not a path.

      ✅ **ADDITIVE ONLY, and tested as such.** The scan is unioned with the
      UNCHANGED `bareWords` loop; `TestShellScanOnlyEverWidensTheClosure` compares
      against `bareWordClosure`, an independent reimplementation of the
      pre-change algorithm kept in the test file. A parse failure or a non-shell
      shebang costs nothing -- the script gets exactly its old treatment.
      ✅ Neutralized three ways, none by compile error: suppressing the
      directory fan-out fails the nginx test naming `envsubst`; dropping relative
      `source`/`exec` fails the scanner test; stubbing `scanShell` to always
      error fails the traversal tests **while the widening and non-shell tests
      still pass**, which is what proves the fallback is intact rather than
      merely present.
      ❗ **That third neutralization found a defect in my own test.** The first
      version of the widening test computed its expectation with `bareWords` over
      EVERY script in the inventory, including the four only the fan-out reaches
      -- so it was re-asserting reachability and it FAILED under the stub, which
      is the one case where the fallback is supposed to carry the closure
      unharmed. Predicting "this one still passes" is what exposed it.

      ⚠️ **COST, and it is not small.** nginx:latest goes from 34.9 MB of fused
      images to 100.9 MB and from 6 programs to 29 -- 23 more full translations,
      minutes to hours each -- and its region use rises 18.5% -> 23.6%. The
      object cache goes cold for any image whose closure changed. This is the
      price of the programs being present at all, but it is real.
      ⚠️ **Widening AMPLIFIES the existing over-approximation.** Each
      newly-reachable script is also fed to `bareWords`, so its noise words are
      resolved against PATH too. The narrowing half (command-position extraction)
      is what would pay that back and is deliberately out of scope.
      ⚠️ **postgres is UNCHANGED at 71 and still 16.1 MiB over the region
      ceiling.** Its entrypoint does not use the fan-out idiom. The region
      decision is untouched.

      ✅ **AND WHAT IS NOT RESOLVED IS NOW REPORTED, 2026-08-27.**
      `image.Closure` returns `[]image.Unresolved` alongside the closure;
      `raptormark build` prints a capped summary and `pipeline.Result.Unresolved`
      carries the full list. Each entry has the reference as written, the script,
      a kind (`command` = a computed EXEC target, `path` = a path-shaped
      argument), and **`Via`, the commands inside any `$(…)`**.

      ❗ **`Via` is the whole point.** "Should we emulate sed/awk/printf?" was
      asked 2026-08-26 and answered by measuring six images: 37 command
      substitutions, **none producing a path to an executable** -- they make log
      prefixes (`ME=$(basename "$0")`), a resolver list from `/etc/resolv.conf`,
      an envsubst filter from `ENVIRON`, and `mkdir -p` subdirs. Two of those
      inputs do not exist at build time, so a perfect `sed` would not help: **the
      ceiling is not effort, it is that raptormark is AOT.** Six images is a
      sample, so rather than pick a tool to emulate from it, the scanner now
      reports and the next image supplies evidence.

      First readings: nginx:latest 13 references, 1 computed exec (`"$f"`, already
      covered by the fan-out), `commands inside $(...): awk=1` -- and that awk
      builds an env filter, not a path. postgres:17 10 references, 2 computed
      execs (`"$f"` over the empty `/docker-entrypoint-initdb.d`, and
      `"${query_runner[@]}"`, a bash array whose value `psql` is already in the
      closure).

      ⚠️ **The first version was correct and unusable**, which running it showed:
      nginx:latest reported 25 entries of which ONE mattered, the rest being
      `entrypoint_log` arguments that contain a slash because they quote a
      filename. A literal-whitespace filter now excludes them -- **except when the
      word contains a `$(…)`**, because the awk case that answers the question is
      full of spaces and would otherwise have been filtered away with the noise.
      Both halves have controls.
      ⚠️ `exec "$@"` is deliberately NOT reported: it ends nearly every
      entrypoint and `pipeline.EntryFromSeeds` already resolves it from CMD. A
      diagnostic whose first line is always noise is one people stop reading.
      ❗ A positive control caught a real gap here: `exec "$CMD"` reported
      NOTHING, because command position is the literal `exec` and `"$CMD"` has no
      literal separator. Without that control the positional exclusion would have
      been indistinguishable from a reporter that never fires.

      **Still not handled**, and none of it is silent -- each simply yields
      nothing, and is now REPORTED rather than merely absent: `${VAR}` / `$(cmd)`
      in a path, relative paths outside `.`/`source`/`exec`, `zsh` (the parser
      does not implement it), and any directory named only through an expansion.
      `ClosureOptions.Extra` remains the escape hatch.
      ⚠️ **`image.Closure`'s signature changed** to `([]string, []Unresolved,
      error)`, matching `image.Plugins`' shape. 12 call sites take `_`.

- [x] **✅ FIXED 2026-08-26: `raptormark build <image>` FAILED AT DISCOVERY for
      almost every real image, because the entry was `seeds[0]` and a real
      image's ENTRYPOINT is a shell script.** Verified against the real command,
      not reconstructed:

      ```
      $ raptormark build nginx:alpine --out … --builder raptormark-builder:rebal
      discovery: exporting nginx:alpine ...
      build <image>: the entry /docker-entrypoint.sh is not in the closure
      (3 programs). …                                            [exit 1]
      ```

      ❗ **The two halves of discovery disagreed, and each was right on its own.**
      `image.EntrypointSeeds` deliberately resolves SCRIPTS as well as programs
      (`resolveProgram` returns `inv.Scripts[p]`), because `image.Closure` seeds
      FROM a script -- it scans it for the bare words naming real programs. So a
      script seed is correct, and a script is by construction never a MEMBER of
      the closure. `pipeline.build` then took `seeds[0]` as the entry and required
      it to be a member.

      **Measured over seven images** with `.agents-workspace/drivers/headroom`,
      which now dumps every seed and whether it is in the closure:

      | image | seeds[0] | seeds[1] |
      |---|---|---|
      | nginx:alpine | `/docker-entrypoint.sh` ❌ | `/usr/sbin/nginx` ✅ |
      | nginx:latest | `/docker-entrypoint.sh` ❌ | `/usr/sbin/nginx` ✅ |
      | postgres:17 | `/usr/local/bin/docker-entrypoint.sh` ❌ | `/usr/lib/postgresql/17/bin/postgres` ✅ |
      | redis:7-alpine | `/usr/local/bin/docker-entrypoint.sh` ❌ | `/usr/local/bin/redis-server` ✅ |
      | node:22-slim | `/usr/local/bin/docker-entrypoint.sh` ❌ | `/usr/local/bin/node` ✅ |
      | php:8.3-cli | `/usr/local/bin/docker-php-entrypoint` ❌ | `/usr/local/bin/php` ✅ |
      | ruby:3-slim | `/usr/local/bin/irb` ❌ | *(none)* |

      Seeds are ENTRYPOINT then CMD, so **"the first seed that is in the closure"
      is exactly "the program the entrypoint script runs"**, and it is right in
      six of the seven.

      ✅ **`entryFromSeeds` extracted from `build`** so the rule is testable
      without an image or a builder -- the pattern `suspendViaCallFor` and
      `runtimeArgs` already use.
      ✅ **STRICTLY WIDENING.** When `seeds[0]` IS in the closure (a bare program
      CMD -- `debian:trixie-slim`'s `bash`, `python:3-slim`'s `python3`) it is
      still chosen, because it is the first match. Nothing that built before
      builds differently; only inputs that hard-failed can move. `e2e`'s
      `pgExtFixture` is `CMD ["/usr/bin/pgdlhost"]` with no ENTRYPOINT, which is
      why the suite never saw this.
      ✅ **It still REFUSES `ruby:3-slim`**, which has one seed (CMD `irb`, a
      script, no ENTRYPOINT). Falling through to `closure[0]` would make every
      image "work" and is the guess `build.go`'s own comment rejects -- right
      most of the time, which is the worst kind. The refusal names `--entry` and
      explains why a script is never in the closure.
      ✅ Guarded by `TestEntryFromSeedsTakesTheFirstSeedThatIsAProgram` (seed
      lists TRANSCRIBED from the seven measured images, plus three controls) and
      `TestEntryFromSeedsRefusesWhenNoSeedIsAProgram`. Both neutralized, neither
      by compile error: reverting to `seeds[0]` fails the four script cases while
      the two program-CMD controls still pass, and falling through to
      `closure[0]` fires the refusal test.
      ✅ **Verified by effect on the real command.** `raptormark build
      nginx:alpine` now reports `entry /usr/sbin/nginx`, `5 plugin(s)`,
      `fuse: 8 image(s)` -- 3 programs and 5 units -- and proceeds into
      translation. The units are new: the entry-carries-units branch is
      `guest == entry`, so while the entry was outside the closure **every
      image's plugin units were silently skipped** as well.

      ⚠️ **This changes nothing about `raptormark run`,** and an entry that is
      reachable is not the same as an image that runs. postgres additionally has
      the region overflow below; python has the `_tkinter` refusal above.

- [ ] **`node:22-slim` cannot be fused: 29 unhandled `R_AARCH64_COPY`
      relocations.** Found 2026-08-26 by the `-fuse` survey. Two of its three
      programs fuse; `/usr/local/bin/node` fails with

      ```
      fuse: 29 relocation(s) need a policy decision:
        node: unhandled R_AARCH64_COPY at 0x66be370
        …
      ```

      ❗ **This is a NEW gap, not a known one.** `R_AARCH64_COPY` appears nowhere
      in this file, in `.agents/docs/LTM/`, or in `internal/fuse/*.go` outside the
      generic `UnsupportedError` that reports it. The diagnostic is good -- loud,
      counted, addressed -- which is why it was found in seconds once anything
      actually fused a real node image.

      ❗ **RE-SCOPED 2026-08-27 BY MEASUREMENT: THIS IS THE NON-PIE CASE, AND
      NON-PIE IS A SHRINKING MINORITY.** Counting `R_AARCH64_COPY` in each
      surveyed image's principal binary:

      | image | ELF type | COPY relocs |
      |---|---|---|
      | node:22-slim | **ET_EXEC** | **29** |
      | php, postgres, redis, nginx, ruby, python | ET_DYN (PIE) | **0** |

      The zeros are STRUCTURAL, not luck: a copy relocation exists only because a
      **non-PIE** executable references a data object in a shared library. A PIE
      reaches the same data through the GOT and the linker never allocates a
      copy. So this is not "node is the first and others will follow" -- node is
      the only non-PIE binary in the survey, and distro toolchains have defaulted
      to PIE for years.

      **What handling them would mean**, investigated 2026-08-27 and cheaper than
      this entry first said:
        * ✅ **The re-pointing half is FREE.** node DEFINES each copy-reloc symbol
          in its own `.dynsym` (measured: `in6addr_any` 16 bytes,
          `_ZSt7nothrow` 1, `_ZTVN10__cxxabiv117__class_type_infoE` 88, all at
          the relocation's own offset), and `globalSymbols` is first-wins with
          the executable at `objs[0]`. Every reference, including the library's,
          already resolves to the executable's copy.
        * What is missing is the memcpy: `st_size` bytes from the LIBRARY's
          definition into the executable's slot.
        * ⚠️ **It must be DEFERRED**, like `ifuncFixup` already is. The targets
          are in `.data.rel.ro`, they are C++ vtables full of pointers, and the
          executable does not relocate them itself -- measured, only 3 unrelated
          `ABS64` touch that range. The pointers become correct when the SOURCE
          library is relocated, so the copy has to run after `relocate` returns.
        * ⚠️ **Source selection is versioned** (`@GLIBCXX_3.4.21`,
          `@GLIBC_2.17`) and would need the same default-version preference
          `globalSymbols` gained on 2026-08-26. Copying the wrong version is
          silent.

      ⚠️ **NOT TAKEN, and the reason is the priority, not the difficulty.** One
      image in seven, for the case the ecosystem is moving away from, against a
      change whose failure mode is a C++ program that crashes deep in its runtime.
      ✅ **It IS cheaply verifiable if it is ever wanted** -- and this entry
      previously implied otherwise by pointing at node's size. A minimal non-PIE
      C++ guest that references a vtable from a shared library reproduces the
      relocation in seconds, so it does not need node's 122 MB translated.
      ⚠️ Also relevant if node is ever the target: it is at **73.2%** of the
      fused region with only 7 libraries, the tightest of any non-postgres image.

- [ ] **⭐ `raptormark build python:3-slim` FAILS, and the safety net the code
      documents for exactly this case is not on the build path.** Found
      2026-08-26, immediately after the `/usr/local/lib` fix below unblocked the
      layout -- which is the lesson: **a plan is not a build**, and "the layout
      planned" was being read as "the image builds".

      ```
      fuse: cannot satisfy dlopen'd plugin .../_tkinter.cpython-314-aarch64-linux-gnu.so:
      fuse: cannot find libtk8.6.so in [...]
      ```

      ❗ **The module is broken IN THE SOURCE IMAGE.** Verified, not inferred: no
      `libtk*` or `libtcl*` anywhere in the rootfs, and `import tkinter` in the
      stock `python:3-slim` container raises the identical
      `libtk8.6.so: cannot open shared object file`. Debian ships the extension
      without its dependency. So raptormark refuses to build the entire image
      over a module the guest could never have loaded.

      ❗ **The handling exists and is bypassed.** `fuse.load` ALREADY skips an
      unsatisfiable plugin and records it as `SkippedExtra` -- and that type's
      doc names `_tkinter` as "the case that forced this". But:
        * `fuse.FuseClosure` degrades and reports through `Report.SkippedExtras`;
        * `fuse.Fuse` and `fuse.FuseWithUnits` turn the same skip into a FATAL
          error, deliberately, because "this signature cannot report it";
        * **`internal/pipeline.build` never calls `FuseClosure`.** It calls
          `FuseWithUnits` for the entry and `Fuse` for every other program.

      So `internal/image/plugins.go` carried a ⚠️ note saying `FuseClosure`
      reports these "through `Report.SkippedExtras`, which is a different and
      equally visible list" -- true about `FuseClosure`, and false as protection
      for a build. ✅ That comment is CORRECTED in place; the defect is not.

      ⭐ **THE DECISION, and it is a policy one so it was not taken here.** The
      asymmetry `Fuse` documents is RIGHT for an explicit `Options.Extra`:
      somebody named that plugin and a silently absent dlopen'd module is the
      exact failure `Extra` exists to prevent. Under `--plugins auto` nobody named
      it -- discovery found it by walking a directory. That distinction is what a
      fix should turn on. Two placements:
        1. **Exclude at discovery.** `image.Plugins` already returns
           `ExcludedPlugin{Guest, Reason}` and `pipeline.Build` already PRINTS
           them, and the type's own doc calls an unfusable plugin "a capability
           the guest does not have". Cost: `Plugins(root)` would have to resolve
           `DT_NEEDED`, so it needs the library search list -- a signature change.
        2. **Thread `SkippedExtra` out of `FuseWithUnits`/`Fuse`.** Cheaper, but
           it weakens the explicit-`Extra` guarantee unless the two cases are
           distinguished, and `pipeline.Result` deliberately has no `Skipped`
           field today.
      ⚠️ Whichever is chosen, the guard must distinguish "the image does not
      contain this dependency" from "our search list is too narrow" -- the second
      is what the `/usr/local/lib` entry below was, and silently excluding
      plugins would have HIDDEN it.

- [x] **✅ FIXED 2026-08-26: the library search list was a SUBSET of what the
      images declare, and `python:3-slim` / `ruby:3-slim` could not be fused at
      all.** `pipeline.libraryPaths` listed four Debian multiarch directories.
      Debian's own `/etc/ld.so.conf.d/` names four, and two of them were absent:

      ```
      libc.conf               /usr/local/lib
      aarch64-linux-gnu.conf  /usr/local/lib/aarch64-linux-gnu, /lib/…, /usr/lib/…
      ```

      `python:3-slim` keeps `libpython3.14.so.1.0` in `/usr/local/lib` and
      `ruby:3-slim` keeps `libruby.so.3.4` there, so each image's PRINCIPAL
      library resolved nowhere: `fuse: cannot find libpython3.14.so.1.0 in [...]`.
      Both images are in README's survey. Found by planning a layout for nine
      real images with `.agents-workspace/drivers/headroom` -- **not** by reading
      the list, which looks complete and carries a comment explaining why the
      four are right.

      ✅ **APPENDED, never inserted, and that is what made it safe to ship.**
      `fuse.findLib` takes the FIRST match over an ordered list, so an entry at
      the end cannot move a name that already resolved -- only one that resolved
      nowhere, which moves from "the build fails" to "found". No fused bytes
      change for any image that was already building, so no cached object is
      spent. Putting `/usr/local` first would match Debian's ld.so.conf ORDER and
      would silently re-point every name present in both places.

      ✅ Guarded by `TestLibraryPathsCoverWhatTheImageItselfDeclares`, whose
      wanted set is TRANSCRIBED from `cat /etc/ld.so.conf.d/*.conf` inside
      `python:3-slim` rather than read back from the function. Neutralized BOTH
      arms, neither by compile error: dropping the two entries fires the
      missing-directory arm naming both, and moving them to the front fires the
      ordering arm naming the indices.
      ✅ Verified by effect, not only by test: both images now plan a shared
      layout -- python 43.4% used / 88.4 MiB free, ruby 21.3% / 122.7 MiB.

      ⚠️ **WHAT IS STILL OPEN, and the fix must not be read as more than it is.**
      The list is still HARDCODED. Nothing reads the rootfs's own
      `/etc/ld.so.conf`, and the fuser honours neither `DT_RUNPATH` nor
      `DT_RPATH` -- it reads only `DT_NEEDED` (`fuse.go:272`, `:290`). An image
      with a custom conf, or one that relies on a `$ORIGIN`-relative RUNPATH,
      still fails the same way.
      ⚠️ **Six more copies of the four-path list live inline in `e2e/`**
      (`e2e_test.go`, `sharednames_test.go`, `pgdlopen_test.go`,
      `rtldhooks_test.go`, `minsigstksz_test.go`, `tlsdesc_test.go`,
      `hostedload_test.go`) and were deliberately NOT updated: every one of them
      uses a Debian fixture that keeps nothing in `/usr/local`, so nothing
      behaves differently. Per this file's own rule, "it appears twice" is not
      sufficient reason to touch it -- but a fixture that moves to a
      `/usr/local` image will need them.

- [x] **✅ Dynamic side-module loading RUNS. Moved here 2026-08-23.**
      `TestPostgresStyleDlopenResolvesPerUnit` passes against
      `raptormark-builder:unitfix`: two plugins fused as their own units,
      lifted, linked into one module with a dlopen map, and a real glibc guest
      gets `0xA1` from ext_a and `0xB2` from ext_b through distinct handles,
      with each unit's constructors run at its own `dlopen`.

      **Neutralized**: making `dlsym` ignore its handle reproduces the postgres
      defect exactly -- ext_b returns ext_a's magic, both handles resolve to one
      address, `_PG_init` collapses too. All three discriminators fire.

      Required `patches/0066-drvd-rmp-covers-store-before-call.patch`, whose
      root cause was our own `patches/0007`. Full trail in `JOURNAL.md`.

      Gates: Go 10 packages, Rust 280 tests + wasm32 check, Bazel 11/11,
      E2E differential green.

      ⚠️ STILL OPEN below: the ORIGINAL entry, because much of it is unchanged.

- [ ] **Dynamic side-module loading: the parts still not exercised.**
      Added 2026-08-23. Phases 0-4 of the plan are implemented -- plugin band,
      per-plugin unit fusing, the dlopen map, discovery, handle-aware `dlsym`,
      real `dlerror`, the loader seam with `preloaded` and `hosted`, park/wake,
      the un-freeze. Full trail in `JOURNAL.md` under the 2026-08-23 entries.

      ⚠️ **CORRECTED 2026-08-23, later the same day.** This entry was written
      when nothing had run and said so. Since then a builder image was rebuilt
      (`raptormark-builder:unitfix`), the full E2E suite passed, the Bazel gate
      passed 11/11, and the postgres differential passes AND neutralizes. See
      the entry above. What follows is only what is STILL not exercised.

      Still open, and what each needs:
        * ✅ **`//runtime:loader_exclusion_test` DONE** 2026-08-23. Bazel is
          now 12/12. `//runtime/hosted` builds the second archive; the test
          matches the mangled `ecvisor::loader::hosted` path (NOT the import
          name, which appears in neither archive) and carries two controls --
          an export floor and a "pattern is live" check. Those controls caught
          a vacuous pass on the first run.
        * ✅ **DONE 2026-08-24: BOTH TRIGGERS serve a real load mid-run.**
          `execve` was the second half of the plan's one-seam argument and it
          did not work: `ensure_unit_loaded` sat on the execve path but takes a
          registry INDEX, which a program the host has not placed does not have.
          `Programs::load` resolved exec-map paths to indices AT CONSTRUCTION and
          dropped the unregistered ones -- the exact defect `dlmap` was written
          hash-keyed to avoid, one map later. `execve` to a deferred program gave
          ENOEXEC, and startup shouted "the guest runs the WRONG PROGRAM" about
          the normal case. Fixed by mirroring dlmap; `e2e/execload_test.go`
          proves it, and the run against the pre-fix image is the neutralization.
        * ✅ **DONE 2026-08-24: a real dlopen load is SERVED mid-run.** A guest
          calls `dlopen`, parks; the host compiles and instantiates the unit's
          side module in response, registers it, calls `ecv_side_loaded`; the
          guest wakes and its `dlsym` resolves out of a unit that was not in the
          module a moment earlier. Two units, two distinct addresses, correct
          magics, `loads served: 2`, exit 0.
          `e2e/hostedload_test.go` + `e2e/testdata/hostedembedder.mjs`.

          Neutralized: `NEUTRALIZE_REFUSE_ALL=1` makes the host refuse every
          load; both dlopens return NULL with a real `dlerror` and all three
          assertions fire.

          ❗ **Four defects found, all only reachable by running it** -- a
          parked dlopen reported as DEADLOCK (the guard was on the branch a
          socket-free profile never takes); a wake that set `Runnable` without
          `run_queue.push_back`, giving a CLEAN EXIT 0 with the guest's work
          undone; the same unit placed twice under its token and then its index;
          and `Hosted::slot` doing `Vec::resize(2^30)`, whose first call
          quietly took ~1 GiB and whose second dlopen trapped. Plus two build
          gaps: `supervisorLinkArgs` ignored `--profile`, and `LinkRequest` had
          no `Profile` field. See the 2026-08-24 JOURNAL entry.
        * ✅ **PHASE 4 IS DONE. Re-verified 2026-08-25 against the tree, and the
          bullet below was STALE when it was written down.** It named the wrong
          file: the mid-run §8 sequence was delivered in a SECOND harness,
          `e2e/testdata/hostedembedder.mjs`, not by extending `embedder.mjs`.
          Verified by reading it — it parses `dylink.0` MEM_INFO (`:82`, and
          `:233` REFUSES a module linked flat), reserves and instantiates,
          calls `__wasm_apply_data_relocs` (`:302`), registers, and calls
          `ecv_side_loaded` (`:347`) **between `ecv_run_slice` calls**, which is
          the whole of what the bullet asked for. Three tests drive it:
          `hostedload_test.go`, `execload_test.go`, `pipelinehosted_test.go`.
          ❌ **`embedder.mjs` was deliberately NOT extended, and should not be.**
          Its header states its role — a DEVELOPMENT embedder that places every
          side module up front to prove the §8 protocol works on real artifacts.
          Teaching it to load mid-run would merge two harnesses that answer two
          different questions, and would cost the up-front path its only witness.
          Original text: **`e2e/testdata/embedder.mjs` mid-run load** -- needs a
          supervisor rebuilt with `link-all --side-out`; the fixtures predate
          every change. This is now the ONLY thing between Phase 4 and done ...
          Late registration already exists (`push_late`,
          `set_late_register_hook`), so `abi.rs` FROZEN is no longer the blocker
          the plan expected.
        * ✅ **DONE 2026-08-24: WASIX SOCKETS. `--profile wasix` has real
          egress**, the first profile besides the default that does.
          `runtime/src/net/wasix.rs` over `wasix_32v1`, verified by running
          both directions under stock `wasmer run --net`
          (`e2e/wasixnet_test.go`). ⚠️ This is the NET half and is independent
          of the loader half below, which is still deferred -- a guest that
          does not `dlopen` is unaffected by it.
          * ⚠️ `--profile wasix` now implies `--suspend-via-call` only with
            `--side-out`. A flat link DEFINES `__ecv_unwinding`; measured, a
            flat wasix module has 33 imports and no `env.*`. Implying it
            unconditionally cost a cold object cache for nothing.
          * ⚠️ **`--mapdir` is deprecated in wasmer 7.3.0.** The entries below
            use it. `--volume HOST:GUEST` is current -- the wasmtime order with
            the wasmedge separator.
          * ❗ `profile_exclusion_test` gained a positive control while doing
            this, because it was passing vacuously: `net::loopback` leaves no
            symbols, so "loopback is absent" was true of an archive that had
            compiled it in. See the JOURNAL entry.
        * **Phase 5 `load-wasix` -- SIDE MODULES WORK, SUPERVISOR DEFERRED.**
          ✅ WASIX loads a real raptormark side module (`rc=0`, a lifted guest
          from `link-all --profile wasix --side-out`). Two things were needed:
          elfconv patch 0067 (`--suspend-via-call`), which removes the
          `env.__ecv_unwinding` GLOBAL import that WASIX's linker refuses; and
          `-Wl,--shared-memory` with the `wasm32-wasip1-threads` libc.

          ❌ **DEFERRED by the user 2026-08-24: the SUPERVISOR.** wasmer needs
          the main module to IMPORT `__stack_pointer`, which only a PIC/PIE link
          does, and that needs every object to carry PIC relocations -- including
          the precompiled Rust std. `-Crelocation-model=pic` reaches our crate
          and stops there. A PIC std means nightly + `rust-src` + `-Z build-std`
          (this host has stable only), plus a build-std path through rules_rust.
          ⚠️ Nothing ELSE blocks it: not exception handling, not the threads
          proposal for the lifted objects (`--no-check-features` covers that),
          not `wasixcc`.

          ⚠️ **`runtime/cshim/ecv_globals_pic.c` HAS NO CONSUMER YET.** It is the
          PIC-safe globals shim the supervisor link needs -- a wasm global cannot
          be compiled `-fPIC` at all, so `ecv_globals.c` cannot be in a PIC
          module. It compiles and is correct, but no BUILD target references it,
          which is exactly the "looks like it is doing something" shape this file
          warns about elsewhere. Wire it when the supervisor work resumes, or
          delete it; do not leave it ambiguous.
          ❗ It is ONLY correct alongside patch 0067 -- with the global form,
          `--allow-undefined` resolves the suspend read to zero and the guest
          never suspends, which is a HANG, not a link error. Its header says so.

          ✅ Verified 2026-08-24 that patch 0067 is INERT BY DEFAULT, which
          matters because `patches/*.patch` is globbed by every base build: on a
          base with 0067 applied, a default lift still emits the
          `env.__ecv_unwinding` global import, and only `--suspend-via-call`
          drops it.

      ⚠️ Before extending any of it, read the defect tally in the 2026-08-23
      consolidation entry: six defects, all found by reading, five of them in
      code written and gated green in the same session, every one a claim no
      test could tell from its opposite.

- [ ] **Dynamic TLS for a dlopen'd unit.** `ensure_unit_loaded` REFUSES a unit
      carrying its own `PT_TLS`, because `setup_tls` lays out only the static
      block from `.ecv.tls` at bring-up. Deprioritised on measurement, not
      guess: **0 of postgres:17's 79 extensions and 0 of 3 OpenSSL
      modules/engines carry `PT_TLS`**.
      ✅ **python MEASURED 2026-08-23, and the refusal stands**: 1 of 77
      `lib-dynload` modules carries `PT_TLS`, and it is
      `_ctypes_test.cpython-314-aarch64-linux-gnu.so` -- CPython's own test
      fixture, not something a program dlopens. site-packages: 0 of 0.

      Across all three sets measured: **1 of 159 real plugins**, and that one is
      a test module. Dynamic TLS blocks nothing anyone ships. Revisit only if a
      concrete guest needs it.
      ✅ **RE-VERIFIED 2026-08-25: the refusal is still there and still loud.**
      `context.rs:8704` returns `"the unit has its own TLS, which dynamic loading
      does not support yet"` after a `find_data_section(b".ecv.tls")` hit, with
      the reason stated in the code at `:8690` -- `setup_tls` lays out the STATIC
      block at bring-up and nothing extends it afterwards.
      ⚠️ The refusal is what makes this SAFE to defer: it is a named error, not
      a silent partial load, so a guest that ever does need dynamic TLS says so
      instead of resolving a symbol into a TLS slot nobody allocated.
      ❗ Do NOT confuse this with the `execve` path, which used to inherit this
      same refusal and returned ENOEXEC for every fused DYNAMIC program.
      `sys_execve` goes through `ensure_unit_code` for that reason; the split is
      deliberate and re-merging them would reintroduce a defect no `gcc -static`
      test can see.

- [x] **✅ FIXED 2026-08-26: symbol versioning -- a DEFAULT version now
      outranks a COMPAT one in `globalSymbols`.** The entry below is the
      measurement that led to it and is kept because the numbers are the
      argument. What changed: `globalSymbols` is still first-wins, with one
      exception -- a held definition from a NON-DEFAULT (hidden) version is
      displaced by a later default one. `debug/elf` exposes the hidden bit as
      `VersionIndex.IsHidden()` (Go 1.26), so this reads the ELF rule exactly
      rather than guessing from version strings.

      **Measured effect on the real postgres:17 closure: 18 of the 67 real
      divergences are re-bound**, every one a compat -> default move inside
      libc/libm. All three ACTIVELY wrong ones (`exp`, `fmod`, `log2f`) are
      fixed, `glob` is fixed, and the `totalorder*` family (10 of the 18, a
      genuine glibc 2.31 signature change) is fixed.

      ✅ **Blast radius confirmed on real data, not asserted**: musl closure
      **0** re-bound of 940 divergences, postgres's 79 extensions **0** re-bound
      of 3. Unversioned symbols are treated as default, so on musl and on every
      plugin this degenerates to exactly the old first-wins -- the per-unit
      path is untouched.

      Tests: `internal/fuse/symver_test.go`, six of them, all transcribed from
      `readelf` output rather than derived from a run. **Neutralized twice**:
      reverting to plain first-wins fails five with their intended diagnostics
      while both controls (unversioned first-wins, two-defaults interposition)
      correctly still pass; removing only the ifunc-clear fails exactly the
      ifunc test.

      ❗ **An earlier compat definition may be an ifunc while the default is
      not**, so the fix must CLEAR the ifunc mark when it displaces one --
      otherwise the runtime treats a plain implementation address as a resolver
      to call, which returns a pointer and does nothing. That is the one way
      this change could have introduced a new silent defect; it has its own test
      and its own neutralization.

      ⚠️ **COST, and it is not small: this changes the fused ELF, so every
      cached translated object for a GLIBC image is invalidated.** musl images
      are byte-identical (`globalSymbols` output is unchanged there), so their
      objects survive. No E2E run has been done against this -- the gates run
      were Go, and Bazel 14/14.

      Gates: `gofmt`/`go build`/`go vet`/`go test` across both module patterns,
      `raptormark bazel test //...` 14/14.

- [ ] **(the measurement behind the fix above) Symbol versioning is discarded,
      and `glob` is the proof it matters.**
      Added 2026-08-26 -- the dynamic-side-module plan flagged this as out of
      scope but recommended an entry, and nothing had one.

      `fuse.go:463` loads symbols with `f.DynamicSymbols()`, which returns BARE
      names, and `.gnu.version*` is never read. `globalSymbols` then keys on the
      name alone and takes the first definition (`fuse.go`, "first definition
      wins"). So `foo@GLIBC_2.17` and `foo@GLIBC_2.27` collapse into one entry
      and whichever the `.dynsym` lists first is bound, image-wide.

      ✅ **MEASURED, and it is far narrower than the framing suggested.**
      `.agents-workspace/drivers/symver` mirrors `globalSymbols`' admission rule
      exactly and was run over aarch64 glibc, libcrypto.so.3 and libruby.so.3.4:
      10,575 distinct defined names, **196** defined at more than one version --
      but only **5** of those 196 resolve to more than one ADDRESS. The other
      191 are one implementation wearing two version labels (glibc's 2.17/2.34
      re-versioning), where discarding the version costs nothing.

      The five that genuinely diverge, first-wins choice shown:
      | name | bound | other | correct? |
      |---|---|---|---|
      | `fmemopen` | 2.22 | 2.17 | ✅ current |
      | `glob` | **2.17** | 2.27 | ❌ **compat** |
      | `glob64` | 2.27 | 2.17 | ✅ current |
      | `pthread_kill` | 2.34 | 2.17 | ✅ current |
      | `quick_exit` | 2.24 | 2.17 | ✅ current |

      ❗ `glob` and `glob64` are ALIASES -- the same two addresses -- and
      first-wins gives them DIFFERENT implementations in one image. A guest
      calling `glob()` gets glibc's pre-2.27 compat entry while `glob64()` gets
      the current one. glibc 2.27 changed `gl_readdir`'s return layout, so a
      caller passing `GLOB_ALTDIRFUNC` reads the wrong struct; without that flag
      the two are believed equivalent, which is why nothing has noticed.

      ✅ **RE-MEASURED THE SAME DAY over the REAL postgres:17 closure** -- 89
      objects (the `postgres` binary's transitive `DT_NEEDED` set plus all 79
      extensions and each of their own closures), 76,458 distinct defined names:

      | | count |
      |---|---|
      | defined more than once | 290 |
      | ...at more than one VERSION | 242 |
      | ...at more than one ADDRESS | 99 |
      | &nbsp;&nbsp;of which `SHN_ABS` version tags | 21 |
      | &nbsp;&nbsp;of which C++ vague linkage | 8 |
      | &nbsp;&nbsp;of which linker bounds | 3 |
      | &nbsp;&nbsp;**REAL** | **67** |

      ❗ **The raw 99 would have overstated it ~1.5x, and an earlier bad run
      overstated it 1000x.** `GLIBC_2.17` and `OPENSSL_3.0.0` are real `.dynsym`
      entries (`SHN_ABS` version nodes) and `globalSymbols` admits them, since it
      filters only `SHN_UNDEF` -- harmless, but not evidence. C++ vague symbols
      SHOULD collapse to one; that is the ODR, not a defect. The probe now
      categorises rather than lumping.

      Of the 67 real: **64 are libc/libm version pairs and 3 are postgres**.
      **32 of the 64 bind `GLIBC_2.17`, the OLDEST compat implementation** --
      `exp`, `expf`, `exp10`, `fmod`, `hypot`, `log2f`, `modf`, `frexp`,
      `ldexp`, `scalbn`, `copysign`, `finite`, and `glob`.

      ❗ **Narrowed once more, and this is the number that matters: THREE are
      ACTIVELY mis-bound.** A wrong binding only costs something if some object
      actually references the name across an object boundary at a DIFFERENT
      version. Cross-checking every `UND` reference in the closure against what
      first-wins binds:

      | symbol | referenced as | bound to | by |
      |---|---|---|---|
      | `exp` | `GLIBC_2.29` | **`GLIBC_2.17`** | `postgres`, `libLLVM`, `libz3` |
      | `fmod` | `GLIBC_2.38` | **`GLIBC_2.17`** | `postgres`, `libLLVM` |
      | `log2f` | `GLIBC_2.27` | **`GLIBC_2.17`** | `libLLVM` |

      ⚠️ `postgres` itself imports `exp@GLIBC_2.29` and `fmod@GLIBC_2.38` and
      gets glibc's SVID-era compat wrapper instead. **The defect is live in the
      shipping postgres pipeline, not hypothetical.** The other 29 that bind
      2.17 have no cross-object reference wanting a newer version, and `glob`
      is among them -- latent here, though it is still the cleanest test
      subject because `glob`/`glob64` are aliases that get different answers.

      ⚠️ **Severity is still LOW, and the activation does not change that.** All
      three are libm pairs where 2.17 is the SVID wrapper (`_LIB_VERSION`,
      `matherr`) around the same kernel. They agree on ordinary inputs and
      differ at edge cases -- overflow/underflow errno. Do NOT upgrade this to
      "wrong math results"; what is provably wrong is *which* implementation
      runs, not the value it returns for normal arguments.

      ✅ **The 3 postgres ones are ALREADY FIXED** by the per-unit path
      (`FuseWithUnits`), which is what it was built for: `Pg_magic_func` has
      **79** definitions, `_PG_init` **15**, `_PG_output_plugin_init` **2**.
      Under the flat `Options.Extra` path first-wins binds `amcheck.so`'s magic
      and `auth_delay.so`'s init for every extension.

      ⚠️ **CORRECTION to this file's own earlier framing.** The dlopen entry
      says every extension defines "`Pg_magic_func`, `_PG_init` and
      `pg_finfo_*`". The first two collide; **`pg_finfo_*` does not and never
      did** -- those names carry the SQL function name (`pg_finfo_hstore_in`),
      so they are unique per extension. Exactly 3 names collide, not a class.

      ✅ **musl: measured, and it cannot be affected.** The nginx:alpine base
      closure (3 objects, 7,498 names) has **0** versioned names and **0** real
      divergences. musl has no symbol versioning at all.

      ❗ **Methodological, and it cost a wrong number once.** Running the probe
      over every `.so` in the lib directory (218 objects) reported **104,978**
      divergences -- it loads mutually-exclusive alternatives no fuse would ever
      see together. Always feed it a real `DT_NEEDED` closure.

      What a fix costs: Go's `debug/elf` populates `Symbol.Version` and
      `Symbol.Library` on `DynamicSymbols()`, so the data is already in hand --
      this is a keying change in `globalSymbols`, not a parser. The hard half is
      deciding what an UNVERSIONED reference binds to (the default version, per
      `.gnu.version_d`'s `VER_FLG_BASE`), which means reading `.gnu.version_d`
      off the INPUT object.

      ⚠️ Do NOT confuse that with `emit` dropping the version sections from the
      OUTPUT image (`fuse.go:1032-1039`). That drop is deliberate and must stay:
      BFD, which elflift loads through, rejects an ELF carrying dynamic-linking
      metadata with "does not look like an executable file". The input's
      `.gnu.version_d` is fully available to the fuser and is simply never
      consulted -- nothing has to be preserved in the output to fix this.

      A regression test has an unusually good subject: assert `glob` and
      `glob64` bind the SAME address in a fused glibc image. That fails today
      for the right reason and cannot pass vacuously.

- [ ] **No weak/strong or visibility precedence in `globalSymbols` -- REAL but
      with no measured witness.** Added 2026-08-26, same probe as above.

      `globalSymbols` (`fuse.go`) reads neither `ST_BIND` nor `st_other`: it
      admits any defined, named symbol and takes the first. So an earlier WEAK
      definition beats a later STRONG one, and `STV_HIDDEN` is ignored. Note
      `UnitExports` (`unit.go`) DOES filter `STB_LOCAL`, so the two disagree
      about what a unit exports versus what the image binds.

      ⚠️ **The probe found ZERO cases** of weak-first-beats-later-global across
      those same 10,575 names. That is a negative result, not a clean bill: the
      set had one libc and no closure with duplicate cross-object definitions,
      which is exactly where this would bite. ❗ Per `AGENTS.md`, "these appear
      twice" is not sufficient reason to act -- what decides is whether anything
      BEHAVES differently, and right now nothing measured does.

      ✅ **That zero is NEUTRALIZED, which is the only reason to believe it.**
      A witness pair (`weak collide` in one .so, `strong collide` in another)
      makes the counter read 1 in weak-first order and 0 in strong-first order.
      ❗ The first version of the probe read **0 on that witness too**: it
      compared the raw `s.Value`, and two identically-shaped .so files both put
      `collide` at 0x578. `globalSymbols` stores `o.addr(s.Value)`, so
      definitions in different objects can never share an address -- the probe
      now keys on (object, value). Had this not been neutralized, "0 weak/strong
      cases" would have been reported from a branch that could not fire.

      ✅ **RE-MEASURED THE SAME DAY, and the answer is settled: still ZERO.**
      This entry asked for the postgres closure and a musl fixture. Both were
      run:

      | closure | objects | names | weak-first cases |
      |---|---|---|---|
      | glibc + libcrypto + libruby | 3 | 10,575 | **0** |
      | postgres:17, real `DT_NEEDED` + 79 extensions | 89 | 76,458 | **0** |
      | nginx:alpine (musl) base | 3 | 7,498 | **0** |
      | nginx:alpine + all 14 module variants | 18 | 19,287 | **0** |

      ❗ **RECOMMENDATION: do not implement precedence. Record the omission as
      deliberate.** Four closures across two libcs, 113,000 name-definitions,
      zero witnesses, and a detector proven live by a witness pair. Adding an
      untriggered precedence rule to `globalSymbols` would be code with no
      test that can distinguish it from its opposite -- which is the failure
      mode this file catalogues, not a fix for one.

      ⚠️ The one thing that WOULD reopen it: an object set where the same name
      is defined weak in one object and strong in another. Nothing shipped has
      produced one. If a future guest does, the probe reports it by name.

      ❌ **RETRACTED, same day: the `STB_LOCAL` "inconsistency" is CORRECT
      behaviour and must not be reconciled.** This entry twice noted that
      `globalSymbols` and `UnitExports` disagree about `STB_LOCAL` -- the latter
      filters it, the former does not -- and called it a real inconsistency.
      It is not. They read DIFFERENT TABLES, and each does the right thing for
      the table it reads:

      * `globalSymbols` reads `.dynsym` (`f.DynamicSymbols()`). Measured across
        all 89 objects of the postgres closure: **zero** named defined
        `STB_LOCAL` entries. The only defined locals are section symbols, whose
        `st_name` is 0, so `debug/elf` reports `Name == ""` and the existing
        `s.Name == ""` filter already drops them. A `STB_LOCAL` filter there
        would be dead code.
      * `UnitExports` reads the SYNTHESIZED `.symtab`, which `emit`
        deliberately fills with named `STB_LOCAL` `_ecv_fde_<addr>` function
        boundary symbols recovered from `.eh_frame` (`fuse.go:1213`).
        `bash-glibc.fused` carries **2,110** of them. Without the filter every
        one would be reported as an export of the unit.

      ❗ **Worth keeping as a near-miss.** "These two functions disagree" was
      written down twice as though it were a defect, and acting on it -- making
      them agree -- would have either added dead code to `globalSymbols` or
      broken `UnitExports` by leaking 2,110 boundary symbols as exports. The
      rule that caught it is the one in `AGENTS.md`: what decides is whether
      anything BEHAVES differently, not whether two sites look inconsistent.


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

- [x] **✅ REBALANCED 2026-08-25 on the user's instruction. The mmap window went
      96 -> 160 MiB and the arena did not grow.** Two constants:
      `BRK_END_VMA` and `MMAP_START_VMA`, both `0x1000_0000` -> `0x0C00_0000`.

      | region | before | after |
      |---|---|---|
      | brk | 96 MiB | **32 MiB** |
      | mmap window (private + shared) | 96 MiB | **160 MiB** |

      ❗ **The datum that looked like a reason for caution was the argument for
      going further.** `arena.rs`'s own doc records a real initdb backend with
      its break ~88 MiB into brk, which read as "postgres needs the 96 MiB".
      Reading `sys.rs` settled it the other way: `NR_BRK` returns the break
      UNCHANGED when a request leaves the region -- Linux's failure convention --
      glibc turns that into ENOMEM and malloc switches to mmap. That backend
      already asks for **~190 MiB** and already takes the fallback. Its demand
      could not be met by ANY brk size, so brk size is nearly irrelevant to it
      and mmap size is exactly what it needs.

      **Evidence**: 104 guest runs, max brk high-water **1,164 KiB of 96 MiB**,
      103 of them at 136 KiB. 32 MiB is 27x the largest ever measured, and a
      guest exceeding it lands in the region this grew by 64 MiB.

      ✅ Guarded: `the_arena_layout_is_consistent` asserts
      `BRK_END_VMA == MMAP_START_VMA` -- neutralized by moving one constant
      alone, which fails with "the mmap window must start where brk ends, with
      no gap to lose", so a half-change cannot ship.
      ✅ E2E **137 pass / 0 fail / 19 skip** on `raptormark-builder:rebal`,
      identical to the pre-change run, including
      `TestSharedFileMappingsDoNotPinTheWindowUnderEcvisor` -- the test that
      peaked at 88 MiB in this very window.

      ❗ **CORRECTION, same day: "YJIT's 128 MiB now FITS" was WRONG, and
      measuring it corrected the entry's own framing too.** Tested against the
      rebalanced module with the `p-env.img` probe (`RUBY_YJIT_ENABLE=1`, the
      wall-2 path -- argv cannot arm YJIT at all, see the decoder gap below).
      It still fails, and the instrument says exactly why:

      ```
      mmap region exhausted (want 134217728, bump 0xf020000, 0 hole(s),
                             shm_top 0x16000000) -> ENOMEM
      address budget -> brk 1728 KiB of 32 MiB | private mmap 49280 KiB
      ```

      | | |
      |---|---|
      | window | 160 MiB (`0x0C000000`..`0x16000000`) |
      | **ruby consumes before YJIT asks** | **48 MiB**, 0 holes, all live |
      | contiguous left | 111.9 MiB |
      | YJIT wants | 128 MiB |
      | **short by** | **16.1 MiB** |

      ❗ **The real requirement was never 128 MiB. It is 176.** This entry's own
      table says "YJIT | 128 MiB into a 96 MiB window", which reads as though
      128 were the bar; ruby's 48 MiB of startup mappings is not in that number
      and never was. Any future sizing argument has to use 176.

      ✅ The rebalance still moved it: short by **80 MiB** before, **16 MiB**
      now. ❌ It did not clear it, and the claim that it would was made from
      arithmetic rather than a run -- exactly what this file says to refute by
      removal instead.

      ⚠️ **What is NOT settled.** This relieves the starvation; it does not make
      reservations lazy. Ruby's shape cache still asks for 402,653,184 bytes and
      still cannot be served -- 160 MiB is not 384.

      ---

      ### ❗ THE THREE OPTIONS, COSTED. Investigated 2026-08-26 in answer to
      "does introducing a memory mapper (more indirection) help?"

      The entry's option 3 ("make reservations lazy") is a MAPPER. It is the only
      one that fixes all four faces -- but "mapper" conflates two changes with
      costs two orders of magnitude apart, and the cheap one was never listed.

      **1. Full mapper: translate every guest access.** ✅ Fixes everything. A
      reservation would stop costing what an allocation costs, and the entry's
      own measurement shows the prize: ruby's shape cache is **RSS 28 kB against
      a 384 MiB mapping, 14,000:1**.
      ❌ **It costs the hot path, and there is no seam to hang it on.** A
      translation point EXISTS -- `TranslateVMA` in
      `third_party/elfconv/runtime/Runtime.cpp` -- but only under
      `MEMORY_INSTRUMENT`. In a shipping build the memory intrinsics abort:
      `VmIntrinsics.cpp` says in as many words "`__remill_(read | write)_memory_*`
      functions are not used for the optimization" and
      `elfconv_runtime_error("... must not be called!")`. Lifted code emits
      `arena_ptr + addr` inline, folded into the wasm load/store offset.
      `__ecv_wild_store` having NO CALLER anywhere is the same fact from the
      other side: there is no per-access check to extend.
      ❌ **And no cheap partial version.** The obvious dodge -- virtualise only
      `PROT_NONE` reservations, fault pages in on first touch -- needs
      first-touch detection, and wasm gives a guest module no page-fault hook. A
      store past the arena traps at the ENGINE, uncatchable from inside.
      Cost: **one translation per guest load/store**, on a module already ~35x
      slower than AOT under the shipping shim.

      **2. Better placement inside the window.** Already exists and is not the
      problem: `Arena.mmap_free` reuses and coalesces holes. It cannot cross a
      region boundary, and the YJIT failure reports `0 hole(s)` -- nothing to
      reuse.

      **3. ⭐ DYNAMIC REGION BOUNDARIES -- the option nobody listed, and it needs
      NO indirection.** Verified 2026-08-26: `MMAP_START_VMA` / `BRK_END_VMA`
      appear NOWHERE outside `runtime/` -- not in the lifter, the fuser, the
      linker or the image. They are pure ecvisor POLICY: where it starts handing
      out addresses when the guest calls `mmap`. Everything stays identity-mapped,
      so no access changes.
      ⚠️ **`BRK_START_VMA` is the opposite and must not be confused with them**:
      `internal/fuse/layout.go:45` uses it as the ceiling for the fused closure
      layout, guarded cross-language by `TestBrkStartMatchesTheRuntime`. It IS
      baked into the image and cannot move at run time.
      Cost: **one decision per boot**, zero per access. A guest that needs a big
      contiguous reservation could be given a window sized for it; one that does
      not keeps its brk.

      ### ❗ OPTION 4: COPY-AND-FIX-REFERENCES -- and the fix set is EMPTY

      Explored 2026-08-26 on request. "Move the allocation and repair every
      pointer to it" fails for GUEST allocations for the usual reason: you cannot
      enumerate references in a C heap, where a pointer may be in a register,
      computed, or stored as an integer. That is precise GC without type
      information, and it is undecidable.

      **But applied to the ARENA ITSELF it is nearly free, because all three
      classes of reference turn out to need no fixing:**

      | reference class | why it needs no fix |
      |---|---|
      | guest pointers | **They are OFFSETS.** Under the identity map a guest pointer IS an arena offset; moving the backing changes nothing it can observe. |
      | frozen host pointers in suspended stacks | **There are none.** `entry.rs:216`: "Cooperative scheduler (full-replay; **no asyncify**, no EH)". Suspension is a plain RETURN since `patches/0026-forkemu-suspend-by-early-return`, so the wasm stack is fully unwound at every suspension point. |
      | cached host base pointers | **There are none.** Every consumer re-fetches: `entry.rs:158`, `entry.rs:344` (inside the leg loop), `context.rs:3793`, `:4112`, `:8742`. `State` is a `Box<State>` beside the arena, not in it. |

      ⚠️ **`arena.rs:766-769` says the opposite and is STALE**: "The live arena
      buffer address is fixed, so ... any asyncify-saved stack (which baked in
      that fixed base) stays valid." That cites a mechanism the tree removed.
      Anyone costing this option from the comment would conclude the arena cannot
      move, which is the reverse of what the code now supports. ❗ Same class as
      every other stale claim found this week, and it sits on the exact function
      an implementer would read first.

      **So "grow the arena" is much cheaper than this entry assumes.** It was
      listed as costing "every guest's linear memory", which is true, and read as
      also requiring pointer surgery, which it does not. The arena could be
      reallocated AT A LEG BOUNDARY -- including **on demand**, growing only when
      a reservation does not fit, so a guest that never asks never pays.
      ❌ **The real cost is the copy, and it is not small.** A `Vec` realloc is
      allocate-new + memcpy + free-old, so 384 -> 512 MiB peaks around 900 MiB
      transiently -- and wasm `memory.grow` NEVER SHRINKS, so that peak is
      permanent for the process. Whether the allocator can extend in place
      instead is unmeasured and is the thing to measure first.

      ### OPTION 5: A CUSTOM ALLOCATOR BACKING THE ARENA -- ❌ SET ASIDE

      ❌ **Dropped by the user 2026-08-26.** Kept because the analysis is
      costed and the small-module RSS experiment below is still the cheapest
      way to settle whether option 3 is already free -- but nobody is working
      on this, and it should not be read as a live recommendation.

      Raised 2026-08-26. It is the right instinct and it makes option 4 cheap,
      for three reasons -- and it does NOT on its own fix the failure.

      **1. The `Vec` buys nothing.** `arena.rs:754` is
      `bytes: vec![0u8; MEMORY_ARENA_SIZE]` on wasi-libc's dlmalloc. The region
      is fixed-size, indexed by offset, never pushed to. It is a raw region
      wearing a `Vec`'s clothes, and the `Vec` contract is what forces the next
      two costs.

      **2. Growth today means realloc + memcpy.** 384 -> 512 MiB peaks near
      900 MiB transiently, and wasm `memory.grow` NEVER SHRINKS, so that peak is
      permanent. An allocator owning the TOP of linear memory could extend the
      arena with `memory.grow` alone -- **zero copy**. That is what would make
      option 4's on-demand growth actually affordable.

      **3. `vec![0u8; N]` may be FORCE-COMMITTING 384 MiB at boot.** wasm
      guarantees freshly grown pages are zero, so a `calloc` that knows its
      memory is fresh can skip the memset. Whether wasi-libc's dlmalloc does is
      **unverified**. If it does not, every guest pays a 384 MiB memset at
      startup and loses whatever lazy backing the host would have given.
      ❗ If it DOES skip, then a much larger arena is close to free until
      touched -- which is the "lazy reservation" this entry calls option 3, got
      without any mapper at all.

      **Measured 2026-08-26, and it does NOT settle point 3.** Peak RSS of
      wasmedge running the rebalanced ruby module: **1,606 MiB against 443 MiB of
      linear memory**, reached within ~2 s and then FLAT for the rest of the run.
      Front-loaded, nothing accrues during execution -- consistent with the arena
      being committed up front, but not proof, because a 51 MiB module's
      interpreter structures dominate and cannot be separated from outside.
      **The clean experiment is a SMALL module**: same 384 MiB arena, tiny
      module. RSS ~450 MiB means the arena is committed; ~50 MiB means it is
      lazy. One small lift, and it decides whether option 3 is already available
      for free.

      ⚠️ **What an allocator does NOT fix.** YJIT failed on ecvisor's own
      BOOKKEEPING -- `mmap region exhausted (want 134217728, bump 0xf020000,
      shm_top 0x16000000)` -- an address-range refusal, not a memory shortage.
      A better allocator makes a bigger arena cheap; it does not by itself widen
      the range. **Both are needed**: more address space AND cheap backing. The
      allocator is what stops the first from costing what it currently would.

      **The comparison in one line**: a mapper prices the fix per MEMORY ACCESS,
      dynamic boundaries price it per MMAP. Same symptom, ~10^9 difference in how
      often you pay.
      ❌ Neither is done. Option 3 is a real design (who chooses the sizes -- a
      manifest? the guest's own first big request? -- and what happens when the
      choice is wrong), but it is the first thing in this decision with a costed
      alternative to touching the hot path. The other
      three options remain available and are now cheaper to evaluate against a
      window that is no longer the binding constraint.
      ⚠️ Real postgres brk/mmap is still unmeasured; the instrumentation to do it
      is in the tree (`diag::note_address_use`, under `RAPTORMARK_ECV_DEBUG`).

      Original entry follows.

- [ ] **(original framing) ⭐ DECIDE what ecvisor does about SPECULATIVE RESERVATIONS. This is the
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

      ✅ **ARITHMETIC RE-CHECKED 2026-08-25, independently of the entry.** Every
      number in the table above holds, including the one that matters most:

      ```
      MEMORY_ARENA_SIZE 384 MiB = 402,653,184
      ruby (0x80000 * 32) * 24  = 402,653,184   <- EQUAL, and unrelated
      MMAP_END - MMAP_START     = 96 MiB        <- YJIT's 128 MiB cannot fit
      ```

      ❗ **The coincidence is REAL, so the trap is real.** Two independently
      derived constants land on the same 402,653,184, and nothing in ruby reads
      it from us. An investigator who checks only that the numbers match will
      conclude ecvisor caused it, and be wrong. Recomputing ruby's expression
      from its own headers is what distinguishes them, and it takes one line.
      ❗ **A FIFTH OPTION, and it may be free. Found 2026-08-25 by mapping the
      arena's actual budget rather than re-reading the entry.** The four options
      above are grow / split / lazy / accept. None of them notices that **the
      starved window has a 96 MiB NEIGHBOUR**:

      | region | range | MiB |
      |---|---|---|
      | image + fused | `0x400000`-`0x0A000000` | 156 |
      | **brk heap** | `0x0A000000`-`0x10000000` | **96** |
      | mmap window (shared AND private) | `0x10000000`-`0x16000000` | **96** |
      | stack | `0x16000000`-`0x18000000` | 32 |
      | **arena total** | | **384** |

      **REBALANCE rather than grow.** Lowering `MMAP_START_VMA` into unused brk
      space costs no linear memory, no bounded-snapshot headroom and no
      memory-model change -- the three things that make the other options
      expensive. It is a constant, not a design.
      ⚠️ It also does not need the shared/private split to be decided first: more
      total window relieves the starvation whichever way they divide it.

      ❗ **GATED ON ONE MEASUREMENT THAT DOES NOT EXIST: real brk high-water.**
      `Arena.brk_cur` tracks it and NOTHING reports it -- `grep brk` over
      `diag.rs` returns nothing. The plausibility argument is that glibc malloc
      sends large allocations to mmap and only small ones to brk (postgres's
      collation ladder went through mmap, per the trace in the entry below), so
      96 MiB of brk is likely far more than any guest uses. ❌ That is an
      argument, not a number, and this file's own rule is to refute by removal
      rather than by reasoning.
      ✅ **MEASURED 2026-08-25. brk is 99% DEAD; the mmap window is 92% USED.**
      Added three high-water counters (`diag::note_address_use`, reported at
      process exit under `RAPTORMARK_ECV_DEBUG`), side-built
      `raptormark-builder:brkmeas` onto the existing patched base with labels
      verbatim, and harvested the line from **104 guest runs** across the fast
      e2e suite:

      | region | size | MAX observed | used |
      |---|---|---|---|
      | brk | 96 MiB | **1,164 KiB** | **1.2%** |
      | private mmap | 96 MiB (shared with below) | **90,112 KiB** | **91.7%** |
      | shared (`MAP_SHARED`) | same 96 MiB | 24,576 KiB | -- |

      brk distribution is almost flat: **103 of 104 runs at 136 KiB**, four at
      192 KiB, one outlier at 1,164 KiB. The plausibility argument -- glibc
      malloc sends large allocations to mmap and only small ones to brk -- is
      now a number.

      ⚠️ **The 88 MiB private-mmap peak is
      `TestSharedFileMappingsDoNotPinTheWindowUnderEcvisor`**, a test written to
      STRESS that window. It is a deliberate worst case, not a natural workload
      -- but it is the right worst case, because it is the region under
      discussion, and it came within 8 MiB of the ceiling.

      ❌ **What this does NOT measure: real postgres.** The suite's
      postgres-derived fixtures run small custom guests, not the postmaster that
      hit ENOMEM. A brk figure of ~1 MiB across 104 runs is strong evidence the
      region is over-provisioned for these workloads; it is not proof for all.
      The next measurement, if this option is taken, is real postgres.

      **So the rebalance option looks live**: moving `MMAP_START_VMA` down even
      to `0x0C000000` would take 32 MiB from a region using 1.2% of its 96 MiB
      and give it to one at 91.7%, as a one-constant change with no arena growth
      and no memory-model change.

      ⚠️ Found while measuring, and unrelated to the decision: the
      `RAPTORMARK_ECV_DEBUG` run made
      `TestUndecodedCensusFiresAndStaysOff/handler*` FAIL falsely. Its
      fatal-path detector is `strings.Contains(out, "undecoded instruction at")`,
      and debug output contains that phrase without the fatal path having run --
      a check that cannot distinguish two states. Latent (debug is not the
      default) but real.

      ⚠️ Still a DECISION and still not swept: every option is a trade nobody has
      been asked to make. Raised to the user 2026-08-25 as the one item in the
      sweep that needs an answer rather than a check.

- [ ] **The shared window and the private mmap arena are the same 96 MiB.**
      (Still 96 MiB as of 2026-08-12: the arena reverted to 384 MiB, so
      MMAP_START 0x10000000 .. MMAP_END 0x16000000 is unchanged from when this
      was written.)
      A large `MAP_SHARED` therefore starves malloc's mmap fallback: with
      `shared_buffers` at 76 MiB the private side is ~20 MiB. Splitting them, or
      at least sizing the shared window from a policy rather than letting it eat
      the window top-down, is what makes the item above tractable without simply
      buying more arena. Verified 2026-08-11.
      ✅ **RE-VERIFIED 2026-08-25 against `runtime/src/arena.rs`, unchanged.**
      `MEMORY_ARENA_SIZE = 384 MiB` (`:37`), `MMAP_START_VMA = 0x1000_0000` and
      `MMAP_END_VMA = 0x1600_0000` (`:40-41`) -- a 0x600_0000 = 96 MiB span that
      `ShmWindow` carves DOWNWARD from `MMAP_END_VMA` while `Arena::mmap_cur`
      bumps UPWARD from `MMAP_START_VMA` into the same range. The starvation is
      structural rather than incidental: the two allocators share one span by
      construction and neither reserves anything for the other.
- [ ] **`fmov <Vd>.2S, #imm` is unlifted.** Patch 0010 covers only the 2D
      vector-immediate form (`FMOV_ASIMDIMM_D2_D`); the S form is a decode stub,
      and `fmov v1.2s, #1.0` is what a compiler emits for `v2f x = {1.0f,1.0f}`.
      Found because it killed a test guest before any of its checks ran.
      Verified 2026-08-11.
      ✅ **RE-VERIFIED 2026-08-25 in the patch itself.**
      `patches/0010-fmov-vector-immediate-d2.patch` is the only patch naming
      `FMOV_ASIMDIMM`, and its own diff context shows
      `TryDecodeFMOV_ASIMDIMM_S_S` left as the generated `return false` while
      only `_D2_D` gained a real decoder. The patch name says `d2` for that
      reason; the S form is untouched and still a stub.
- [ ] **A by-element 2S multiply cannot express lanes 2 and 3.** The index is
      H:L regardless of Q, so `fmul v0.2s, v1.2s, v2.s[3]` is legal, but the
      semantics take both operands as the same arrangement and a 64-bit view has
      only two lanes. Patch 0045 makes the decoder REFUSE those rather than read
      out of range (loud beats silently wrong), which means such an instruction
      now stops the module. Doing it properly needs the element operand read as
      the full 128-bit register and new ISELs taking (v2f, v4f, index).
      Verified 2026-08-11.
- [x] **✅ CLOSED 2026-08-24. `fuse.Options.Extra` now generalises.** Everything
      this entry asked for exists: per-module namespacing in the fuser
      (`.ecv.dlsyms` is emitted PER UNIT by `fuse.FuseWithUnits`) and a
      handle-aware `dlsym` in the runtime. `e2e/pgdlopen_test.go` proves two
      extensions defining the same `Pg_magic_func` resolve to DIFFERENT
      addresses through different handles, and `dlopen` of an absent plugin now
      returns NULL with a real `dlerror` instead of a sentinel handle. The
      original text is kept below for the trail.

      **(original framing, now false)** `fuse.Options.Extra` handles ONE
      dlopen'd plugin, and does not generalise. Added 2026-08-11 so `dict_snowball.so` reaches the closure --
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
      ✅ **RE-VERIFIED 2026-08-25, and it is one line.**
      `context.rs:4461` is `self.shm_window.reserve(len, self.arena.mmap_cur)` --
      `self.arena` is the RUNNING process's, so the floor is exactly the claim.
      `ShmWindow::reserve` then refuses only `at < floor` (`arena.rs:318`), which
      is the check that would have to consult every process's bump instead.
      Still not observed, and the fix is still one that costs window space for
      everybody.
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

      ✅ **THE 2026-08-21 FIX RE-VERIFIED 2026-08-25.** `SharedSeg::unmap_is_whole`
      is at `arena.rs:194` and its host tests at `:2647-2655` assert all three
      arms -- exact length whole, over-length whole, short NOT whole -- so the
      head case really does join the tail case and leak rather than release.
      The leak itself remains open and the three-structure argument above is
      unchanged.
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
      ✅ **RE-VERIFIED 2026-08-25 with line numbers, so the next check is a
      read.** `__remill_error` is `intrinsics.rs:277` and ends in `fatal!` at
      `:290`; `__ecv_warning` is `:294` and posts SIGILL with a documented
      fallback when no handler is installed. They are 17 lines apart, not ten,
      and otherwise exactly as described.
      ⚠️ **The caution is well-founded and should not be sanded off.**
      `__remill_error`'s own comment (`:284-288`) records the incident that made
      it loud, and `__ecv_warning`'s records why ITS site was different --
      PostgreSQL probes for ARMv8 CRC32 by EXECUTING the instruction under a
      SIGILL handler, so aborting removed a recovery the guest had already
      arranged. That asymmetry is the argument: `__ecv_warning`'s guest WANTED
      the signal, and nothing has shown a guest that wants SIGTRAP from `brk`.
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

      ✅ **UPDATE 2026-08-24: a THIRD backend can express it, which narrows the
      title again.** `net-wasix` maps `IPPROTO_TCP`/`TCP_NODELAY` onto WASIX's
      `Sockoption::NoDelay` through `sock_set_opt_flag`
      (`runtime/src/net/wasix.rs::wx_sockopt`), so `--profile wasix` is the
      first profile with BOTH real egress and a working `TCP_NODELAY`. That
      makes the shipping-profile gap measurable at last: the same nginx
      workload can now be run on wasmedge (option dropped, and LOGGED as
      dropped) and on wasmer (option applied), which is the measurement this
      entry has been asking for since 2026-08-09 and could not previously get.
      ⚠️ It does not CLOSE the entry -- the shipping profile is still WasmEdge
      and still inert.

- [ ] **(original framing, now backend-specific) `TCP_NODELAY` cannot be expressed through WasmEdge's socket options.**
      There is no TCP option level, so nginx's `tcp_nodelay on` is silently
      inert. Unmeasured: at ~10 ms per request Nagle is not obviously biting, but
      it would show on keepalive request pipelining. Needs either a WasmEdge-side
      extension or a measurement showing it does not matter. Verified 2026-08-09.

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

- [ ] **⚠️ PARTLY CLOSED 2026-08-24: the PLUGIN half is fixed, the LIBRARY half
      is not.** Plugins no longer consume `libAlign`: they are packed in their
      own band above the library band at their own `PT_LOAD` alignment (64 KiB),
      so postgres's 79 extensions cost ~12.6 MiB rather than 158 MiB. What
      remains is this entry's actual subject -- **95 LIBRARIES at 2 MiB each**,
      which exhausts the region on alignment padding regardless of plugins. The
      fallback is still correct and still loud (`Result.SharedLayout`, and
      `raptormark build` prints it).

      **(original framing)** A plugin-heavy closure loses the shared library
      layout. With 95
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

      ✅ **MOSTLY CLOSED 2026-08-23 by the plugin band.** Full entry in
      `JOURNAL.md`, "Phase 1a: the plugin band". The measurement this asks for
      was done and the 2 MiB alignment is **not** load-bearing: all 2,114 real
      aarch64 shared objects sampled, and all 79 postgres extensions, report a
      maximum `PT_LOAD` `p_align` of `0x10000`. `Options.Extra` objects now go in
      their own band above the libraries, at their own alignment.

      The third option in the list above is what was taken, and placing the band
      ABOVE rather than below is deliberate: every library base stays
      byte-identical, so no cached object or partition is spent.
      postgres:17 + initdb + **78** extensions now plan SHARED, top `0x8f80010`,
      89.7% of the region.

      ✅ **THE EXCLUSION LIST IS BUILT. Re-verified 2026-08-25** against
      `internal/image/plugins.go`, not assumed. It excludes by **dependency, not
      by name** (`jitSonamePrefix = "libLLVM"`, `:62`): `llvmjit.so` is the only
      one of the 79 that names libLLVM, so a name check would work today and
      silently stop working for the next image, whereas "linked against LLVM's
      codegen" IS the property. Surfaced as `ExcludedPlugin{Guest, Reason}` and
      printed by `pipeline.Build` (`build.go:139`), so an exclusion is visible
      rather than inferred from a count.
      ⚠️ The entry above asked for `Report.SkippedExtras`; that name was
      deliberately NOT used. `build.go:97` records why -- a `Skipped` field
      existed briefly, was always nil, and its absence is the accurate statement
      because `fuse.Fuse` ERRORS on an unsatisfiable plugin rather than skipping
      it. An always-empty field claiming otherwise is the "declared field
      nothing reads" shape.
      Discovery also excludes anything unreadable as an aarch64 shared object
      WITH A REASON rather than dropping it, which exists because the first
      version reported "discovered 0 plugin(s), excluded 0" on a rootfs holding
      79 -- a clean, plausible, wrong answer with no diagnostic.

      ⚠️ **WHAT IS STILL OPEN, and it is the only part left: HEADROOM.** With
      `llvmjit.so` excluded, postgres + initdb + 78 extensions plan SHARED at
      **89.7%** of the 156 MiB fused region. That is a pass with ~16 MiB to
      spare, and the next large closure is what spends it. "Grow the region" is
      now the live option -- the other two (shrink `libAlign`, pack plugins
      densely) have both been taken already and cannot be taken again.
      ❗ The failure mode when it does overflow is the loud, correct fallback to
      per-image packing, so this is a COST cliff, not a correctness one: the
      build still succeeds and silently loses every cross-program library share.

      ❌ **RE-MEASURED 2026-08-26, AND THE CLIFF HAS ALREADY BEEN CROSSED.
      `raptormark build postgres:17` does NOT plan a shared layout today.** The
      "89.7%, ~16 MiB to spare" above is reproducible, but only over **2
      programs** -- and the pipeline fuses the whole closure, which is **71**.
      Measured with `.agents-workspace/drivers/headroom`, which mirrors
      `pipeline.build` stages 1-2 (Inspect, ExportRootfs, Scan, Closure, Plugins,
      PlanLayoutFor) and stops before translation, so it costs an image export
      and seconds of Go:

      | run | programs | libs | plugins | top | result |
      |---|---|---|---|---|---|
      | `-max 2` | 2 | 36 | 81 | `0x9020010` | 89.8%, 15.9 MiB free |
      | full, `-plugins=none` | 71 | 49 | 0 | `0x9a41640` | 96.3%, 5.7 MiB free |
      | **full (what `build` does)** | 71 | 51 | 81 | needs `0xb020010` | ❌ **OVER by 16.1 MiB** |

      ❗ **The cause is fully accounted for and it is not the plugins.** `shared
      min` is `0xe00000` in every run, so `exeTop` never moved -- postgres is the
      largest executable and is in all of them. The 69 non-entry programs pull in
      **15 additional distinct libraries**, and at `libAlign` 2 MiB those cost
      ~32 MiB. The two tops differ by exactly **32.0 MiB**. Even with plugin
      discovery off the library band alone is at 96.3%, i.e. room for ~2 more
      libraries.

      ⚠️ **The recorded number was never describing a build.** It is not that
      something regressed since 2026-08-23 -- a 2-program plan is not what
      `raptormark build` plans, so the entry has been reporting a pass for a
      configuration the pipeline does not use. Re-verify a headroom figure by the
      PROGRAM COUNT it was taken at; this file's own rule about re-verifying
      entries is what turned it up.

      ❗ **Nothing in the suite would ever have noticed, by construction.**
      `e2e/pipeline_test.go` is the only `SharedLayout` assertion and it (a) runs
      a small custom fixture (`pgExtFixture`: debian + 2 extensions), not
      `postgres:17`, and (b) deliberately only `t.Logf`s the shared-layout status
      because an overflow is a legitimate degradation. Both choices are defensible
      on their own and together they mean the flagship closure's regression is
      invisible. Same shape as `_recovery/`: every reference tolerates absence.

      ✅ **SURVEYED ACROSS NINE IMAGES 2026-08-26, so "grow the region -- by how
      much?" now has a number.** Only postgres is anywhere near the ceiling, and
      both its versions are already over it:

      | image | programs | libs | plugins | used | headroom |
      |---|---|---|---|---|---|
      | **postgres:18** | 71 | 53 | 83 | ❌ needs `0xb690010` | **over by 22.6 MiB** |
      | **postgres:17** | 71 | 51 | 81 | ❌ needs `0xb020010` | **over by 16.1 MiB** |
      | node:22-slim | 3 | 7 | 0 | 73.2% | 41.7 MiB |
      | php:8.3-cli | 6 | 42 | 3 | 69.8% | 47.1 MiB |
      | python:3-slim | 1 | 19 | 80 | 43.4% | 88.4 MiB |
      | ruby:3-slim | 2 | 10 | 3 | 21.3% | 122.7 MiB |
      | nginx:latest | 6 | 10 | 3 | 18.5% | 127.1 MiB |
      | redis:7-alpine | 3 | 4 | 5 | 11.4% | 138.2 MiB |
      | nginx:alpine | 3 | 5 | 5 | 11.0% | 138.8 MiB |
      | debian:trixie-slim | 1 | 6 | 3 | 12.1% | 137.1 MiB |

      ❗ **The gap between postgres and the next-largest image is enormous** --
      postgres:18 wants 179 MiB where node, the runner-up, uses 114 MiB and
      everything else fits in a quarter of the region. So this is not a fleet-wide
      squeeze that a modest bump relieves; it is one workload, and a region sized
      for it (>= 179 MiB, i.e. `brkStartVMA` >= ~`0xB800000`) leaves every other
      image with more slack than it already has.

      ✅ **DECIDED AND DONE 2026-08-27 BY THE OPERATOR: the region was grown,
      156 -> 188 MiB.** `BRK_START_VMA` / `brkStartVMA` 0x0A000000 -> 0x0C000000.
      Both postgres versions now plan SHARED:

      | image | top | used | headroom |
      |---|---|---|---|
      | postgres:17 | `0xb020010` | 91.6% of 188 MiB | 15.9 MiB (~7 libraries) |
      | postgres:18 | `0xb690010` | 95.0% of 188 MiB | 9.4 MiB (~4 libraries) |

      ❗ **THE ARENA DID NOT GROW, AND MUST NOT HAVE.** `runtime/src/arena.rs`
      records a hard ceiling: `arena_size × (live + suspended) < 4 GiB` on wasm32,
      where 384 MiB allows 10 process buffers and **postgres needs 7
      concurrently** for a single psql. Buying the fused region out of the arena
      would have spent exactly the headroom this change exists to serve.

      The 32 MiB came from two regions next door, both measured:
        * **brk 32 -> 8 MiB.** Max brk high-water across 104 guest runs was
          1,164 KiB, 103 of them at 136 KiB. 8 MiB is still 7x the largest ever
          seen, and overflow is graceful -- `NR_BRK` returns the break unchanged,
          malloc falls back to mmap, the path initdb already takes.
        * **mmap 160 -> 152 MiB.** Peak private mmap measured 88 MiB, so still
          1.7x, and well above the 96 MiB that prompted the 2026-08-25 rebalance.

      ✅ **No cached object was invalidated.** `brkStartVMA` is used only as a
      refusal test, so every closure that already fit places every library at the
      same base -- verified on nginx:latest, whose band still starts at
      `0x800000`. `BRK_END_VMA`/`MMAP_START_VMA` appear nowhere outside
      `runtime/`. The rebuilt builder carries identical `raptormark.base_id` and
      `raptormark.translate_sh` labels, and a DIFFERENT `libecvisor.a` hash --
      both checked, the second on the artifact rather than the labels.

      ⚠️ **postgres:18 is at 95%.** Four more 2 MiB-aligned libraries and it
      falls back again. The next lever is `libAlign`, which moves every base and
      re-lifts everything.
      ⚠️ **A GAP THIS EXPOSED:** `TestBrkStartMatchesTheRuntime` compares Go
      source to Rust SOURCE, not to the runtime baked into the builder image. A
      stale builder plus a new fuser passes the guard and produces a module whose
      libraries sit where the heap starts. The two halves must ship together and
      nothing enforces it.

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

      ✅ **DONE 2026-08-23.** ecv-split is the default; the switch is now
      `RAPTORMARK_NO_STABLE_SPLIT` -> `ECV_NO_STABLE_SPLIT`, in `experimentalVars`
      in place of the old one, so a DEFAULT build's `TranslateID` is unchanged
      (the pinned literal in `translate_test.go` still holds) and only turning it
      OFF moves the key. `TestStableSplitIsTheDefault` guards the default and was
      neutralized by reverting `stableSplitEnabled` to the old semantics.

      ⚠️ **The batching advice was already moot, and the reason is worth
      keeping.** `-fPIC` had landed unconditionally by then
      (`translateone.go`, `TestCompileIREmitsPIC`), so there was nothing left to
      batch with. More to the point, the cache this was protecting is not
      reachable anyway: the newest builder image on the host is
      `raptormark-builder:fsync` (2026-08-11), which has `ecv-split` but **no
      `ecv-prepare`** — it predates the 2026-08-13 merged-prepare default and
      cannot run today's pipeline at all — and its patched base
      (`sha256:d8f7a3aa…`) is no longer present as an image. So the 6.6 GB
      `.agents-workspace/objcache` is keyed to a toolchain that no longer exists
      here, and a `BASE_ID`-preserving side build is not available.

      ❗ **OWED: E2E evidence.** The flip changes emitted bytes, and this
      project's standard for that is the E2E suite on an image carrying the
      default (that is what the ecv-prepare flip rested on). It has NOT been run,
      because no image can carry it yet. Next time a builder is built, run the
      fast suite before treating the promotion as validated.

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
- [x] **✅ REMOVED 2026-08-25 on the user's instruction, after measurement showed
      `.ecv.funcs` was REDUNDANT BY CONSTRUCTION.** Full history in `JOURNAL.md`.

      **The measurement that settled three sessions of debate**, and it needed no
      lift -- just the section decoded from three fused fixtures and diffed
      against the `.symtab` the lifter actually reads:

      | fixture | `.ecv.funcs` | symtab FUNC | sizes disagree | ecv-only |
      |---|---|---|---|---|
      | `python-glibc.fused` | 11,666 | 11,667 | **0** | **0** |
      | `nginx-alpine.fused` (musl) | 20,142 | 20,142 | **0** | **0** |
      | `ruby-glibc.fused` | 16,740 | 16,741 | **0** | **0** |

      Never a superset, never a disagreement. `funcRangesOf` fed BOTH tables, so
      unifying them -- which fixed a real 2026 defect where they disagreed by
      2,531 functions -- is exactly what made the second copy redundant.

      **What was removed**: `funcTable` and its `addTable` call
      (`internal/fuse/fuse.go`), `internal/fuse/funcs_test.go`, its `BUILD.bazel`
      entry, and two stale doc references. `funcRangesOf` and `symbolsOf` STAY --
      the merged `.symtab` is built from them and is now their only consumer,
      which their docs say.

      ✅ **Verified on the artifact, not by grep**: a real OpenSSL fuse now emits
      7 `.ecv.*` sections and no `.ecv.funcs`, and the fused image went
      **11,160,720 -> 10,828,272 bytes, reclaiming 332 KB** of a region already
      at 89.7% on postgres.
      ✅ Guarded by `assertNoRedundantFuncTable` in
      `TestOpenSSLFixtureDiscoverAndFuse`, with a positive control requiring the
      other `.ecv.*` sections to be present -- an absence check passes trivially
      on a truncated read. Both arms neutralized.

      ⚠️ **Cost**: the fused bytes changed, so the object cache is cold for any
      image whose fuse output moved. That was known and accepted.
      ⚠️ **What would reopen it**: evidence that elflift mis-bounds a function
      the `.symtab` does not bound. The 17 size-0 symbols on nginx and 12 on ruby
      are real fallback cases -- and `.ecv.funcs` could not have helped with any
      of them, since it recorded size 0 for them too.
      ⚠️ Recorded because the first pass of this sweep asserted the opposite,
      from the grep hit alone, before reading the test.
- [ ] **Decide whether the shared-name path stops being opt-in.** It still
      reaches no default path. The blocker is that
      `TestEcvisorTwoProgramsLinkWithoutCollision` asserts two programs' objects
      share NO symbols, which sharing inverts by design, so that test needs its
      own expectations before the flag can default on. Running without the
      closure layout must stay a no-op, not a wrong answer.
      ✅ **RE-VERIFIED 2026-08-25, unchanged and still accurate.**
      `RAPTORMARK_SHARED_NAMES` is still gated through
      `internal/translate/experimental.go:68` and reaches the container only as
      `ECV_SHARED_NAMES=1` (`translate.go:601`); `e2e/sharednames_test.go` sets
      it explicitly, so nothing default-path exercises it. The named blocker is
      real: `e2e_test.go:358` states the invariant as the two objects' exported
      sets being **DISJOINT** -- in those words -- which is exactly what sharing
      inverts.
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

- [x] **✅ CLOSED 2026-08-25 — STRUCTURALLY, which is what this entry said it was
      waiting for.** Re-verified against the tree, not assumed:
      `builder/Dockerfile` has no `COPY builder/_tools/...` line at all — it is
      `COPY . /` over `//builder:stage`, and `builder/BUILD.bazel:119` lists
      `//cmd/raptormark:builder_tools_linux_arm64` as a **dependency** of that
      stage, so Bazel cannot assemble the image contents without building the
      binary. `grep -rn _tools` finds no remaining reader of the path.
      The entry closed itself in the way it asked to be closed: it rejected both
      structural fixes it could think of (folding the hash into `TranslateSH`,
      a Docker build stage) and settled for "one forgotten command rather than
      one forgotten thought". A third fix it did not consider — moving the build
      into the dependency graph — made the hazard unrepresentable instead.
      `raptormark build-tools` now FAILS with instructions rather than rebuilding
      a file nothing reads (`internal/builder/buildtools.go:62`); a command that
      looks like it worked is the same failure it was written to prevent.
      ⚠️ **The stale binary is still on disk** at
      `builder/_tools/raptormark-builder-tools`, read by nothing. Left in place:
      it is a pre-existing user file, not this session's to remove.
      Original text follows.

      **⚠️ `builder/_tools/raptormark-builder-tools` is a PREBUILT binary, and a
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

      ✅ **(2) ANSWERED 2026-08-25: THE SHIM INTERPRETS.** Read at the EXACT
      version `e2e/containerd_test.go` pins, `containerd-shim-wasmedge/v0.6.1`,
      not at `main`:

      ```toml
      wasmedge-sdk = { version = "0.14.0", default-features = false }
      default = ["standalone", "static", "plugin"]
      ```

      ❗ **No `aot` feature, so this is not a runtime configuration choice -- the
      compiler is not in the binary.** The shim cannot AOT-compile whatever it is
      handed. `instance.rs` agrees and adds nothing: it builds a default
      `ConfigBuilder::new(CommonConfigOptions::default())`, makes a `Vm`, and
      calls `vm.run_func(...)`. There is no `Compiler`, no
      `CompilerOutputFormat`, no compile step of any kind.

      Which of the entry's two branches that lands on:
        * ✅ **Our measurements DO describe the shipping configuration.** Every
          wasmedge number in this tree is an interpreter number, and so is
          production. Nothing recorded needs re-qualifying as "not the shipping
          case".
        * ❗ **And every guest ships ~35x slower than it needs to.** That is now
          a stated performance headroom rather than an open question -- on the
          measured call-heavy loop, 4,673 ms interpreted against 133 ms AOT.

      ⚠️ **This is READ, not RUN.** The evidence is the shim's build
      configuration and source at the pinned tag, which is strong for "cannot
      AOT" -- an absent feature cannot be enabled by a flag -- but no run was
      timed through containerd. The empirical confirmation is the env-gated
      `RAPTORMARK_E2E_CONTAINERD` suite with a timing guest, which pulls ubuntu
      and shim releases from GitHub and so was not run for a sweep.
      ⚠️ Consequence (1) is UNCHANGED and still open: the tree's wasmedge
      timings remain interpreter numbers, valid as comparisons and not as
      absolutes.

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

      ❗ **MEASURED 2026-08-25, and it shrinks the claim AGAIN: a bare `wasmedge`
      is only a PARTIAL proposal gate.** Both entries assume that dropping
      `--enable-all` would restore a runtime no-proposal guard. It would restore
      part of one. Four hand-authored 33-to-40-byte modules under wasmedge
      0.17.1, each a minimal module differing from its control in ONE byte:

      | module | bare `wasmedge` | `--enable-all` |
      |---|---|---|
      | `return_call` (tail calls, post-2.0) | **ACCEPTED** | accepted |
      | shared memory (threads, post-2.0) | **REJECTED** | accepted |
      | `call` (the same module, 2.0) | accepted | accepted |
      | unshared memory (the same module, 2.0) | accepted | accepted |

      The rejection is at PARSE time, before validation:
      `loading failed: malformed limits flags, Code: 0x11d ... At AST node:
      limit`. The two 2.0 rows are the controls that stop the table being read as
      "bare wasmedge rejects small modules"; the one-byte deltas
      (`0x12`->`0x10`, limits flag `0x03`->`0x01`) are what make each pair
      differ in the proposal and nothing else.

      **So the decision below is now: is a gate that catches THREADS but not
      TAIL CALLS worth the change?** It is not nothing -- threads/shared memory
      is precisely the proposal the `wasix` side-module work needed, so it is the
      one most likely to creep in -- but it must not be described as "the runtime
      no-proposal guard". ⚠️ Whatever is decided, `wasm-opt`'s flag list
      (`TestWasmOptEnablesNoProposal`) stays the authority, because it is the
      only check that is not a particular engine's opinion of a particular
      release.
      Probes kept at `.agents-workspace/tmp/proposalctl/`; ⚠️ that directory is
      disposable, and the generator is four lines of Python recorded in the
      2026-08-25 JOURNAL entry.
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

## Product surface

- [x] **✅ CLOSED 2026-08-24, verified 2026-08-25. No end-to-end driver.** Both
      commands named in the original text now exist and are the four-stage
      driver: `pipeline.Build` and `pipeline.Run`, registered in
      `cmd/raptormark/main.go:28-29`, implemented in `internal/pipeline/`.
      `build` also discovers plugins (`--plugins auto`) and fuses each as its own
      dlopen-able unit, which is more than this entry asked for.
      ⚠️ Two things the driver taught that the entry could not have known, both
      recorded in `AGENTS.md`: **discovery finds more than you planted** (5 units
      on a `postgres:17` fixture, 3 of them the base image's own OpenSSL
      modules — a unit count is not a plugin count), and **the three runtimes
      spell the directory flag three different ways** with none of them failing
      loudly when swapped. `runtimeArgs` in `internal/pipeline/run.go` is the one
      place that knows.
      Original text: `cmd/raptormark` covers the build steps (`build-image`,
      `translate-one`, `link-all`), but the pipeline that strings them
      together — discovery, fuse, translate, link — still runs only from the
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

      ✅ **AUDITED 2026-08-25. No fourth hole found in the paths traced, and one
      comment that would have ENDED the audit early was fixed.**
      All 8 mutation sites (`push`/`pop`/`clear`/`set_len`/`mem::take`/assign)
      and all 10 bracket sites were enumerated and matched:
        * syscall trampoline `intrinsics.rs:486-488` adopts before `sys::svc`
          and publishes after. **This is what covers every context switch**:
          suspension is decided INSIDE `svc`, so the adopt necessarily precedes
          any `mem::take` of the vector at `context.rs:3263`.
        * `entry.rs:408` publishes before entering lifted code -- the one site
          that covers `load_current` swapping a different vector in.
        * `entry.rs:459` adopts before the replay pop (this was hole #1).
        * `entry.rs:188` publishes during bring-up (hole #3's fix).
        * the `_ic` wrappers bracket the plain intrinsics for modules built with
          the fast path; the plain ones need no brackets because such a module
          leaves `ecv_ch_built` at 0, making both calls no-ops.

      ❗ **The finding was a COMMENT, not code.** `_ecv_save_call_history` said
      "adopt them before adding ours, and republish afterwards" while doing
      neither -- that text describes `_ecv_save_call_history_ic`, which brackets
      a call to it, and stayed behind when the two entry points were split. An
      auditor reading the plain function sees the invariant apparently honoured
      there and stops. **That is precisely how this entry says holes two and
      three survived the first.** Corrected in place, with why the plain entry
      point genuinely needs no brackets.

      ✅ **The one caveat this audit first recorded is now RESOLVED, and by a
      stronger argument than the one it was hedging.** It said the
      `resume_scheduling()` path "was traced least conclusively" and rested on
      inline epilogues balancing the global. That turned out to be the wrong
      thing to worry about: **that path never takes the vector at all.**
      `save_current` -- which holds the `mem::take` at `context.rs:3263`, the only
      switch-out mutation -- has exactly ONE production caller,
      `retire_after_suspend` (`:4652`, `:4662`), which itself has exactly one,
      `schedule_after_suspend` (`:4855`), reached only from `entry.rs`'s
      `if suspended` branch. Suspension happens inside `sys::svc`, which the
      trampoline brackets. So the adopt precedes every switch-out **by control
      flow**, not by convention.
      The plain leg-return path reaches `resume_scheduling` -> `pick_next` ->
      `load_current`, which LOADS and never takes, and is followed by
      `entry.rs:408`'s publish before lifted code runs again.
      ⚠️ Note the shape of the correction: the hedge named the wrong risk.
      Whether the epilogues balance does not matter on a path that never reads
      the length.

      ⚠️ And the entry's own point stands: a completed audit is not enforcement, and
      the next edit reopens the question. ❌ The structural fix it proposes (a
      borrow-guard that adopts on construction and publishes on drop) was NOT
      done, because it is explicitly conditional -- "if this feature is kept" --
      and whether to keep the opt-in at all is a separate open DECISION in this
      file.
## Found by strengthening the postgres query, 2026-08-19

## Ruby and the JIT boundary, 2026-08-19

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

- [ ] **❗ REFRAMED 2026-08-26 by the user: YJIT IS NOT A TARGET, AND MUST END IN
      A HARD FAILURE.** The entries below read as though clearing walls 1 and 2
      would enable YJIT. They would not.

      **raptormark is AOT and there is no lifter in the module.** Code the guest
      emits at run time has no lifted counterpart, so a jump into it can only
      fail. That is wall 3, it is architectural, and it is permanent. Clearing
      walls 1 and 2 does not unblock YJIT -- it advances the failure to the one
      that cannot be cleared.

      ✅ **Wall 3 already fails HARD and informatively**, verified 2026-08-26:
      `__remill_function_call` (`intrinsics.rs:205`) and `__remill_jump`
      (`:239`) both end in `fatal!` naming the VMA and the intrinsic, and the
      `None` arm ADOPTS the call history first (`:181`) so the stack dump is not
      short by the inline-pushed frames. A JIT guest gets a loud, attributed
      abort rather than a silent wrong answer -- which is the correct outcome for
      something out of scope by construction.

      ❗ **So keep walls 1 and 2 open ON THEIR OWN MERITS, not as YJIT work.**
      Both are general defects that the YJIT path merely reaches first:
        * **Wall 1 is an ARGV-PARSING defect.** The undecoded
          `orr v28.2s, #0x80` sits in `proc_options`, so it breaks ruby option
          handling generally -- `--yjit`, `--jit` and `--yjit-exec-mem-size=8`
          all die at the same address. It is a decoder gap that happens to be
          reachable through a YJIT flag, not a YJIT feature.
        * **Wall 2 is the ADDRESS BUDGET**, which starves postgres's
          `shared_buffers` with no JIT involved. See the ⭐ entry.
      ❌ Do not cite "unblocks YJIT" as justification for either. It is not true
      and it makes both look speculative when both are ordinary.

      ⚠️ **One diagnostic gap worth considering** (not done): wall 3's message,
      "vma 0x... not in the lifted function table", is accurate but does not
      distinguish the two causes a reader must tell apart -- a LIFTING GAP, which
      is a defect to fix, versus GUEST-GENERATED CODE, which is out of scope by
      design. If the target VMA falls inside a region the guest mapped
      `PROT_EXEC` at run time, saying so would turn a generic abort into a
      specific one. Same diagnosis cost, different conclusion.

- [ ] **(original framing) What a JIT guest actually does under ecvisor -- MEASURED 2026-08-22, and
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

- [x] **✅ FIXED 2026-09-01: the setjmp/longjmp defect.** Root cause was NOT the
      mask and NOT any lifted instruction: `__remill_jump` ran an indirect
      branch as a nested CALL and returned, so `__longjmp`'s `br x30` returned
      into `__libc_siglongjmp` at the instruction after `bl __longjmp` -- the
      mask-restore block, which branches back to the call. Everything below
      about "only the mask-restoring path fails" is **SUPERSEDED**: `mask0.c`
      (mask=0, `main` returns) FAILS and `noret.c` (mask=1, `main` `_exit`s)
      PASSES. The variable was always whether the jumped-to code RETURNS;
      `variants.c` only implicated `siglongjmp(1)` because it was its last test.
      Fixed in the runtime alone (`nonlocal_jump_depth` /
      `begin_nonlocal_jump` + the `longjmp_pending` arm in `entry.rs`), so no
      `BASE_ID` move and no re-translation. ⚠️ Recursion is still a guess --
      the innermost matching frame is chosen, because `call_history` records no
      per-frame SP.
      — *evidence: `JOURNAL.md`, `2026-09-01` (two entries); reproducers and
      the neutralization in `.agents-workspace/longjmp/`*

- [x] **✅ FIXED 2026-09-01 (`patches/0072`, `0073`, `0074`): the whole
      LD1/ST1 multiple-structures family.**
      * `0072` -- `LD1_ASISDLSEP_I2_I2` (2-register post-index), the silent
        no-op that hung every Go guest.
      * `0073` -- `LD1_ASISDLSE_R3_3V`, `R4_4V`, `LD1_ASISDLSEP_I3_I3`, `I4_I4`,
        via new `TTriple`/`TQuad` return types.
        ❗ The framework support I predicted was NOT needed -- the lifter
        already handles an N-element struct return generically and `TPair` was
        never special-cased. My estimate of this as "materially bigger" was
        wrong.
      * `0074` -- `ST1_ASISDLSE_R3_3V`, `R4_4V`, `ST1_ASISDLSEP_I1_I1`,
        `I3_I3`, `I4_I4`. These had `return false` STUBS in `Decode.cpp` as
        well as no semantics, so they failed loudly; stubs removed, real
        decoders added to `Arch.cpp`.

      Validated on side build `raptormark-builder:simd`: all six reproducers in
      `.agents-workspace/govm/` pass, including the full 36-case `ld1mat.c`,
      which previously died at the first undecoded ST1.

      ⚠️ STILL OPEN: the `*_ASISDLSEP_R*_R*` register-post-index family
      (`[Xn], Xm`) for both LD1 and ST1 -- no decoder, fails loudly, and nothing
      measured uses it.
      — *evidence: `JOURNAL.md`, `2026-09-01 -- patches/0072` and
        `... 0073 and 0074`*

- [x] **✅ ROOT CAUSE (fixed above): `ld1 {v..-v..}, [xN], #imm` was a no-op.** Found
      2026-09-01; this is the ROOT CAUSE of the Go traceback hang, chased down
      from "postgres:17 stops" through five wrong hypotheses.
      Full matrix measured by `.agents-workspace/govm/ld1mat.c` (36 cases,
      all ok natively), which lifts in SECONDS -- so a patch can be validated
      on a side-tagged base without re-translating postgres:

      | LD1 | data | writeback |
      |---|---|---|
      | 1-reg, both forms | ok | ok |
      | 2-reg, no writeback | ok | -- |
      | **2-reg, post-index** | ok | **MISSING** |
      | 3-reg, either form | all zero | missing |
      | 4-reg, either form | all zero | missing |

      | ST1 | result |
      |---|---|
      | 1-reg, no writeback | ok |
      | **1-reg, post-index** (`0x4c9f7314`) | **UNDECODED, fatal** |

      ✅ COMPLETED from `Semantics/DATAXFER.cpp`, anchoring on `^DEF_ISEL(`
      versus `^// *DEF_ISEL(` -- ⚠️ an earlier pass grepped the symbol name and
      counted COMMENTED-OUT lines as implementations. Three states, not two:

      * **ACTIVE**: `LD1_ASISDLSE_R1_1V`, `R2_2V`, `LD1_ASISDLSEP_I1_I1`,
        `ST1_ASISDLSE_R1_1V`, `R2_2V`, `ST1_ASISDLSEP_I2_I2`.
      * **COMMENTED OUT** -> silent wrong answer: `LD1_ASISDLSE_R3_3V`,
        `R4_4V`, `LD1_ASISDLSEP_I2_I2`, `I3_I3`, `I4_I4`.
      * **NEVER WRITTEN** -> fatal undecoded: `ST1_ASISDLSE_R3_3V`, `R4_4V`,
        `ST1_ASISDLSEP_I1_I1`, `I3_I3`, `I4_I4`, and every
        `*_ASISDLSEP_R*_R*` (register post-index `[x], xM`).

      ❗ **The disabled code cannot simply be uncommented.** It is written
      against the old `DEF_SEM(..., R64 addr_reg, ADDR next_addr)` signature
      with an explicit `Write(addr_reg, ...)`, while the ACTIVE single-register
      form uses `DEF_SEM_T_RUN` and does no explicit writeback. Porting is
      required; estimating this as "uncomment two lines" will be wrong.

      Original (now superseded) reading of the cells:

      * **No semantics at all (loud failure)**: `ST1_ASISDLSE_R3_3V`
        (`0x4c00600a`, measured), `ST1_ASISDLSE_R4_4V`,
        `ST1_ASISDLSEP_I1_I1` (`0x4c9f7314`, measured),
        `ST1_ASISDLSEP_I3_I3` (`0x4c9f62aa`, measured),
        `ST1_ASISDLSEP_I4_I4`, and the ENTIRE register-post-index family
        (`LD1/ST1_ASISDLSEP_R*_R*`, i.e. `[x], xM`) for both loads and stores.
      * **Semantics present but WRONG (silent)**: `LD1_ASISDLSEP_I2_I2` loses
        its writeback -- this is the one that hangs Go; `LD1_ASISDLSE_R3_3V`,
        `R4_4V`, `LD1_ASISDLSEP_I3_I3`, `I4_I4` load nothing.

      ⚠️ `ST1_ASISDLSEP_I2_I2` handles writeback CORRECTLY while
      `LD1_ASISDLSEP_I2_I2` does not -- the same shape on the store side is
      fine, so do not assume the two sides share a bug or a fix.
      ⚠️ `ld1wb.c` (kept) claimed the 2-register post-index also loses its 2nd
      vector; that was its own constraint bug. Only the WRITEBACK is missing
      there, which is still exactly what breaks Go.
      ❗ SILENT: the instructions DECODE, so there is no `_ecv_unreached` and
      `RAPTORMARK_ECV_UNDEC_CENSUS=1` reports nothing. Patch 0068 fixed the LD1
      SINGLE-ELEMENT post-index operand order; this is the MULTIPLE-STRUCTURES
      family, different decoders, not covered.
      ✅ SCOPE MEASURED: glibc's ONE multi-register instruction is
      `ld1 {v1.16b-v2.16b}, [x1]` (2-reg, NO writeback) -- a form that WORKS;
      musl has none; gosu has 23, of which 16 are post-index and 14 are
      4-register. A Go-guest defect, not a general one, and for a specific
      reason: the single form libc uses is the one the lifter gets right.
      The reason E2E stayed green is that the fixture set has no Go guest.
      ❌ "Add a Go guest to `e2e/`" is NOT available as the cheap first step,
      contrary to what this entry first said: EVERY Go binary reserves 512 MiB
      at startup, so no Go guest can run until that is resolved. The usable
      regression guest is `ld1mat.c` itself -- plain C, exercises the broken
      encodings directly, and would have caught this. It cannot be added to a
      green suite until the patch lands, since it fails today.
      Batching this with `stlxrb` is the obvious move: one `BASE_ID` move buys
      both, as was chosen for the previous lifter round this session.
      — *evidence: `JOURNAL.md`, `2026-09-01 -- ROOT CAUSE`; reproducer
        `.agents-workspace/govm/`*

- [x] **✅ FIXED 2026-09-01 (`patches/0075`): zero-register reads took the
      wrong TYPE.** Not a `STLXRB` bug -- `LiftRegisterOperand` returns early
      for `XZR`/`WZR` with a hard-coded i64/i32, skipping the truncation every
      real register gets, so any narrower semantic mismatched. Fixed by taking
      the argument type. Covers `STXRB`, `STLXRH`, `STXRH`,
      `STRB`/`STRH`/`STLRB`/`STLRH` too, not just the one instruction.
      Go 1.26 binaries now lift (`gohello`: abort -> 5.41 MiB module).

- [x] **✅ FIXED 2026-09-01 (`patches/0076`, `0077`): the LD<n>R replicate
      family, plain and post-index.** Only `LD1R_ASISDLSO_R1` had ever been
      implemented; the rest were `return false` stubs (fatal, not silent).
      0076 does the plain `LD2R/LD3R/LD4R`, reusing `TTriple`/`TQuad` from
      0073; 0077 does all four post-index forms, sharing the semantics.
      ⚠️ Access size stays ONE element while the advance is
      `num_regs * elem_bytes`.
      **Go 1.26 now lifts, runs and prints a fully symbolised traceback**,
      stopping only at the 512 MiB reservation like `gosu`. Reproducer
      `.agents-workspace/govm/ldnr.c` (19 checks incl. upper lanes and
      writeback).
      ⚠️ Register-OFFSET forms (`[Xn], Xm`) of both families are still stubs;
      they fail loudly and nothing measured uses one.

- [x] **(superseded) Go 1.26 stops on `ld4r {v0.4s-v3.4s}, [x10]`.**
      Found 2026-09-01 immediately after `0075`, in
      `internal/chacha8rand.block`. LD4R is load-and-REPLICATE, a separate
      family from the multiple-structures forms `0072`-`0074` implemented.
      ⚠️ Does NOT affect the postgres path: `gosu` (Go 1.24) also carries four
      `ld[1234]r` but dies in `mallocinit` before reaching them.
      — *evidence: `JOURNAL.md`, `2026-09-01 -- patches/0075`*

- [x] **(superseded) The lifter aborts on `stlxrb Ws, wzr, [Xn]` (`0x081bfc1f`).**
      Found 2026-09-01 building a hello-world with the tree's own Go (1.26.5):
      `Check failed: op_type == arg_type ... (READ_OP (REG_32 WZR)) to
      STLXRB_SR32_LDSTEXCL ... Expected i8 but got i32. arg_num: 2`.
      ❗ NOT a "newer Go emits new instructions" problem -- `gosu` (1.24.6) also
      carries `stlxrb`/`ldaxrb` and LSE atomics and lifts fine. The
      discriminator is the ZERO REGISTER as the byte operand: gosu has 0 such
      instructions, the new binary has 3, and the assertion's
      `address: 162792` = `0x27BE8` is the first of them exactly.
      A real encoding for a test, per AGENTS.md's rule about hand-derived masks.
      ⚠️ Costs a `BASE_ID` move and a full re-translation, and nothing in the
      postgres closure needs it today -- so it gates future Go guests, not the
      current one.
      — *evidence: `JOURNAL.md`, `2026-09-01 -- gosu standalone`; reproducer
        `.agents-workspace/govm/`*

- [ ] **`postgres:17` now stops in `gosu`: Go wants a 512 MiB reservation.**
      Found 2026-09-01. The entrypoint's final `exec gosu postgres ...` replaces
      pid 1 with a Go binary whose `mallocinit` reserves 536,870,912 bytes in
      one call; the mmap region is 152 MiB and the whole arena is 384 MiB, so it
      is ENOMEM and Go dies with "failed to reserve page summary memory".
      ❗ **Growing the arena cannot fix it**: 817 MiB would be needed, which
      drops the process-buffer ceiling from 10 to 5, and postgres needs 7 for
      one `psql`. The two constraints are now in direct conflict.
      The reservation is `sysReserve` -- PROT_NONE, never touched, therefore
      never divergent between processes -- so the direction worth costing is a
      SHARED reservation region charged once against the 4 GiB ceiling rather
      than once per process buffer. Not attempted.
      ✅ Reproduces standalone in 90 s via `.agents-workspace/govm/` (lift
      `gosu` alone) rather than an ~8-minute postgres run.
      — *evidence: `JOURNAL.md`, `2026-09-01 -- postgres:17 past find`*

- [x] **✅ FIXED 2026-09-01 by `patches/0072`: the guest spun in Go's traceback.**
      The spin WAS the `ld1` no-op: `findnull` returned -32, so
      `printFuncName` looped on a string of negative length. With 0072 applied
      `gosu` prints a fully symbolised traceback and exits rc=2. This entry was
      written before the root cause was known and is closed by measurement, not
      by inference -- the fix and the exit were both observed.
      ⚠️ Superseded, kept because its investigation notes record two WRONG
      characterisations of the same log ("scheduler spinning", "parked in a long
      sleep") that are worth not repeating.
      Found 2026-09-01. MEASURED, with `RAPTORMARK_ECV_TRACE=sched,svctramp` on
      a standalone `gosu.wasm` (90 s to reproduce, vs ~8 min via postgres):

      1. the 512 MiB reservation fails -> ENOMEM;
      2. Go writes the fatal message (3 x `write`, nr=64);
      3. two 1 ms `nanosleep`s (nr=101), each a normal block/IDLE/resume cycle
         -- the scheduler handles them correctly;
      4. one more `write` of `"\nruntime stack:\n"` (16 bytes);
      5. then **nothing at all** for the remaining ~85 s: zero syscalls, no
         `IDLE` line, no further output, until the harness timeout (rc=124).

      `svctramp` traces EVERY syscall and `sched` prints on every idle, so zero
      of both while the module is still running places the guest inside lifted
      code -- an infinite loop in Go's traceback, not a park and not a
      scheduler defect.

      ✅ LOCALISED with the differential call tracer:
      `traceback2 -> printFuncName -> funcNamePiecesForPrint`, called with a Go
      string whose LENGTH IS -32 (`0xffffffffffffffe0`), which
      `runtime.findnull` returned immediately before. A print loop over a
      negative length does not terminate.
      ✅ pclntab RULED OUT: the guest bytes at `0x11f589` are
      `runtime.throw\0`, identical to the ELF, dumped with
      `RAPTORMARK_ECV_DTRACE_DUMPX0`. The bytes are right; the LENGTH is wrong.
      ✅ Undecoded instructions RULED OUT: `RAPTORMARK_ECV_UNDEC_CENSUS=1`
      reports no `addr=` lines.
      ✅ Localised to `internal/bytealg.IndexByteString.abi0` (`0x12280`, NEON
      assembly), reached from `runtime.findnull` at `0x649e4`; it must return
      13 for `"runtime.throw"` and something yields `-32` (note: NOT `-1`, the
      not-found answer).
      ⚠️ OPEN: whether that function is mis-lifted, or whether `[sp,#32]` is
      never written and `findnull` reads stale stack. NEXT STEP: the
      differential trace the tracer exists for -- trace `0x12280` under ecvisor
      and under native gdb on the same input, diff the first divergence.
      ❗ **Wider than gosu.** `IndexByteString` is on the path of ordinary Go
      string handling, not just panics, so this is not gated behind the 512 MiB
      reservation.

      ⚠️ This entry has been rewritten twice from the same log. It first said
      "leaves the module spinning" (wrong -- the scheduler was idle, not
      spinning), then "parked in a long sleep" (also wrong -- the sleeps are
      1 ms and there are only two). Both were read off `[sched]` lines without
      checking the syscall trace. Do not re-characterise this from the sched
      lines alone.
      — *evidence: `JOURNAL.md`, `2026-09-01 -- gosu standalone`;
        reproducer `.agents-workspace/govm/`*

- [x] **✅ FIXED 2026-09-01: `fchdir`, and relative paths against a dirfd.**
      Found 2026-09-01, immediately after the longjmp fix, by running
      `.agents-workspace/pgbuild/out4/app.wasm`. `docker-entrypoint.sh` reaches
      `find`, which prints "Failed to change directory" and "Failed to restore
      initial working directory". Measured with `-e RAPTORMARK_ECV_DEBUG=1`:
      `2 x ENOSYS syscall 50` (fchdir) and `8 x ENOSYS syscall 43` (statfs).
      `chdir` (49) IS implemented; `fchdir` is simply absent.
      ✅ Both were ONE root: `OpenFile::Dir` did not record its directory.
      Adding `path` to it gave `fchdir` and `resolve_base` alike. ❗ The refusal
      turned out to live in FOUR places (`resolve_arg`, `unlinkat`,
      `readlinkat`, `mkdirat`), each with its own errno -- all four converted.
      Measured by `.agents-workspace/dirfd/dirwalk.c`: `DIRWALK FAILED 7`
      before, `DIRWALK OK` after, matching native.
      ⚠️ `statfs` (43) is still ENOSYS, 8 calls' worth. Nothing has yet been
      shown to FAIL on it -- `find` and the entrypoint carried on -- so it is
      recorded, not assumed to be the next blocker.
      — *evidence: `JOURNAL.md`, `2026-09-01 -- postgres:17 runs past the
      longjmp` and `... Directory descriptors`*

- [x] **✅ CLOSED 2026-09-01: ruby's `--disable-gems` requirement WAS the
      setjmp/longjmp defect.** Controlled comparison on the same fused ELF and
      the same `p-gems.img` sidecar (whose argv omits `--disable-gems`):
      the 2026-08-22 module prints
      `*** longjmp causes uninitialized stack frame ***` and exits 1; the same
      ELF relinked with the fixed runtime prints NO longjmp message and runs
      much further. glibc's `__longjmp_chk` FORTIFY check no longer fires.

- [x] **✅ FIXED 2026-09-01/02: `pthread_getattr_np` failed ENOENT (no `/proc`).**
      Found 2026-09-01 while chasing the Ruby GC corruption.
      `.agents-workspace/rubygems/stackb.c`: native reports
      `[0xffffcab28000,0xffffcb328000)` with sp inside; ecvisor returns rc=2.
      glibc implements it for the MAIN thread by reading `/proc/self/maps`, and
      a grep for `"/proc` across `runtime/src` finds only a comment -- nothing
      is synthesised.
      ❗ Wrong on its own terms regardless of Ruby: a guest asking where its own
      stack is gets ENOENT. It is also the machinery a CONSERVATIVE GC uses to
      choose its scan range, so it is the leading candidate for the Ruby
      corruption.
      ✅ FIXED 2026-09-01 (runtime-only, `raptormark-builder:procmaps`):
      `sys_openat` synthesises `/proc/self/maps` from `arena.rs`'s regions.
      `pthread_getattr_np` now returns `[0x16000000,0x18000000)` with sp inside.
      ⚠️ Did NOT fix the Ruby corruption -- kept because a guest being unable to
      find its own stack is wrong on its own terms.
      ✅ GATED: `cargo fmt` clean, 327 Rust tests, and E2E 121/0/35 on
      `raptormark-builder:procmaps` -- identical to the pre-change baseline by
      name-set comparison, 369 s on a warm cache (runtime-only, `BASE_ID`
      unchanged).
      Fix by synthesising `/proc/self/maps` (ecvisor knows the true bounds via
      `arena::STACK_TOP_VMA` and the process's stack allocation), or by
      special-casing the query.
      — *evidence: `JOURNAL.md`, `2026-09-01 -- GC mechanism`*

- [ ] **Ruby still cannot load RubyGems -- new blocker, an out-of-bounds read.**
      Found 2026-09-01 immediately after the above. Without `--disable-gems`:

          execution failed: out of bounds memory access, Code: 0x408
            Accessing offset from: 0x25fc084c to: 0x25fc084f
            Out of boundary: 0x1a03ffff
            In instruction: i32.load (0x1e)

      ✅ LOCALISED 2026-09-01 via `drivers/wasmnames.py`:

          rb_id_table_lookup            <- the OOB load
          callable_method_entry_or_negative / rb_vm_search_method_slowpath
          rb_funcallv -> rb_inspect -> rb_protect
          name_err_mesg_to_str          <- building a NameError's message
          exc_to_s -> rb_String -> rb_check_string_type
          rb_get_detailed_message -> rb_ec_error_print_detailed -> error_handle

      ❗ Ruby is ALREADY HANDLING AN EXCEPTION. RubyGems raises a **NameError**
      and the crash is in FORMATTING it -- so the NameError is the real defect
      and the OOB is downstream. Nothing reached stderr, which is why it looks
      like a bare wasm trap. (Same shape as the Go traceback hang: the visible
      crash was in the reporting path.)
      ✅ RULED OUT as the longjmp fix misfiring: `fn_plt -> __remill_jump`
      recurs in the stack, but the same module with `--disable-gems` prints
      `STARTUP-OK` rc=0, and a PLT `br` targets a function ENTRY, which
      `nonlocal_jump_depth` classifies as a tail call first.
      ✅ DIAGNOSED further 2026-09-01 with `.agents-workspace/rubygems/probe.rb`
      (boots `--disable-gems`, then `require "rubygems"` inside a `rescue`, so
      the exception is named WITHOUT running the crashing printer; that probe
      exits rc=0):

          class=NoMethodError
          name=:to_s
          recv-failed NoMethodError      <- .class on the receiver ALSO raises
          bt=/usr/local/lib/ruby/3.4.0/rubygems.rb:9:in 'Kernel#require'

      `rubygems.rb:9` is `require "rbconfig"`, and filetrace shows `rbconfig.rb`
      IS read -- nothing is missing from the image.
      ❗ **The receiver is a CORRUPTED Ruby VALUE**, not an object missing a
      method: no valid object raises `NoMethodError` from `.class`.
      ✅ ONE root cause, not two: the corrupted VALUE raises the
      `NoMethodError`, and describing it walks a garbage class pointer through
      `rb_id_table_lookup`, which is the OOB. Fix the corruption and both go.
      ✅ NARROWED 2026-09-01 to the BOOT path. Refuted, each with a probe:
      frozen string literals (6/6 OK), `rbconfig` itself (top-level require
      OK), and nested require (`/nested.rb` OK with and without the magic
      comment). The remaining difference is that the failing sidecar omits
      `--disable-gems`, so RubyGems loads during `ruby_options`; loading it
      POST-boot instead reaches rubygems.rb:1415.
      ⭐ **2026-09-01: GC is the TRIGGER (not proven to be the bug).**
      `gcprobe.rb` requires rubygems with and without `GC.disable`, same binary
      and sidecar: GC enabled -> `NoMethodError name=:to_s`; **GC disabled ->
      `LoadError`**, i.e. no corruption at all, reaching only the fixture's
      missing `monitor.so`.
      Bisect that led there: rubygems.rb first 17 lines OK, first 18 crash
      (line 18 = `require_relative "rubygems/defaults"`), but that file ALONE
      is fine -- cumulative, i.e. a collection threshold.
      ❌ REFUTED 2026-09-01: "setjmp does not spill live registers".
      `.agents-workspace/rubygems/gcregs.c` parks magics in x19/x20, calls
      `setjmp` and scans the buffer -- exactly what
      `rb_gc_mark_machine_context` does. Native and ecvisor BOTH find them.
      ⭐ LEADING CANDIDATE instead: the GC cannot learn its scan RANGE.
      `pthread_getattr_np` fails ENOENT under ecvisor (no `/proc` at all) --
      see the entry above. NOT yet confirmed: that Ruby's fallback bounds are
      actually wrong. Confirm by instrumenting Ruby's computed
      `stack_start`/`stack_end` against the real extent.
      ❗ **Not Ruby-specific.** Any guest with a conservative stack/register
      scanning GC has the same exposure; gems is merely the first workload that
      allocates enough to collect.
      ✅ Workaround while measuring: `GC.disable`.
      ❌ CORRECTED same day -- the GC is NOT broken in general.
      `.agents-workspace/rubygems/gcmin.rb`: 2000 objects, 3 collections, zero
      corrupted, plus an object held across a GC in a C frame. So the fault
      needs GC **and** something RubyGems does.
      ⭐ **PROVEN 2026-09-02: an object is FREED WHILE STILL REFERENCED.**
      `.agents-workspace/rubygems/heapchk.rb` runs
      `GC.verify_internal_consistency` between each step of rubygems.rb's first
      18 lines: heap clean through `compatibility`, then the failing require
      gives `check_rvalue_consistency: T_NONE is T_NONE` and
      `[BUG] objspace/memsize_of(): unknown data type 0x0`. `T_NONE` is an
      EMPTY slot -- a live object was collected.
      ❌ SEVEN hypotheses refuted by measurement: frozen literals, nesting,
      boot-vs-post-boot, `setjmp` register spilling (`gcregs.c`), missing stack
      bounds (`/proc/self/maps` now works and changed nothing), a general GC
      defect (`gcmin.rb`: 2000 objects x3 collections clean), and
      conservative-scan blindness to wasm locals (`consgc.c`: ecvisor matches
      native exactly).
      ⚠️ **STOP THEORISING -- INSTRUMENT.** Each hypothesis cost a
      build-and-run cycle and the cheap ones are gone. Identify WHICH object is
      freed and what referenced it: `RAPTORMARK_ECV_WATCH`/`WATCHLEN` for a
      guest address, plus Ruby's `GC.stress` / `ObjectSpace` / `RUBY_GC_DEBUG`
      to narrow the allocation site.
      ❌ Also refuted: the missing stack bounds. `/proc/self/maps` is now
      synthesised and `pthread_getattr_np` works, and ruby still corrupts.
      ❗ **The rbbench fixture CANNOT exercise RubyGems to completion**: its
      `root/` has ZERO `.so` files where `ruby:3-slim` ships 18, so a post-boot
      load dies on `LoadError: cannot load such file -- monitor.so`. Rebuild
      the fixture root with native extensions before measuring gems.
      Ruled out cheaply first: missing syscall (ENOSYS census identical in the
      working and failing runs), missing file (sidecar carries `rubygems.rb`),
      failed load (both files read).
      ⚠️ Reproducing costs a ~8-minute re-translation: the ruby object is not
      cached under the current `BASE_ID`.
      — *evidence: `JOURNAL.md`, `2026-09-01 -- Ruby: the --disable-gems
        requirement was the longjmp defect`*

      Every preserved 2026-08-19 sidecar carries
      `--disable-gems -I /usr/local/lib/ruby/3.4.0 -I .../aarch64-linux` and
      none of them says why, so a sidecar built the obvious way fails in a way
      that reads exactly like a builder regression. It cost an hour and a wrong
      hypothesis this session; the refutation was running the NEW sidecar
      against the KNOWN-GOOD 2026-08-19 module and watching it fail there too.

      Not diagnosed further -- nobody has localised which of RubyGems' startup
      `setjmp`/`longjmp` pairs ecvisor mis-serves, or whether it is the machine
      stack bounds `__longjmp_chk` consults.

      ✅ **NARROWED 2026-08-27, statically and cheaply. Four things are now
      settled and one of them the entry above asks as an open question.**

      1. ❗ **It is NOT the stack bounds, and `runtime/src/sys.rs` already said
         so.** `NR_SIGALTSTACK`'s comment records the probe: reporting the WHOLE
         ARENA as `ss_sp`/`ss_size`, which would admit any in-arena target, and
         `____longjmp_chk` still refused -- "so the SP glibc recovers from the
         jmp_buf is outside the arena altogether." The entry's second open
         question was answered in the code before this entry was written.
      2. **The mechanism, disassembled from `ruby-glibc.fused`.**
         `__sigsetjmp` (`0x1636600`) mangles BOTH the return address and the
         stack pointer by XOR with `*__pointer_chk_guard`, loaded through the
         GOT: `ldr x2,[x2,#3632]; ldr x3,[x2]; eor x5,x4,x3; str x5,[x0,#104]`.
         So a wrong guard at demangle time yields an arbitrary SP -- exactly the
         symptom in (1).
      3. ❌ **The obvious fuser hypothesis is REFUTED.** The GOT slot at
         `0x17afe30` holds `0x183fb58`, which is the single `__pointer_chk_guard`
         in the image -- and there IS only one (`libc.so.6` defines it,
         `ld-linux-aarch64.so.1` defines none). So this is not `globalSymbols`
         first-wins picking the wrong copy, which is the class of defect fixed
         on 2026-08-26 and was the prior worth testing first.
      4. The guard is **zero in the image** (`.data.rel.ro.l6`, PROGBITS), and
         ecvisor DOES supply `AT_RANDOM` -- 16 bytes of `0x01` at the stack top
         (`arena.rs`, `build_stack`) -- so glibc's startup writes a non-zero
         constant over it. A guard that stayed 0 throughout would round-trip
         harmlessly; one that CHANGES between the setjmp and the longjmp gives
         `SP ^ 0x0101010101010101`, which lands far outside a 96 MiB arena.

      ⭐⭐⭐ **LOCALISED 2026-08-28 to a single unknown.** 7-line reproducer
      (`static jmp_buf jb; setjmp; inner(){longjmp}`), and the failure is ONLY on
      the mask-restoring path: `_longjmp` OK, `siglongjmp(mask=0)` OK,
      `siglongjmp(mask=1)` FAILS.

      Chain, every step measured: `__longjmp` returns to the CORRECT address
      (`0x40057c` = the `cbz w0` right after `bl __sigsetjmp`); `w0` is 0 there,
      which `__longjmp`'s `cmp/mov/csel` tail cannot produce; main therefore
      re-enters its `== 0` body and calls `siglongjmp` again; x19 is then 1
      (proved: the traced `rt_sigprocmask` set is `0xb9` = 1 + `#0xb8`); a
      jmp_buf pointer of 1 reads zeros, so `eor x30, x4, x3` yields the guard and
      the branch target is `0x0101010101010101`.

      ❌ **Refuted by measurement, so do not re-try these**: the stack canary; the
      pointer guard's stability (jmp_buf verified byte-identical at both points);
      `ldp`/`ldr`/`eor` lifting (isolated replica correct); callee-saved clobber
      across a raw syscall (x19 preserved); `sigprocmask` corrupting the jmp_buf;
      and the backward-branch CFG shape (replica preserved x19).

      ⭐ **The one unknown: why is `w0` 0 at the return?** Two candidates -- the
      `svc` trampoline not restoring `x0` on this path, or `br x30` not carrying
      it. Distinguished by printing `x0` at `0x40057c`, which ecvisor can hook
      (`RAPTORMARK_ECV_COUNTRET` already hooks return addresses).

      ⭐⭐ **SECOND WITNESS 2026-08-28, AND IT IS NOW THE POSTGRES BLOCKER.**
      With shebang support in, `raptormark run` on the postgres:17 module boots
      correctly and dies here:

      ```
      [ecvisor] pid=1 boot argv: ["/usr/local/bin/docker-entrypoint.sh", "postgres"]
      [ecvisor] pid=1 boot argv after #!: ["/usr/bin/env", "bash", "/usr/local/bin/docker-entrypoint.sh", "postgres"]
      ... env runs, execs bash, 2 program bring-ups ...
      *** longjmp causes uninitialized stack frame ***: terminated
      [ecvisor] fatal: guest trap ... at PC 0x2021ae0 (__remill_error)
      ```

      ❗ **The witness is BASH, not RubyGems**, which makes the reproducer far
      smaller than `ruby --yjit` -- and it means this is not a Ruby quirk but the
      general `setjmp`/`longjmp` path. It is now on the critical path for the
      flagship image rather than for one interpreter's opt-in feature.

      ⭐ **THE NEXT STEP, and it is dynamic rather than static.** Read
      `*__pointer_chk_guard` at the setjmp and again at the failing longjmp. If
      they differ, the question becomes what rewrites `.data.rel.ro` mid-run
      (`exec_into` restores libraries with pristine data by design). If they
      agree, the guard is exonerated and the remaining suspect is the GCS path
      **this glibc's setjmp contains and nothing in this tree has looked at**:
      `mov x16,#1; chkfeat x16; tbnz w16,#0,skip; mrs x2,gcspr_el0` at
      `0x1636648`. `chkfeat` is ARMv9.4 HINT-space, so a lifter that treats it as
      a NOP leaves `x16 = 1`, takes the branch and skips GCS -- correct BY LUCK.
      Anything that clears `x16` instead falls into an unimplemented system
      register read.
      ⚠️ Nothing above is a diagnosis. It is four facts and two named
      hypotheses, and the point of recording them is that the next attempt starts
      from the disassembly rather than from the stack-bounds theory the code had
      already refuted.
      — *evidence: `JOURNAL.md`, `2026-08-22 -- What a JIT guest does under
      ecvisor`, "Incidental" section*

## Multi-module modules (`.agents/docs/MULTIMODULE.md`), 2026-08-15

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
      - ❌ **"The images exist on this machine" WAS TRUE FOR ONE DAY AND IS NOW
        FALSE. Re-verified 2026-08-26: all four are ABSENT** --
        `raptormark-builder:llvm22`, `:llvm22-v2`,
        `raptormark-elfconv-base:llvm22-v2`, `:llvm22`. This note was written
        2026-08-22; the 2026-08-23 mass removal took "the whole `llvm22` line"
        (`LTM/recovery-and-builder-provenance.md`). The line is still first-class
        in the CODE -- the `--llvm 22` flag and the README section are unchanged
        -- but nothing on this machine can run it, and re-creating a base is the
        expensive path, not a side build.
      - (original, now false) The images exist on this machine:
        `raptormark-builder:llvm22` and `:llvm22-v2` (2026-07-31),
        `raptormark-elfconv-base:llvm22-v2` (2026-08-12). `:llvm22-v2` carries
        **both** `/usr/lib/llvm-16` and `/usr/lib/llvm-22` with
        `ECV_LLVM_VER=22`.

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

## Diagnostics that read guest text from the arena, 2026-08-19

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

## Found while measuring the clock, 2026-08-21

## Browser host and re-entrant runtime follow-ups, 2026-08-21

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

- [ ] **Add browser transport capabilities only when a workload requires them.** Current limits are no connection reuse, response streaming, relay UDP, inbound relay listener, or demonstrated guest TLS path.
      — *source: browser networking entries from 2026-08-20 through 2026-08-21*

- [ ] **Narrow `Service-Worker-Allowed: /` if `internal/serve` becomes general-purpose.** The broad scope fits a server dedicated to `web/`, not a reused server.
      — *source: `2026-08-21 -- The bundle back under dist/, and the header that makes it possible`*

- [x] **✅ CLOSED 2026-08-24 by a `reconcile-journal-ltm` pass. LTM has no WASIX
      or wasmer document at all.** Both halves are done:
      `.agents/docs/LTM/wasix-and-wasmer.md` carries the measured ABI and its
      five silent traps, the SCOPED reading of the Wasm-2.0 rule, the
      PIC-Rust-std wall, and the `proc_fork` / `proc_spawn2` verdicts; and
      `LTM/INDEX.md` gained a "Related Documents Outside LTM" table pointing at
      `WASIX_ABI.md`, `MULTIMODULE.md` and `QUALITY_GATE.md`.
      ⚠️ The LTM document is a SUMMARY. `.agents/docs/WASIX_ABI.md` remains the
      measured record with the probes, and the probes themselves are in
      `.agents-workspace/wasmer/` -- do not re-derive an ABI fact from the
      summary when the measurement is one file away.
      Original text: `grep -rn -iE "wasix|wasmer|dylink" .agents/docs/LTM/`
      returned **zero hits**, while the arc spanned a dozen JOURNAL entries, a
      README status section, `.agents/docs/WASIX_ABI.md` and a shipped net
      backend.

- [x] **✅ CLOSED 2026-08-26 by the user: the patch is COMMITTED.** Verified,
      not assumed -- `git ls-files patches/` lists
      `0067-suspend-check-can-go-back-to-a-call.patch`, `git log` attributes it
      to `bcad8a3`, and `git status` reports nothing untracked under `patches/`.
      The exposure below is therefore gone: a fresh clone now gets the whole
      series, so a base rebuilt anywhere gets a lifter that can do
      `--suspend-via-call`.
      ⚠️ Kept rather than deleted because the FAILURE SHAPE is the durable part
      and generalises to the next untracked patch: `patches/*.patch` is a GLOB,
      so a missing file subtracts silently and the base still builds.

- [ ] **(original framing) `patches/0067-suspend-check-can-go-back-to-a-call.patch` is UNTRACKED,
      and `patches/*.patch` is globbed by every base build.** Recorded
      2026-08-24. A base rebuilt on a machine without it silently produces a
      lifter that cannot do `--suspend-via-call` -- and the failure does not
      arrive at the lift. It arrives at the WASIX loader, hours later, as
      `Expected import to be a function: 'env'.__ecv_unwinding`, or worse: with
      `ecv_globals_pic.c` in the link, `--allow-undefined` resolves the suspend
      read to zero and the guest never suspends, which is a HANG rather than a
      link error.
      The predicament predates this work; what is new is that a SHIPPING profile
      now depends on it. Committing is the user's call, so this entry exists to
      make the exposure visible rather than to take it.

- [x] **✅ DELETED 2026-08-25 on the user's instruction.
      `raptormark-builder:wasixneut` was a deliberately-broken neutralization
      build.** It carried a flipped port endianness in `net::wasix`, so anything
      run against it failed its socket tests for a reason that was correct and
      looked like a regression.
      Removed with a targeted `docker rmi`, **not** a prune -- verified first
      that `wasixneut` was the ONLY tag on image id `7e3155bbdf4e`, so nothing
      else lost a name, and confirmed afterwards that every other builder and the
      patched base are still present. That distinction is the whole of
      `AGENTS.md`'s Docker rule: the images are the only copies of things that
      exist nowhere else, and a prune cannot be aimed.
      ⚠️ The hazard it represented is gone with it, but the general form is not:
      a neutralization build is INDISTINGUISHABLE from a regression at the point
      you read the test output. Check the builder tag before diagnosing a red
      suite, whatever the tag is.

- [x] **✅ CLOSED 2026-08-25. ALL FOUR PROFILES NOW EPOLL A SOCKET.** Done the
      way the entry asked -- `epollSocketGuestSrc` was **moved**, not copied, to
      the new `e2e/epollsock_test.go`, and three tests joined the wasix one:

      | profile | runtime | result |
      |---|---|---|
      | `wasmedge` (SHIPPING) | wasmedge | PASS first time |
      | `loopback` | stock wasmtime | **FAILED, and found a real defect** |
      | `browser` | Node, `--reentrant --net-v1` | PASS first time |
      | `wasix` | wasmer `--net` | PASS (pre-existing) |

      ❗ **The loopback failure is the reason this was worth doing, and it was
      not in `ready` at all.** `FAIL getsockname gave a real port (errno=0)`:
      `net::loopback::bind` stored the address VERBATIM, so a `bind` to port 0
      stayed bound to port 0. Every earlier loopback test named a port, so
      nothing had bound 0 -- and this guest only binds 0 because two profiles'
      runs must not collide on a fixed one, which `AGENTS.md` warns about for
      unrelated reasons. The bug was found by the fixture's SAFETY property, not
      by its subject.
      It failed silently in both directions: `find_listener` matches 0 against 0
      as happily as any other number, so connections still worked and only the
      advertised port was wrong; and two sockets binding 0 both became "bound to
      0", with `find_listener` returning the FIRST for both -- a silent
      cross-connect between two unrelated servers.
      Fixed in `runtime/src/net/loopback.rs`: `bind` assigns from Linux's
      default ephemeral range (32768..=60999), lowest free first, so the
      assignment is deterministic and therefore assertable. Three host tests
      guard it, and the neutralization fired on two of them with the intended
      diagnostics -- `an_explicit_port_is_left_alone` is the control that
      refuses a fix which assigns unconditionally.
      ⚠️ **The differential is the strong part**: the staged `loopback`
      `libecvisor.a` DIFFERS from the previous image's while the default
      `libecvisor.a` is BYTE-IDENTICAL (`47203e76...` both), so the shipping
      profile is provably untouched by this change.
      Built as `raptormark-builder:lbport`, layered onto
      `raptormark-elfconv-base-patched:wasix` with `BASE_ID`/`TRANSLATE_SH`
      passed verbatim -- both labels verified identical to
      `raptormark-builder:wasixnet`, and every lift was served from the object
      cache, which is that reuse property working as designed.
      Original text follows.

      **`NetBackend::ready` is exercised end-to-end on ONE profile only.**
      Found 2026-08-24: nothing in `e2e/` epolled a socket at all until
      `epollSocketGuestSrc`, so the `fd_ready` socket arm -- the only caller of
      `NetBackend::ready` -- was unreached by any run on any profile.
      `timers_test.go` epolls an *eventfd*, and every socket test blocks in
      `accept`/`connect`, which the scheduler serves through `wait` instead.
      The new guest is written against nothing WASIX-specific and closes the gap
      for `wasix` only.
      ⚠️ The SHIPPING profile is still uncovered there, and its `ready` is the
      one guarding the PostgreSQL postmaster ServerLoop deadlock (a listener
      reported perpetually readable makes the guest `accept()` on an empty
      backlog and block forever). Lift `epollSocketGuestSrc` out of
      `e2e/wasixnet_test.go` and run it under wasmedge, loopback and browser
      too -- it is one lift per profile, and the second caller is what should
      move it rather than copy it.

- [ ] **A guest resolving a real NAME under `--profile wasix` has not been
      demonstrated.** Added 2026-08-24 with WASIX sockets. The backend carries
      UDP -- `TestWasixProfileCarriesDatagrams` sends, receives and replies to
      the reported source -- but only over loopback, so nothing shows that
      `wasmer run --net` permits outbound UDP to port 53. wasmer's `--net`
      accepts filter rules (`dns:allow=example.com:80`, `ipv4:allow=…`), so a
      real deployment may have to grant it explicitly.
      Needs a guest calling `getaddrinfo`, which needs `/etc/resolv.conf` and
      `/etc/nsswitch.conf` in the rfs sidecar -- so it is a `raptormark build`
      test rather than a `liftOne` one.
      ❌ Do NOT close this by wiring `net::dns` into the wasix backend. That
      module exists because a browser has no UDP at all; here it would replace a
      working transport with a synthesised answer and hide what the sandbox is
      really doing.

## Found by an autonomous check, 2026-08-25

- [x] **✅ RESOLVED 2026-08-25 by rewriting the tests against a fixture the
      suite BUILDS. `raptormark-tmp-ossldgst:latest` was absent and is not
      recoverable.**

      The image is genuinely gone, not merely untagged -- all 57 distinct
      pre-wipe image ids were inspected for an `openssl`/`dgst` entrypoint and
      none has one, against a control that finds two `postgres` entrypoints in
      the same sweep.

      **What replaced it.** `osslFixture` is now `raptormark-e2e-ossldgst:v1`,
      built from `osslFixtureDockerfile` in `e2e/e2e_test.go` off a
      DIGEST-PINNED `debian` base. `requireFixture` BUILDS it and `t.Fatal`s if
      the build fails -- it no longer skips. That matters more than it reads:
      when the old image was lost, three tests went permanently quiet and
      reported it as a SKIP, which is `0 fail` with coverage silently gone.

      ❗ **THE PROPERTY THAT DID NOT SURVIVE, stated in the code so a
      reproducible fixture cannot quietly inherit a historical claim.** The old
      image was PRE-WIPE -- "the closest thing the project has to a record of
      what already worked" -- which let a failure be read as "we regressed"
      rather than "this input is new and hard". The replacement is built from
      today's Debian and cannot make that distinction. Said plainly on
      `osslFixture`.

      ✅ **Two neutralizations, and the first one FOUND A DEFECT IN MY OWN
      RECIPE.** The Dockerfile initially ran `strip --strip-all` on libcrypto,
      on the assumption the fixture had to arrange stripping. Rebuilding with
      that line removed **PASSED** -- because Debian already ships stripped.
      Measured: libcrypto.so.3 is 6,302,952 bytes with `.symtab` absent either
      way. The strip was a no-op that cost an apt install, an apt purge, and two
      stray scripts the purge left behind (visible as 132 vs 130 scripts).
      Removed, and `assertFixtureIsHard` now VERIFIES the property instead of
      the recipe pretending to cause it.
      The second neutralization inverted all three of its arms and each fired
      with its intended diagnostic -- behavioural, not a compile error.

      **What is asserted now**, measured on the built fixture rather than
      assumed from the recipe: libcrypto `.symtab` absent / `.dynsym` present,
      and libc carrying `.relr.dyn`. ⚠️ RELR is GLIBC's here, not libcrypto's --
      libcrypto has only `.rela.dyn` -- so a reader checking the wrong library
      will conclude the property was lost in the rewrite. It was never
      libcrypto's.

      ✅ **PROVEN END TO END 2026-08-25**, not just fused: the new fixture goes
      through discovery, fusing, ecvisor translation, the rfs sidecar and a run
      that reproduces the real SHA-256 of `/etc/os-release`.
      `TestOpenSSLFixtureEcvisorEndToEnd` PASS in 490 s, of which a genuinely
      COLD 7m40s is translation (the log says "translated", not "served from the
      object cache"), over a 33.9 MB sidecar of 2,687 files.
      ⚠️ **The "~30 min" in that test's skip message was measured on the LOST
      pre-wipe fixture and has been replaced with the measurement above.** An
      inflated cost estimate is what stops a gate being run, and this one is now
      a quarter of what its own skip message claimed.

- [x] **✅ DONE 2026-08-25. All 13 code citations of `.agents/docs/JOURNAL.md`
      resolved, one at a time.** No live citation points at that file any more;
      the four remaining mentions are notes recording that a target is gone.

      | outcome | n | where |
      |---|---|---|
      | repointed into `LTM/` | 6 | `fuse.go`, `image.go`, `run.go`, `rfs.go`, and two in `e2e_test.go` |
      | target gone from JOURNAL **and** LTM | 4 | `arena.rs`, `context.rs`, `sys.rs`, `e2e_test.go` (`__wrap_main`) |
      | claim itself was STALE | 1 | `net_test.go` -- see below |
      | not a citation | 2 | already-fixed skip messages |

      ❗ **For the 4 whose target is gone, the citation now says so AT THE
      CITATION rather than being deleted.** A deleted pointer looks like the
      detail was never there. In three of them the surrounding comment already
      carried the substance -- the malloc doubling ladder
      (`0.75+1.5+3+6+12+24+48 MiB`), the nginx `setuid(101) failed (38)`
      symptom, the "39,893 lines" instrument story -- so those comments are now
      the ONLY record and are marked "do not trim to a summary".

      ❗ **`e2e/net_test.go` was not a pointer problem at all: THE CLAIM WAS
      FALSE.** It said the ecvisor half of `TestNetGuestsNativeContract` could
      not be built because "socketpair/sendmsg/recvmsg are absent ... the ecvisor
      side is added when they land". They landed. All three are dispatched
      (`sys.rs:697-699`) and implemented, not stubbed (`sys_socketpair` :4653,
      `sys_sendmsg` :4714, `sys_recvmsg` :4812), and `e2e/uds_test.go` exercises
      guest AF_UNIX end to end.
      The comment now says the ecvisor half is UNBUILT, not BLOCKED. ⚠️ That is
      a different thing and worth the words: the old wording read as a standing
      impossibility and would stop anyone trying. See the new entry below for
      what building it would actually take.

- [x] **✅ DONE 2026-08-25 for the tractable third. `e2e/netfork_test.go` is the
      ecvisor half of `TestNetGuestsNativeContract`.** Two tests, both green,
      against the native baseline that runs the SAME source on this host:
      `TestNetForkServerUnderEcvisor` (shipping profile, wasmedge) and
      `TestNetForkServerUnderLoopback` (in-process network, stock wasmtime).

      **Only `netForkServerSrc` was lifted, and the other two are still open** --
      see the entry below. That guest binds port **0**, learns the port with
      `getsockname` and is its own peer across a `fork`, so nothing outside it
      can make it flaky.

      ❗ **It covers an INTERACTION no other e2e guest reaches**: sockets AND
      fork in one process, with the two ends of a connection on either side of
      the fork. `uds_test.go` is AF_UNIX, the socket tests do not fork, and the
      fork tests do not open sockets.
      ⚠️ The loopback run was expected to be the one that could legitimately
      fail -- `net::loopback` keeps its sockets in a `Vec` inside the runtime, so
      "the child connects to the port the parent bound" is a question about that
      table's interaction with ecvisor's fork. It passes, which also exercises
      the same-day ephemeral-port fix THROUGH a fork: before it, the child would
      have dialled port 0, which `find_listener` matched, so the guest would have
      passed while every real server advertised port 0.

      ✅ Neutralized behaviourally: the guest made to exit 0 while printing
      nothing, and the banner check fired. That also measured that neither
      wasmedge nor wasmtime emits "ok" itself -- after which the assertion was
      still tightened from `"ok"` to `"ok\n"`, because bare "ok" is a substring
      of too much and being green today is a property of two current releases,
      not a guarantee.

- [x] **✅ DONE 2026-08-25 on the user's decision: the net pair is rewritten to
      coordinate ports, and all three guests now have ecvisor halves.**

      The decision recorded above was the user's to make and they made it. The
      guests no longer use fixed ports:
        * `netServerSrc` binds **0**, reads the assignment back with
          `getsockname` and announces `PORT <n>` on stdout -- which is both the
          address AND the readiness signal, so `dialWithRetry`'s guesswork is no
          longer load-bearing.
        * `netClientSrc` takes the port on **argv** and REFUSES to default. A
          default would let a misconfigured run dial something plausible and fail
          elsewhere.

      **What made it possible was a harness limit, not a guest one.** `runGuest`
      buffered stdout and returned it only after `Wait()`, so there was no way to
      learn an ephemeral port in time -- the peer would have waited for a process
      that was itself blocked in `accept()` waiting for the peer. `streamPeer`
      now hands the peer a `nextLine` that reads stdout WHILE the guest runs.
      ⚠️ The fixed ports were that limit's CONSEQUENCE, not its cause.

      | test | what it covers |
      |---|---|
      | `TestNetServerUnderEcvisor` | lifted guest LISTENS, host peer connects |
      | `TestNetClientUnderEcvisor` | lifted guest DIALS OUT to a host listener |

      ❗ `TestNetClientUnderEcvisor` is the only thing exercising `sock_connect`
      against a listener **ecvisor does not own**. Every other socket test either
      binds and accepts, or connects to something inside the same guest.

      ⚠️ Both use `--network host`, which is only safe BECAUSE nothing is fixed
      any more -- that combination was the whole obstacle.
      Neutralized: wrong reply / wrong payload each caught. The NATIVE baseline
      was re-run at every step and stayed green, including after `runGuest` was
      refactored to share `streamPeer`; it is the trusted half and a rewrite that
      quietly changed what it proves would have been the real cost.

      ❗ **AND ONE REGRESSION IT CAUSED, caught by the full suite.**
      `TestWasixProfileServesTheSocketABI` compiles the SAME two guest sources,
      and its fixed port is load-bearing for a different reason: "THE PORT IS THE
      ASSERTION ... if the encode side byte-swapped it, the bind would SUCCEED on
      26810 and this dial would time out". **`bind(0)` has no port to encode**, so
      converting it would have left it green while testing nothing.
      Fixed by making `netServerSrc`'s port OPTIONAL -- absent binds 0 and
      announces (coordination), present binds exactly that (encode coverage) --
      rather than choosing one mode for both callers.
      ⚠️ The miss: the blast radius of the HARNESS was checked, the blast radius
      of the GUEST SOURCES was not. Grepping for callers of a function is habit;
      grepping for users of a shared test CONSTANT is not, and a fixture is data.

      **Two harness defects found by running it, both invisible to review:**
        * `io.Pipe` is SYNCHRONOUS, so teeing the guest's stdout through it made
          every write block until someone read. The forkserver's peer reads
          nothing, so the guest deadlocked on its first write and the suite HUNG
          rather than failed. Fixed with a drained `io.TeeReader` and a
          non-blocking send.
        * The wasm runtime writes to the guest's stdout: `wasmedge --enable-all`
          opens with "component model is enabled", which the first version took
          for the port line. `portFromStream` skips chatter, **bounded at 20
          lines** -- scanning forever would turn a guest that never announces a
          port into a hang, the one failure shape with no diagnostic.

- [x] **✅ ANSWERED 2026-08-25 by the user: `_recovery/` is GONE, and no copy
      survives.** Documents corrected rather than left describing it in the
      present tense: `AGENTS.md` (the "never clean it up" rule),
      `LTM/recovery-and-builder-provenance.md` (three places, including the
      sentence that called it "preserved") and
      `LTM/agent-harness-and-quality-gate.md`.

      **What is recoverable about its contents, and it is not much.** Two names,
      from surviving references: `_recovery/RECOVERY.md` and
      `_recovery/reference/`. The first was a distinct evidence file, NOT the
      recovery journal -- that was `RECOVERY.md` at the repo ROOT until
      2026-08-09 and is now `.agents/docs/JOURNAL.md`, a different document that
      never held the same material. Anyone conflating the two will think the
      evidence survived.
      What that evidence ESTABLISHED does survive, in
      `LTM/recovery-and-builder-provenance.md`: which components are recovered
      versus reconstructed, and how the patched elfconv tree was validated. What
      is lost is the ability to RE-READ it and check a new question against it.

      ❗ **THE LESSON, and it is the reusable part.** All four references to
      `_recovery/` -- `.gitignore`, `.bazelignore`, `BUILD.bazel`'s
      `gazelle:exclude`, `internal/builder/workspace_test.go`'s `SkipDir` arm --
      are EXCLUSIONS. Every one told a tool to ignore it; not one required it to
      exist. So every gate stayed green while it vanished and nothing ever
      reported it. **A "do not delete this" rule in a document is not a guard.**
      The same shape lost `raptormark-tmp-ossldgst:latest` in the same week --
      that one at least announced itself as a skip; this announced itself
      through nothing.
      ⚠️ The exclusions are LEFT IN PLACE deliberately: they cost nothing and
      are what a re-derived `_recovery/` would need.

- [x] **✅ DONE 2026-08-25. `raptormark preserve` is the guard.**
      `internal/preserve` + `.agents/preserve.json`, 9 host tests, Bazel 14/14.

      **The design decision that matters, because the obvious build is wrong.**
      A check for ABSENCE fails on every fresh clone, where nothing is present
      and nothing has been lost. A check that cries wolf gets deleted, leaving
      no check -- which is where this started. So it detects DISAPPEARANCE
      against a recorded baseline:

      | state | answer |
      |---|---|
      | recorded and present | ok |
      | recorded and MISSING | ❗ non-zero exit -- the event nothing reported |
      | not recorded | "nothing is known", said plainly -- not an all-clear |

      ⚠️ The manifest lives in `.agents/`, **not** `.agents-workspace/`, and is
      meant to be committed. A manifest kept beside what it guards dies with it,
      which is exactly how the `_recovery/` references failed -- every one was in
      a file perfectly happy in its absence.
      ❗ `snapshot` REFUSES to record something already missing: a manifest
      listing a lost thing fails forever, and a check that can never pass is one
      somebody switches off.

      ✅ **Proven against the real losses.** Given a manifest naming
      `_recovery/` and `raptormark-tmp-ossldgst:latest`, `preserve check`
      reports both and exits 1. Had it existed, neither loss would have been
      silent.
      Baseline recorded on this machine: 5 entries -- the patched base,
      `raptormark-builder:lbport`, `.agents-workspace/{fixtures,drivers,objcache}`.

      ⚠️ **One of its own guards was nearly mis-read as vacuous.** The
      unknown-kind neutralization "passed", which looked like the assertion
      observing nothing. It was the NEUTRALIZATION that was wrong -- the
      injected `s.Present = true` sat one line above the original
      `s.Present = false`, which overwrote it immediately. Re-run against the
      real assignment, the guard fired. A neutralization that does not neutralize
      is indistinguishable from a test that does not test.

      ❗ **What this does NOT solve: nobody is obliged to run it.** It is a
      command, not a gate. Wiring it into the e2e suite was considered and NOT
      taken -- a fresh machine legitimately has none of these, so a suite failure
      there would be wrong, and gating it on "manifest exists" reintroduces the
      skip-shaped silence. The honest position is that it turns an undetectable
      loss into a detectable one, and someone still has to look.
      there.
