#!/usr/bin/env python3
"""
Daemon launcher for ms-login service.
Fork twice to fully detach from controlling terminal.
"""
import os
import sys
import time
import subprocess
from pathlib import Path

HERE = Path(__file__).parent.resolve()
LOG_FILE = "/tmp/ms_login.log"
PID_FILE = "/tmp/ms_login.pid"


def daemonize():
    """Standard double-fork daemon."""
    # First fork
    if os.fork() > 0:
        sys.exit(0)
    os.setsid()
    # Second fork
    if os.fork() > 0:
        sys.exit(0)
    # Redirect stdio
    sys.stdout.flush()
    sys.stderr.flush()
    log_fd = os.open(LOG_FILE, os.O_RDWR | os.O_CREAT | os.O_TRUNC, 0o644)
    devnull_fd = os.open("/dev/null", os.O_RDONLY)
    os.dup2(devnull_fd, 0)
    os.dup2(log_fd, 1)
    os.dup2(log_fd, 2)


def main():
    daemonize()
    # Write PID
    with open(PID_FILE, "w") as f:
        f.write(str(os.getpid()))
    # Now exec the service
    os.execv(
        "/home/z/.venv/bin/python3",
        ["python3", str(HERE / "service.py")],
    )


if __name__ == "__main__":
    main()
