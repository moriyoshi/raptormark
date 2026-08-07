//! `#!` interpreter resolution, for `execve` and for the boot program.
//!
//! # Why this exists
//!
//! `internal/image` treats scripts as first-class exec targets: `Scan`
//! inventories them, `Closure` walks them and pulls in their interpreter and
//! everything they invoke, and `image.Script`'s doc comment has said since it
//! was written that "the runtime's shebang handling execs the *interpreter* and
//! feeds it the script file, which comes from the rfs sidecar as data".
//!
//! ❗ **It did not. `sys.rs` returned ENOEXEC with the comment "shebang support
//! is a later addition", and the boot path fell back to program 0.** Measured
//! 2026-08-27 by building `postgres:17` end to end and running it: the guest
//! printed apt's `E: Invalid operation postgres`, because apt is program 0.
//!
//! That made every image whose ENTRYPOINT is a script unrunnable, which is
//! nearly all of them -- and it went unnoticed because the two nginx images name
//! their entrypoint with a leading slash, so they resolve as ordinary paths.
//!
//! # What Linux does, which is what this implements
//!
//! `execve("/s", ["s", "a"], env)` where `/s` begins `#!/bin/sh -x` becomes
//! `execve("/bin/sh", ["/bin/sh", "-x", "/s", "a"], env)`.
//!
//!   * the interpreter replaces argv[0];
//!   * the OPTIONAL single argument follows it -- Linux takes everything after
//!     the interpreter, up to the newline, as ONE argument, not as fields;
//!   * the SCRIPT PATH follows that, spelled as it was handed to `execve`, not
//!     canonicalised;
//!   * the caller's argv[1..] follows, and the caller's argv[0] is DISCARDED.
//!
//! Bounded at [`MAX_DEPTH`] levels, as Linux bounds it at `BINPRM_MAX_RECURSION`.
//! The first line is read from at most [`MAX_LINE`] bytes, matching
//! `BINPRM_BUF_SIZE`: a longer interpreter path is TRUNCATED by Linux rather
//! than rejected, and truncating to a path that does not exist is how the kernel
//! reports it, so that behaviour is reproduced rather than improved on.

/// How many `#!` levels to follow. Linux's `BINPRM_MAX_RECURSION`.
pub const MAX_DEPTH: usize = 4;

/// How much of the first line is read. Linux's `BINPRM_BUF_SIZE`.
pub const MAX_LINE: usize = 256;

/// The interpreter a `#!` line names, and its one optional argument.
#[derive(Debug, PartialEq, Eq)]
pub struct Shebang {
    pub interp: Vec<u8>,
    pub arg: Option<Vec<u8>>,
}

/// Parses a `#!` line out of a file's leading bytes.
///
/// `None` when the file does not begin with `#!`, or when the line names no
/// interpreter -- `#!` alone, or `#!   ` -- which Linux reports as ENOEXEC
/// rather than treating as a script with an empty interpreter.
///
/// ⚠️ Only the first [`MAX_LINE`] bytes are considered, and a file with no
/// newline inside them is still parsed: a script whose entire content is one
/// `#!` line and no trailing newline is legal and runs.
pub fn parse(bytes: &[u8]) -> Option<Shebang> {
    let head = &bytes[..bytes.len().min(MAX_LINE)];
    if !head.starts_with(b"#!") {
        return None;
    }
    // Up to the first newline. A carriage return is NOT special: Linux does not
    // strip it, and a CRLF script therefore names an interpreter ending in \r
    // and fails to find it. Reproduced deliberately -- silently accepting CRLF
    // here would make this runtime succeed where the real kernel fails, which is
    // the harder difference to debug.
    let line = match head.iter().position(|&c| c == b'\n') {
        Some(i) => &head[2..i],
        None => &head[2..],
    };

    let is_space = |c: u8| c == b' ' || c == b'\t';
    let start = line.iter().position(|&c| !is_space(c))?;
    let rest = &line[start..];
    let end = rest.iter().position(|&c| is_space(c)).unwrap_or(rest.len());
    let interp = &rest[..end];
    if interp.is_empty() {
        return None;
    }

    // ❗ ONE argument, not fields. `#!/bin/sh -e -x` passes the single argument
    // "-e -x" to /bin/sh, which is why a shebang cannot carry two flags. Getting
    // this wrong would make scripts work here that do not work on Linux.
    let tail = &rest[end..];
    let arg_start = tail.iter().position(|&c| !is_space(c));
    let arg = arg_start.map(|i| {
        let a = &tail[i..];
        // Trailing whitespace is not part of the argument.
        let e = a.iter().rposition(|&c| !is_space(c)).map_or(0, |p| p + 1);
        a[..e].to_vec()
    });

    Some(Shebang {
        interp: interp.to_vec(),
        arg: arg.filter(|a| !a.is_empty()),
    })
}

