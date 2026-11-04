#!/usr/bin/env python3
"""Observe a command or summarize GitHub Actions timings, using the standard library."""

from __future__ import annotations

import argparse
import datetime as dt
import json
import math
import os
from pathlib import Path
import resource
import selectors
import signal
import subprocess
import sys
import time


def write_json(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")
    temporary.replace(path)


class GoEvents:
    def __init__(self):
        self.active = set()
        self.tests = []
        self.packages = []

    def consume(self, line: bytes) -> None:
        try:
            event = json.loads(line)
        except (ValueError, UnicodeError):
            return
        if not isinstance(event, dict):
            return
        package, test, action = event.get("Package"), event.get("Test"), event.get("Action")
        if not isinstance(package, str) or not isinstance(action, str):
            return
        if test is not None and not isinstance(test, str):
            return
        key = (package, test)
        if action == "run" and isinstance(test, str):
            self.active.add(key)
        if action in {"pass", "fail", "skip"}:
            self.active.discard(key)
            elapsed = event.get("Elapsed")
            if isinstance(elapsed, (int, float)) and math.isfinite(elapsed) and elapsed >= 0:
                record = {"package": package, "action": action, "seconds": elapsed}
                if isinstance(test, str):
                    self.tests.append({**record, "test": test})
                else:
                    self.packages.append(record)


def process_sample(pid: int) -> dict:
    """Sample live Linux descendants; exited children aren't included in live CPU."""
    records = {}
    for path in Path("/proc").glob("[0-9]*/stat"):
        try:
            fields = path.read_text().rsplit(") ", 1)[1].split()
            records[int(path.parent.name)] = (
                int(fields[1]), int(fields[11]) + int(fields[12]), int(fields[21])
            )
        except (OSError, ValueError, IndexError):
            continue
    descendants = {pid}
    while True:
        added = {child for child, values in records.items() if values[0] in descendants}
        if added <= descendants:
            break
        descendants.update(added)
    selected = [values for child, values in records.items() if child in descendants]
    if not selected:
        return {}
    return {
        "live_processes": len(selected),
        "live_tree_cpu_seconds": round(sum(x[1] for x in selected) / os.sysconf("SC_CLK_TCK"), 3),
        "live_tree_rss_kib": sum(x[2] for x in selected) * os.sysconf("SC_PAGE_SIZE") // 1024,
    }


def stop_group(process: subprocess.Popen, grace: float) -> None:
    # Always signal the group even when its leader exited, so a child retaining
    # stdout or ignoring SIGTERM cannot escape the command's timeout.
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=grace)
    except subprocess.TimeoutExpired:
        pass
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    process.wait()


def observe(args) -> int:
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        raise ValueError("a command is required after --")
    report = Path(args.report)
    report.parent.mkdir(parents=True, exist_ok=True)
    events_path = report.with_suffix(".events.jsonl")
    log_path = report.with_suffix(".log")
    if report in {events_path, log_path}:
        raise ValueError("report path must differ from its .log and .events.jsonl sidecars")
    go = GoEvents()
    started, last_output = time.monotonic(), time.monotonic()
    usage_before = resource.getrusage(resource.RUSAGE_CHILDREN)
    timed_out, interrupted, error_kind = False, False, None
    peak_rss = 0
    process = None
    returncode = 127
    buffer = b""
    handlers = {}

    def interrupted_signal(number, _frame):
        raise InterruptedError(number)

    with events_path.open("w") as events, log_path.open("wb") as log:
        def emit(kind, **values):
            payload = {"event": kind, "label": args.label, "elapsed_seconds": round(time.monotonic() - started, 3), **values}
            text = json.dumps(payload, ensure_ascii=False)
            events.write(text + "\n")
            events.flush()
            print(text, file=sys.stderr, flush=True)

        try:
            for number in (signal.SIGINT, signal.SIGTERM):
                handlers[number] = signal.signal(number, interrupted_signal)
            process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
            emit("start", pid=process.pid, timeout_seconds=args.timeout_seconds)
            os.set_blocking(process.stdout.fileno(), False)
            next_heartbeat = started + args.heartbeat_seconds
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                while selector.get_map() or process.poll() is None:
                    now = time.monotonic()
                    if now - started >= args.timeout_seconds:
                        timed_out = True
                        emit("timeout", pid=process.pid)
                        stop_group(process, args.grace_seconds)
                        break
                    if now >= next_heartbeat:
                        sample = process_sample(process.pid)
                        peak_rss = max(peak_rss, sample.get("live_tree_rss_kib", 0))
                        emit("progress", pid=process.pid, output_idle_seconds=round(now - last_output, 3),
                             active_go_tests=[name for _, name in sorted(go.active)[:5]], **sample)
                        next_heartbeat = now + args.heartbeat_seconds
                    delay = min(0.2, max(0, next_heartbeat - now), max(0, args.timeout_seconds - (now - started)))
                    for key, _ in selector.select(delay):
                        chunk = os.read(key.fd, 65536)
                        if not chunk:
                            selector.unregister(key.fileobj)
                            continue
                        last_output = time.monotonic()
                        log.write(chunk)
                        log.flush()
                        if not args.quiet:
                            sys.stdout.buffer.write(chunk)
                            sys.stdout.buffer.flush()
                        buffer += chunk
                        while b"\n" in buffer:
                            line, buffer = buffer.split(b"\n", 1)
                            go.consume(line)
                        if len(buffer) > 1024 * 1024:
                            buffer = b""  # Keep unstructured long lines bounded; the log retains bytes.
            returncode = process.wait()
        except InterruptedError as error:
            interrupted = True
            returncode = 128 + int(error.args[0])
        except OSError as error:
            error_kind = type(error).__name__
        finally:
            # Cleanup must survive a second interrupt, including while waiting
            # for the process group to terminate.
            for number in handlers:
                signal.signal(number, signal.SIG_IGN)
            if process is not None:
                if process.poll() is None or interrupted:
                    stop_group(process, args.grace_seconds)
                process.stdout.close()
            for number, handler in handlers.items():
                signal.signal(number, handler)
            if buffer:
                go.consume(buffer)
            usage_after = resource.getrusage(resource.RUSAGE_CHILDREN)
            exit_code = 124 if timed_out else (returncode if returncode >= 0 else 128 - returncode)
            result = {
                "label": args.label, "executable": Path(command[0]).name,
                "elapsed_seconds": round(time.monotonic() - started, 3),
                "exit_code": exit_code, "timed_out": timed_out, "interrupted": interrupted,
                "error_kind": error_kind, "output_log": log_path.name,
                "child_user_seconds": round(usage_after.ru_utime - usage_before.ru_utime, 3),
                "child_system_seconds": round(usage_after.ru_stime - usage_before.ru_stime, 3),
                "sampled_peak_tree_rss_kib": peak_rss,
                "packages": go.packages,
                "slowest_tests": sorted(go.tests, key=lambda x: x["seconds"], reverse=True)[:20],
                "unfinished_tests": [{"package": p, "test": t} for p, t in sorted(go.active)],
            }
            write_json(report, result)
            emit("end", exit_code=exit_code, report=str(report), timed_out=timed_out)
    return exit_code


