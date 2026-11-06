"""Signal handling shared by resource-owning integration launchers."""

from contextlib import contextmanager
import signal


class LauncherInterrupted(Exception):
    """Cancellation must escape selectors' InterruptedError/EINTR retry path."""


@contextmanager
def cleanup_on_interrupt():
    """Turn cancellation into stack unwinding through the owner's finally."""
    previous = {}

    def interrupt(number, _frame):
        raise LauncherInterrupted(number)

    try:
        for number in (signal.SIGINT, signal.SIGTERM):
            previous[number] = signal.signal(number, interrupt)
        yield
    finally:
        for number, handler in previous.items():
            signal.signal(number, handler)


@contextmanager
def finish_process_start():
    """Keep newly started processes owned before delivering cancellation."""
    pending, previous = [], {}

    def remember(number, _frame):
        pending.append(number)

    try:
        for number in (signal.SIGINT, signal.SIGTERM):
            previous[number] = signal.signal(number, remember)
        yield
    finally:
        for number, handler in previous.items():
            signal.signal(number, handler)
        if pending:
            raise LauncherInterrupted(pending[0])
