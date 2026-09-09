"""Run a CLI in a controlling terminal, answer its test approval, capture output."""
import json
import os
import pty
import select
import signal
import sys
import time

pid, fd = pty.fork()
if pid == 0:
    os.execvpe(sys.argv[1], sys.argv[1:], os.environ)

output = bytearray()
answered = False
status = None
try:
    deadline = time.monotonic() + float(os.environ.get("PLY_TEST_TIMEOUT", "15"))
    while time.monotonic() < deadline:
        if select.select([fd], [], [], 0.1)[0]:
            try:
                data = os.read(fd, 65536)
            except OSError:
                break
            if not data:
                break
            output.extend(data)
            if b"Allow? [y/N]" in output and not answered:
                os.write(fd, os.environ.get("PLY_TEST_REPLY", "y").encode() + b"\n")
                answered = True
        child, result = os.waitpid(pid, os.WNOHANG)
        if child:
            status = result
            # Drain any output that was buffered immediately before exit.
            while select.select([fd], [], [], 0)[0]:
                try:
                    data = os.read(fd, 65536)
                except OSError:
                    break
                if not data:
                    break
                output.extend(data)
            break
    if status is None:
        child, result = os.waitpid(pid, os.WNOHANG)
        if child:
            status = result
    print(json.dumps({"output": output.decode(errors="replace"), "answered": answered,
                      "exit": os.waitstatus_to_exitcode(status) if status is not None else None}))
finally:
    if status is None:
        try:
            os.killpg(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        os.waitpid(pid, 0)
    os.close(fd)
