"""Pty regression test for the maelle CLI raw-mode line editor.

Usage: pty_editor_test.py <cli-binary> <base-url> <tmp-home>

Drives the CLI through a real pty and asserts on what the editor actually
submitted (via /history) — this is what catches input-loop bugs like the
backspace spin, which piped-stdin tests cannot see.
"""
import os
import pty
import select
import sys
import time

BIN, BASE, HOME = sys.argv[1], sys.argv[2], sys.argv[3]
os.makedirs(HOME, exist_ok=True)

passed = failed = 0


def check(name, cond):
    global passed, failed
    print(("PASS  " if cond else "FAIL  ") + name, flush=True)
    if cond:
        passed += 1
    else:
        failed += 1


def read_all(fd, timeout=2.0, extend=0.4):
    out = b""
    end = time.time() + timeout
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.2)
        if r:
            try:
                chunk = os.read(fd, 65536)
            except OSError:
                break
            if not chunk:
                break
            out += chunk
            end = time.time() + extend
    return out


def main():
    env = dict(os.environ)
    env.update({
        "HOME": HOME,
        "MAELLE_PROXY_KEY": "dev-secret",
        "MAELLE_URL": BASE,
        "TERM": "xterm-256color",
        "MAELLE_HISTORY": os.path.join(HOME, "hist"),
        "MAELLE_CONFIG": os.path.join(HOME, "config.json"),
    })
    pid, fd = pty.fork()
    if pid == 0:
        os.execvpe(BIN, [BIN], env)

    read_all(fd, 3)  # banner + first prompt

    # 1. backspace at end of line: "hello" + BS + BS + "p" -> "help"
    os.write(fd, b"hello")
    time.sleep(0.3)
    read_all(fd, 1)
    os.write(fd, b"\x7f\x7f")
    time.sleep(0.3)
    out = read_all(fd, 1)
    check("two backspaces delete exactly two chars",
          out.count(b"\x1b[2K") == 2 and out.endswith(b"hel"))
    os.write(fd, b"p\r")
    read_all(fd, 5)  # chat round trip

    # 2. mid-line edit: "abcd" + left + left + BS + "X" -> "aXcd"
    os.write(fd, b"abcd\x1b[D\x1b[D\x7fX\r")
    read_all(fd, 5)

    # 3. backspace spam on empty line must not wedge the editor
    os.write(fd, b"\x7f\x7f\x7f\x7f\x7f")
    time.sleep(0.3)
    os.write(fd, b"ok\r")
    read_all(fd, 5)

    # 4. Ctrl-U kills the line, then retype
    os.write(fd, b"junkjunk\x15clean\r")
    read_all(fd, 5)

    # 5. up-arrow recalls the last submitted line ("clean"), Ctrl-C cancels it
    os.write(fd, b"\x1b[A")
    time.sleep(0.3)
    out = read_all(fd, 1)
    check("up-arrow recalls history", b"clean" in out)
    os.write(fd, b"\x03")
    out = read_all(fd, 1)
    check("ctrl-c cancels line", b"^C" in out)

    # 6. what actually got submitted:
    os.write(fd, b"/history\r")
    hist = read_all(fd, 3).decode("utf-8", "replace")
    for want in ["help", "aXcd", "ok", "clean"]:
        check("submitted contains %r" % want, want in hist)
    for bad in ["hello", "abcd", "junkjunk"]:
        check("submitted does not contain %r" % bad, bad not in hist)

    # 7. Ctrl-D on an empty line exits cleanly (process must not be wedged)
    os.write(fd, b"\x04")
    read_all(fd, 2)
    exited = False
    for _ in range(20):
        wpid, status = os.waitpid(pid, os.WNOHANG)
        if wpid:
            exited = os.WIFEXITED(status) and os.WEXITSTATUS(status) == 0
            break
        time.sleep(0.2)
    check("ctrl-d exits with code 0", exited)

    print("passed: %d, failed: %d" % (passed, failed))
    sys.exit(1 if failed else 0)


main()
