#!/usr/bin/env python3
"""Exercise two real race-instrumented APIs with disposable MySQL/Redis data."""
import argparse
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import subprocess
import tempfile
import time


ROOT = Path(__file__).resolve().parents[2]


def running_group(group):
    """Linux process groups let the launcher own descendants after test panic."""
    result = []
    for path in Path("/proc").glob("[0-9]*/stat"):
        try:
            fields = path.read_text().rsplit(") ", 1)[1].split()
            if int(fields[2]) == group and fields[0] not in ("Z", "X"):
                result.append(int(path.parent.name))
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            pass
    return result


def stop_test_group(process):
    if process is None:
        return
    # The Go command starts a new session; this cannot signal the launcher's
    # terminal, demonstration API or any sibling test invocation.
    for action, duration in [(signal.SIGTERM, 12), (signal.SIGKILL, 3)]:
        if not running_group(process.pid):
            break
        try:
            os.killpg(process.pid, action)
        except ProcessLookupError:
            pass
        deadline = time.monotonic() + duration
        while running_group(process.pid) and time.monotonic() < deadline:
            time.sleep(0.05)
    process.wait(timeout=3)
    remaining = running_group(process.pid)
    print("remaining_test_processes=" + str(len(remaining)), flush=True)
    if remaining:
        raise RuntimeError("could not stop isolated test process group")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mysql-container", required=True)
    parser.add_argument("--mysql-addr", default="127.0.0.1:3307")
    parser.add_argument("--go-bin", default="go")
    parser.add_argument("--check-timeout-cleanup", action="store_true",
                        help="deliberately time out after API startup and verify outer process cleanup")
    args = parser.parse_args()
    required = re.search(r"^go (\S+)$", (ROOT / "go.mod").read_text(), re.M).group(1)
    actual = subprocess.check_output([args.go_bin, "env", "GOVERSION"], text=True).strip()
    if actual != "go" + required:
        raise RuntimeError(f"requires Go {required}; configured tool reports {actual}")
    tag, password = secrets.token_hex(5), secrets.token_hex(24)
    schema, user, container = "gostream_im_" + tag, "gsim_" + tag, "gostream-im-test-" + tag
    work = Path(tempfile.mkdtemp(prefix="gostream-im-"))
    db_created = user_created = container_created = False
    test_process = None

    def run(command, **kwargs):
        try:
            return subprocess.run(command, cwd=ROOT, check=True, text=True, **kwargs)
        except subprocess.CalledProcessError as exc:
            detail = (exc.stderr or "").replace(password, "<REDACTED>")
            raise RuntimeError(f"{command[0]} exited {exc.returncode}: {detail}") from None

    def mysql(sql):
        return run(["docker", "exec", "-i", args.mysql_container, "sh", "-c",
                    'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names'],
                   input=sql, capture_output=True).stdout.strip()

    try:
        mysql(f"CREATE DATABASE `{schema}`;")
        db_created = True
        mysql(f"CREATE USER '{user}'@'%' IDENTIFIED BY '{password}';")
        user_created = True
        mysql(f"GRANT ALL PRIVILEGES ON `{schema}`.* TO '{user}'@'%';")
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        # An explicit host port remains stable across the Redis stop/start test.
        run(["docker", "run", "-d", "--name", container, "-p", f"127.0.0.1:{port}:6379",
             "redis:8.2.9", "redis-server", "--save", "", "--appendonly", "no"], capture_output=True)
        container_created = True
        for _ in range(100):
            probe = subprocess.run(["docker", "exec", container, "redis-cli", "ping"], capture_output=True, text=True)
            if probe.returncode == 0 and probe.stdout.strip() == "PONG":
                break
            time.sleep(0.1)
        else:
            raise RuntimeError("temporary Redis did not become ready")
        print("go=" + actual + " mysql=" + mysql("SELECT VERSION();") + " redis_image=redis:8.2.9", flush=True)
        binary = work / "gostream"
        run([args.go_bin, "build", "-race", "-o", str(binary), "./cmd/gostream"])
        env = dict(os.environ)
        test_tmp = work / "test-tmp"
        test_tmp.mkdir()
        env["TMPDIR"] = str(test_tmp)
        env["GOSTREAM_TEST_IM_WAIT_FOR_TIMEOUT"] = ""
        env.update(GOSTREAM_TEST_IM_BINARY=str(binary),
                   GOSTREAM_TEST_IM_MYSQL_DSN=f"{user}:{password}@tcp({args.mysql_addr})/{schema}?charset=utf8mb4&parseTime=True&loc=Local",
                   GOSTREAM_TEST_IM_REDIS_ADDR=f"127.0.0.1:{port}",
                   GOSTREAM_TEST_IM_REDIS_CONTAINER=container)
        test_args = [args.go_bin, "test", "-race", "./tests/integration/im", "-run", "^TestIMProcessRecovery$", "-count=1", "-v"]
        if args.check_timeout_cleanup:
            env["GOSTREAM_TEST_IM_WAIT_FOR_TIMEOUT"] = "1"
            test_args.append("-timeout=10s")
        else:
            test_args.append("-timeout=6m")
        test_process = subprocess.Popen(test_args, cwd=ROOT, env=env, start_new_session=True,
                                        stdout=subprocess.PIPE if args.check_timeout_cleanup else None,
                                        stderr=subprocess.STDOUT if args.check_timeout_cleanup else None,
                                        text=True)
        if args.check_timeout_cleanup:
            output, _ = test_process.communicate()
            if (test_process.returncode == 0 or "intentional API cleanup timeout" not in output
                    or "panic: test timed out after 10s" not in output):
                raise RuntimeError("expected timeout path was not reached after API startup")
            print("expected_test_timeout=true", flush=True)
            print("running_orphans_before_cleanup=" + str(len(running_group(test_process.pid))), flush=True)
        elif test_process.wait() != 0:
            raise RuntimeError("IM process matrix failed")
    finally:
        # Each cleanup remains independent so one failed deletion cannot skip
        # cleanup of the temporary credentials and the other resources.
        errors = []
        # Stop descendants before removing their SQL credentials or Redis.
        try:
            stop_test_group(test_process)
        except (OSError, RuntimeError, subprocess.TimeoutExpired) as exc:
            errors.append(str(exc))
        for needed, cleanup in [
            (container_created, lambda: run(["docker", "rm", "-f", "-v", container], capture_output=True)),
            (db_created, lambda: mysql(f"DROP DATABASE `{schema}`;")),
            (user_created, lambda: mysql(f"DROP USER '{user}'@'%';")),
        ]:
            if needed:
                try:
                    cleanup()
                except RuntimeError as exc:
                    errors.append(str(exc))
        shutil.rmtree(work)
        print("remaining_schema=" + mysql(f"SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='{schema}';"), flush=True)
        print("remaining_user=" + mysql(f"SELECT COUNT(*) FROM mysql.user WHERE user='{user}';"), flush=True)
        if test_process is not None and running_group(test_process.pid):
            errors.append("isolated API processes survived launcher cleanup")
        if errors:
            raise RuntimeError("; ".join(errors))


if __name__ == "__main__":
    main()