def timestamp(value: str) -> dt.datetime:
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


def summarize_ci(run: dict, jobs: list[dict]) -> dict:
    now = dt.datetime.now(dt.timezone.utc)
    steps, summaries, starts, ends = [], [], [], []
    for job in jobs:
        if not job.get("started_at"):
            continue
        start = timestamp(job["started_at"])
        end = timestamp(job["completed_at"]) if job.get("completed_at") else now
        starts.append(start)
        ends.append(end)
        summaries.append({"name": job["name"], "seconds": (end - start).total_seconds(),
                          "status": job.get("status"), "conclusion": job.get("conclusion")})
        for step in job.get("steps", []):
            if step.get("started_at") and step.get("completed_at"):
                steps.append({"job": job["name"], "step": step["name"],
                              "seconds": (timestamp(step["completed_at"]) - timestamp(step["started_at"])).total_seconds()})
    return {"run_id": run["id"], "url": run.get("html_url"), "status": run["status"],
            "conclusion": run.get("conclusion"),
            "job_span_seconds": (max(ends) - min(starts)).total_seconds() if starts else None,
            "jobs": summaries, "slowest_steps": sorted(steps, key=lambda x: x["seconds"], reverse=True)[:20],
            "note": "Jobs overlap; their durations must not be added to obtain elapsed run time."}


def ci_report(args) -> int:
    prefix = f"repos/{args.repo}/actions/runs/{args.run_id}"

    def fetch(endpoint):
        # Do not depend on --slurp, which is absent in the installed gh 2.45.
        completed = subprocess.run(["gh", "api", endpoint], capture_output=True, check=True, timeout=45)
        return json.loads(completed.stdout)

    run = fetch(prefix)
    jobs = []
    page = 1
    while True:
        result_page = fetch(prefix + f"/jobs?per_page=100&page={page}")
        jobs.extend(result_page["jobs"])
        if not result_page["jobs"] or len(jobs) >= result_page["total_count"]:
            break
        page += 1
    result = summarize_ci(run, jobs)
    write_json(Path(args.report), result)
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0


def positive(raw):
    value = float(raw)
    if not (0 < value < float("inf")):
        raise argparse.ArgumentTypeError("must be a finite number greater than zero")
    return value


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    run = sub.add_parser("run", help="monitor one command; JSON excludes command arguments and environment")
    run.add_argument("--label", required=True)
    run.add_argument("--report", required=True)
    run.add_argument("--timeout-seconds", type=positive, default=300)
    run.add_argument("--heartbeat-seconds", type=positive, default=15)
    run.add_argument("--grace-seconds", type=positive, default=2)
    run.add_argument("--quiet", action="store_true")
    run.add_argument("command", nargs=argparse.REMAINDER)
    run.set_defaults(handler=observe)
    ci = sub.add_parser("ci", help="collect completed and currently running Actions job timings")
    ci.add_argument("run_id", type=int)
    ci.add_argument("--repo", required=True)
    ci.add_argument("--report", required=True)
    ci.set_defaults(handler=ci_report)
    args = parser.parse_args()
    try:
        return args.handler(args)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print(f"timing failed: {type(error).__name__}: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