/// Rewrites `argv` for one `#!` level, Linux-style, and returns the new argv.
///
/// `script` is spelled as the caller spelled it. The caller's `argv[0]` is
/// dropped -- the interpreter's name takes its place -- and everything from
/// `argv[1]` on is preserved.
///
/// ⚠️ An EMPTY incoming argv is not an error. `execve` with an empty argv is
/// legal (and rare), and the result must still name the interpreter and the
/// script, or the interpreter would be handed nothing to run.
pub fn rewrite_argv(sb: &Shebang, script: &[u8], argv: &[Vec<u8>]) -> Vec<Vec<u8>> {
    let mut out = Vec::with_capacity(argv.len() + 3);
    out.push(sb.interp.clone());
    if let Some(a) = &sb.arg {
        out.push(a.clone());
    }
    out.push(script.to_vec());
    out.extend(argv.iter().skip(1).cloned());
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sb(interp: &str, arg: Option<&str>) -> Shebang {
        Shebang {
            interp: interp.into(),
            arg: arg.map(|a| a.into()),
        }
    }

    #[test]
    fn parses_the_common_forms() {
        assert_eq!(parse(b"#!/bin/sh\necho hi\n"), Some(sb("/bin/sh", None)));
        assert_eq!(
            parse(b"#!/bin/bash -x\n"),
            Some(sb("/bin/bash", Some("-x")))
        );
        // The `env` idiom, which is why the argument matters at all.
        assert_eq!(
            parse(b"#!/usr/bin/env python3\n"),
            Some(sb("/usr/bin/env", Some("python3")))
        );
        // Leading space after `#!` is allowed and common.
        assert_eq!(parse(b"#! /bin/sh\n"), Some(sb("/bin/sh", None)));
        assert_eq!(
            parse(b"#!\t/bin/sh\t-e\t\n"),
            Some(sb("/bin/sh", Some("-e")))
        );
        // No trailing newline: a one-line script is still a script.
        assert_eq!(parse(b"#!/bin/sh"), Some(sb("/bin/sh", None)));
    }

    /// ❗ ONE argument, not fields. `#!/bin/sh -e -x` gives /bin/sh the single
    /// argument "-e -x". A parser that split on whitespace would accept scripts
    /// here that Linux rejects, which is the wrong direction to differ in.
    #[test]
    fn the_argument_is_one_string_not_fields() {
        assert_eq!(
            parse(b"#!/bin/sh -e -x\n"),
            Some(sb("/bin/sh", Some("-e -x")))
        );
    }

    #[test]
    fn rejects_non_scripts_and_empty_interpreters() {
        assert_eq!(parse(b"\x7fELF..."), None);
        assert_eq!(parse(b"echo hi\n"), None);
        assert_eq!(parse(b"#"), None);
        assert_eq!(parse(b"#!\n"), None);
        assert_eq!(parse(b"#!   \n"), None);
        assert_eq!(parse(b""), None);
    }

    /// ⚠️ CRLF is deliberately NOT accommodated: Linux does not strip the `\r`,
    /// so the interpreter name carries it and the exec fails. Accepting it would
    /// make this runtime succeed where the kernel fails.
    #[test]
    fn crlf_is_not_stripped() {
        assert_eq!(parse(b"#!/bin/sh\r\n"), Some(sb("/bin/sh\r", None)));
    }

    /// The line is bounded, and truncation is the kernel's behaviour too.
    #[test]
    fn the_line_is_bounded() {
        let mut s = b"#!/".to_vec();
        s.extend(core::iter::repeat(b'a').take(MAX_LINE * 2));
        s.push(b'\n');
        let got = parse(&s).unwrap();
        assert_eq!(
            got.interp.len(),
            MAX_LINE - 2,
            "truncated to BINPRM_BUF_SIZE"
        );
    }

    #[test]
    fn rewrites_argv_the_way_linux_does() {
        // execve("/s", ["s", "a"]) with `#!/bin/sh -x`
        //   -> execve("/bin/sh", ["/bin/sh", "-x", "/s", "a"])
        let got = rewrite_argv(
            &sb("/bin/sh", Some("-x")),
            b"/s",
            &[b"s".to_vec(), b"a".to_vec()],
        );
        assert_eq!(
            got,
            vec![
                b"/bin/sh".to_vec(),
                b"-x".to_vec(),
                b"/s".to_vec(),
                b"a".to_vec()
            ]
        );
    }

    /// The script path is spelled as the CALLER spelled it, not canonicalised --
    /// `$0` inside the script must match what was executed.
    #[test]
    fn the_script_path_is_the_callers_spelling() {
        let got = rewrite_argv(&sb("/bin/sh", None), b"./run.sh", &[b"whatever".to_vec()]);
        assert_eq!(got, vec![b"/bin/sh".to_vec(), b"./run.sh".to_vec()]);
    }

    #[test]
    fn an_empty_argv_still_names_the_interpreter_and_script() {
        let got = rewrite_argv(&sb("/bin/sh", None), b"/s", &[]);
        assert_eq!(got, vec![b"/bin/sh".to_vec(), b"/s".to_vec()]);
    }
}
