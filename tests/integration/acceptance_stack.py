#!/usr/bin/env python3
"""Reusable Compose launcher and lifecycle manager for the final acceptance stack.

Starts and verifies two GoStream API instances sharing MySQL 8.4, Redis 8.2,
RabbitMQ 4.3, and MinIO with FFmpeg enabled, as defined by:
  - docker-compose.yml
  - docker-compose.object.yml
  - tests/integration/object-test.compose.yml

Provides both a standalone CLI and a Python context manager (AcceptanceStack).
Handles automated readiness waiting, deployment and host metadata recording,
child test execution with injected base URLs, and strict resource cleanup
on normal exit, assertion failure, and SIGINT/SIGTERM.
"""

import argparse
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import subprocess
import sys
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

# launcher_lifecycle is in the same directory
ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))
from launcher_lifecycle import LauncherInterrupted, cleanup_on_interrupt, finish_process_start


DEFAULT_COMPOSE_FILES = [
    "docker-compose.yml",
    "docker-compose.object.yml",
    "tests/integration/object-test.compose.yml",
]


def running_group(group):
    """Find live descendant processes in a Linux process group."""
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
    """Gracefully terminate, then force kill, a child process group."""
    if process is None:
        return
    for action, duration in [(signal.SIGTERM, 10), (signal.SIGKILL, 3)]:
        if not running_group(process.pid):
            break
        try:
            os.killpg(process.pid, action)
        except ProcessLookupError:
            pass
        deadline = time.monotonic() + duration
        while running_group(process.pid) and time.monotonic() < deadline:
            time.sleep(0.05)
    try:
        process.wait(timeout=3)
    except subprocess.TimeoutExpired as exc:
        raise RuntimeError("command leader survived process-group cleanup") from exc
    remaining = running_group(process.pid)
    if remaining:
        raise RuntimeError(f"command descendants survived cleanup: {len(remaining)}")


def get_git_info(cwd):
    """Extract current Git commit hash and dirty status."""
    try:
        sha = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=cwd, text=True).strip()
        status = subprocess.check_output(["git", "status", "--porcelain"], cwd=cwd, text=True).strip()
        branch = subprocess.check_output(["git", "rev-parse", "--abbrev-ref", "HEAD"], cwd=cwd, text=True).strip()
        return {"commit": sha, "branch": branch, "dirty": bool(status)}
    except (subprocess.CalledProcessError, FileNotFoundError):
        return {"commit": "unknown", "branch": "unknown", "dirty": False}


def get_host_info():
    """Extract host CPU and memory metadata for diagnostic reports."""
    cpu_model = "unknown"
    try:
        for line in Path("/proc/cpuinfo").read_text().splitlines():
            if line.startswith("model name"):
                cpu_model = line.split(":", 1)[1].strip()
                break
    except LauncherInterrupted:
        raise
    except Exception:
        pass

    memory_kib = 0
    try:
        for line in Path("/proc/meminfo").read_text().splitlines():
            if line.startswith("MemTotal:"):
                memory_kib = int(line.split()[1])
                break
    except LauncherInterrupted:
        raise
    except Exception:
        pass

    sys_uname = os.uname()
    return {
        "kernel": sys_uname.release,
        "machine": sys_uname.machine,
        "cpu_model": cpu_model,
        "memory_bytes": memory_kib * 1024,
    }


def parse_ports(port_range_str):
    """Parse port string like '127.0.0.1:18080-18081' into individual URLs."""
    match = re.search(r"^(?:([^:]+):)?(\d+)-(\d+)$", port_range_str.strip())
    if match:
        host = match.group(1) or "127.0.0.1"
        start = int(match.group(2))
        end = int(match.group(3))
        return [f"http://{host}:{p}" for p in range(start, end + 1)]
    match_single = re.search(r"^(?:([^:]+):)?(\d+)$", port_range_str.strip())
    if match_single:
        host = match_single.group(1) or "127.0.0.1"
        port = int(match_single.group(2))
        return [f"http://{host}:{port}"]
    return ["http://127.0.0.1:18080", "http://127.0.0.1:18081"]


class AcceptanceStack:
    """Manages the full dual-API Compose acceptance stack lifecycle."""

    def __init__(
        self,
        project_name=None,
        compose_files=None,
        build=True,
        api_ports="127.0.0.1:18080-18081",
        mysql_port="127.0.0.1:13307",
        minio_api_port="127.0.0.1:29000",
        minio_console_port="127.0.0.1:29001",
        jwt_secret=None,
        wait_timeout=120,
        go_bin="go",
        root_dir=None,
    ):
        self.root = Path(root_dir or ROOT).resolve()
        tag = secrets.token_hex(4)
        self.project_name = project_name or f"gostream-acc-{tag}"
        self.compose_files = compose_files or DEFAULT_COMPOSE_FILES
        self.build = build
        self.api_ports_str = api_ports
        self.mysql_port_str = mysql_port
        self.minio_api_port_str = minio_api_port
        self.minio_console_port_str = minio_console_port
        self.jwt_secret = jwt_secret or secrets.token_urlsafe(32)
        self.wait_timeout = wait_timeout
        self.go_bin = go_bin

        api_urls = parse_ports(self.api_ports_str)
        self.api_a = api_urls[0] if len(api_urls) > 0 else "http://127.0.0.1:18080"
        self.api_b = api_urls[1] if len(api_urls) > 1 else "http://127.0.0.1:18081"
        self.mysql_addr = mysql_port
        self.minio_endpoint = minio_api_port

        self.env = os.environ.copy()
        self.env.update({
            "JWT_SECRET": self.jwt_secret,
            "GOSTREAM_API_PORTS": self.api_ports_str,
            "GOSTREAM_MYSQL_PORT": self.mysql_port_str,
            "GOSTREAM_MINIO_API_PORT": self.minio_api_port_str,
            "GOSTREAM_MINIO_CONSOLE_PORT": self.minio_console_port_str,
        })

        self.started = False
        self.metadata = {}
        self.child_process = None
        self._interrupt_context = None

    def compose_cmd(self, *args):
        """Construct the docker compose command invocation."""
        cmd = ["docker", "compose", "-p", self.project_name]
        for cf in self.compose_files:
            cmd.extend(["-f", str(self.root / cf)])
        cmd.extend(args)
        return cmd

    def run_compose(self, *args, check=True, capture_output=True):
        """Execute a docker compose command with project isolation."""
        cmd = self.compose_cmd(*args)
        return subprocess.run(
            cmd, cwd=self.root, env=self.env, check=check,
            capture_output=capture_output, text=True
        )

    def check_remaining_containers(self):
        """Query Docker for any surviving containers belonging to this Compose project."""
        cmd = [
            "docker", "ps", "-a",
            "--filter", f"label=com.docker.compose.project={self.project_name}",
            "--format", "{{.ID}}"
        ]
        proc = subprocess.run(cmd, capture_output=True, text=True, check=True)
        return proc.stdout.strip()

    def check_remaining_volumes(self):
        """Check project-owned volumes as well as the containers using them."""
        proc = subprocess.run([
            "docker", "volume", "ls", "--filter",
            f"label=com.docker.compose.project={self.project_name}",
            "--format", "{{.Name}}",
        ], capture_output=True, text=True, check=True)
        return proc.stdout.strip()

    def check_remaining_networks(self):
        """Find Compose networks owned by the named project."""
        proc = subprocess.run([
            "docker", "network", "ls", "--filter",
            f"label=com.docker.compose.project={self.project_name}",
            "--format", "{{.ID}}",
        ], capture_output=True, text=True, check=True)
        return proc.stdout.strip()

    def verify_go_runtime(self):
        """Verify local Go matches go.mod specification."""
        go_mod = (self.root / "go.mod").read_text()
        required = re.search(r"^go (\S+)$", go_mod, re.M).group(1)
        actual = subprocess.check_output([self.go_bin, "env", "GOVERSION"], text=True).strip()
        if actual != "go" + required:
            raise RuntimeError(f"requires Go {required}; configured tool reports {actual}")
        return actual

    def up(self):
        """Build and launch the dual-API acceptance stack in isolated containers."""
        go_version = self.verify_go_runtime()
        git_info = get_git_info(self.root)
        host_info = get_host_info()

        # A caller-selected name is not proof that its data is disposable.
        # Refuse existing projects before claiming any cleanup ownership.
        for kind, probe in (
            ("containers", self.check_remaining_containers),
            ("volumes", self.check_remaining_volumes),
            ("networks", self.check_remaining_networks),
        ):
            if probe():
                raise RuntimeError(f"refusing existing Compose project {self.project_name}: {kind} already present")

        up_args = ["up", "-d", "--scale", "api=2"]
        if self.build:
            up_args.append("--build")

        start_time = time.monotonic()
        # Mark started before invocation so interruption during up() guarantees down() cleanup
        self.started = True
        self.run_compose(*up_args, capture_output=False)

        self.wait_for_ready(self.wait_timeout)
        ready_elapsed = round(time.monotonic() - start_time, 3)

        instances = self.inspect_instances()

        self.metadata = {
            "version": "1.0-demo",
            "git": git_info,
            "go_version": go_version,
            "host": host_info,
            "deployment": {
                "project_name": self.project_name,
                "compose_files": self.compose_files,
                "scale": 2,
                "api_a": self.api_a,
                "api_b": self.api_b,
                "mysql_addr": self.mysql_addr,
                "minio_endpoint": self.minio_endpoint,
                "build": self.build,
            },
            "api_instances": instances,
            "ready_elapsed_seconds": ready_elapsed,
        }
        return self

    def probe_url(self, url, timeout=2):
        """HTTP GET probe returning (status_code, parsed_json_or_text)."""
        req = Request(url, headers={"User-Agent": "GoStream-Acceptance-Launcher"})
        try:
            with urlopen(req, timeout=timeout) as resp:
                raw = resp.read().decode("utf-8", errors="replace")
                try:
                    return resp.status, json.loads(raw)
                except Exception:
                    return resp.status, raw
        except HTTPError as err:
            return err.code, str(err)
        except (URLError, TimeoutError, OSError) as err:
            return 0, str(err)

    def wait_for_ready(self, timeout):
        """Poll until both API instances report /health and /ready."""
        deadline = time.monotonic() + timeout
        last_error = None
        while time.monotonic() < deadline:
            status_a_h, _ = self.probe_url(f"{self.api_a}/health")
            status_a_r, data_a = self.probe_url(f"{self.api_a}/ready")
            status_b_h, _ = self.probe_url(f"{self.api_b}/health")
            status_b_r, data_b = self.probe_url(f"{self.api_b}/ready")

            ready_a = (status_a_h == 200 and status_a_r == 200 and
                       isinstance(data_a, dict) and data_a.get("status") == "ready")
            ready_b = (status_b_h == 200 and status_b_r == 200 and
                       isinstance(data_b, dict) and data_b.get("status") == "ready")

            if ready_a and ready_b:
                return

            last_error = (
                f"API A: health={status_a_h}, ready={status_a_r} ({data_a}); "
                f"API B: health={status_b_h}, ready={status_b_r} ({data_b})"
            )
            time.sleep(1)

        logs = self.run_compose("logs", "--tail=50", check=False).stdout
        raise TimeoutError(
            f"Dual API readiness timed out after {timeout}s: {last_error}\n"
            f"Container logs:\n{logs}"
        )

    def inspect_instances(self):
        """Extract container details and verify metrics from both API instances."""
        instances = []
        for label, url in [("api_a", self.api_a), ("api_b", self.api_b)]:
            status, metrics_raw = self.probe_url(f"{url}/metrics")
            has_metrics = (status == 200 and "gostream" in str(metrics_raw))
            instances.append({
                "label": label,
                "url": url,
                "metrics_available": has_metrics,
            })

        # Inspect compose containers if docker CLI is functional
        try:
            ps_proc = self.run_compose("ps", "--format", "json", check=False)
            if ps_proc.returncode == 0 and ps_proc.stdout.strip():
                try:
                    for line in ps_proc.stdout.strip().splitlines():
                        parsed = json.loads(line)
                        service = parsed.get("Service") or parsed.get("service")
                        if service == "api":
                            name = parsed.get("Name") or parsed.get("name")
                            cid = parsed.get("ID") or parsed.get("id")
                            instances.append({"service": "api", "container_name": name, "container_id": cid})
                except LauncherInterrupted:
                    raise
                except Exception:
                    pass
        except LauncherInterrupted:
            raise
        except Exception:
            pass

        return instances

    def down(self):
        """Stop and remove all containers, networks, and disposable volumes."""
        errors = []
        if self.child_process is not None:
            try:
                stop_test_group(self.child_process)
                self.child_process = None
            except Exception as exc:
                errors.append(f"stop_child_process: {exc}")

        if self.started:
            try:
                # Down with volume removal
                self.run_compose("down", "-v", "--remove-orphans", check=True)
            except Exception as exc:
                errors.append(f"compose_down: {exc}")

            # Verify no remaining containers for this project
            try:
                remaining = self.check_remaining_containers()
                if remaining:
                    errors.append(f"surviving containers: {remaining}")
            except Exception as exc:
                errors.append(f"verify_cleanup: {exc}")

            try:
                remaining = self.check_remaining_volumes()
                if remaining:
                    errors.append(f"surviving volumes: {remaining}")
            except Exception as exc:
                errors.append(f"verify_volume_cleanup: {exc}")

            try:
                remaining = self.check_remaining_networks()
                if remaining:
                    errors.append(f"surviving networks: {remaining}")
            except Exception as exc:
                errors.append(f"verify_network_cleanup: {exc}")

            if not errors:
                self.started = False

        if errors:
            raise RuntimeError("; ".join(errors))

    def run_subcommand(self, cmd_args, env_extra=None):
        """Execute a test command with target base URLs in environment."""
        env = self.env.copy()
        env.update({
            "GOSTREAM_API_A": self.api_a,
            "GOSTREAM_API_B": self.api_b,
            "GOSTREAM_BASE_URL": self.api_a,
            "GOSTREAM_COMPOSE_ACTIVE": "1",
            "GOSTREAM_STACK_PROJECT": self.project_name,
            "GOSTREAM_MYSQL_ADDR": self.mysql_addr,
            "GOSTREAM_MINIO_ENDPOINT": self.minio_endpoint,
        })
        if env_extra:
            env.update(env_extra)

        with finish_process_start():
            self.child_process = subprocess.Popen(
                cmd_args, cwd=self.root, env=env, start_new_session=True
            )

        exit_code = self.child_process.wait()
        return exit_code

    def __enter__(self):
        self._interrupt_context = cleanup_on_interrupt()
        self._interrupt_context.__enter__()
        try:
            self.up()
            return self
        except Exception:
            for number in (signal.SIGINT, signal.SIGTERM):
                signal.signal(number, signal.SIG_IGN)
            try:
                self.down()
            except Exception:
                pass
            self._interrupt_context.__exit__(*sys.exc_info())
            raise

    def __exit__(self, exc_type, exc_val, exc_tb):
        # Ignore secondary interrupts while cleaning up
        for number in (signal.SIGINT, signal.SIGTERM):
            signal.signal(number, signal.SIG_IGN)
        try:
            self.down()
        finally:
            if self._interrupt_context:
                self._interrupt_context.__exit__(exc_type, exc_val, exc_tb)


@cleanup_on_interrupt()
def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project-name", help="custom Compose project name")
    parser.add_argument("--no-build", action="store_true", help="skip docker compose --build")
    parser.add_argument("--api-ports", default="127.0.0.1:18080-18081",
                        help="port range for the two APIs (default: 127.0.0.1:18080-18081)")
    parser.add_argument("--mysql-port", default="127.0.0.1:13307", help="MySQL port binding")
    parser.add_argument("--minio-api-port", default="127.0.0.1:29000", help="MinIO API port binding")
    parser.add_argument("--minio-console-port", default="127.0.0.1:29001", help="MinIO Console port binding")
    parser.add_argument("--wait-timeout", type=int, default=120, help="readiness timeout in seconds")
    parser.add_argument("--go-bin", default="go", help="path to Go executable")
    parser.add_argument("--report-file", type=Path, help="path to write successful metadata JSON")
    parser.add_argument("--probe-only", action="store_true",
                        help="launch, probe /health and /ready, output metadata, then clean and exit")
    parser.add_argument("command", nargs=argparse.REMAINDER,
                        help="subcommand to execute against the active stack (prefix with --)")

    args = parser.parse_args()
    report_path = args.report_file.resolve() if args.report_file else None
    if report_path:
        report_path.parent.mkdir(parents=True, exist_ok=True)
        # Invalidate any prior report so failed runs never masquerade as success
        report_path.unlink(missing_ok=True)

    stack = AcceptanceStack(
        project_name=args.project_name,
        build=not args.no_build,
        api_ports=args.api_ports,
        mysql_port=args.mysql_port,
        minio_api_port=args.minio_api_port,
        minio_console_port=args.minio_console_port,
        wait_timeout=args.wait_timeout,
        go_bin=args.go_bin,
    )

    exit_code = 0
    try:
        stack.up()
        print(f"GoStream dual-API acceptance stack ready: A={stack.api_a}, B={stack.api_b}", flush=True)

        # Strip leading '--' if passed
        subcmd = args.command
        if subcmd and subcmd[0] == "--":
            subcmd = subcmd[1:]

        if subcmd:
            print(f"Executing acceptance test: {' '.join(subcmd)}", flush=True)
            exit_code = stack.run_subcommand(subcmd)
        elif not args.probe_only:
            print("No command specified and not --probe-only. Stack verified successfully.", flush=True)

    finally:
        for number in (signal.SIGINT, signal.SIGTERM):
            signal.signal(number, signal.SIG_IGN)
        try:
            stack.down()
        except Exception as exc:
            print(f"Teardown error: {exc}", file=sys.stderr)
            if exit_code == 0:
                exit_code = 1
        if report_path and exit_code != 0:
            report_path.unlink(missing_ok=True)

    # Only a normally completed subcommand followed by successful teardown
    # may publish an acceptance report. Exceptions/cancellation skip this.
    if exit_code == 0:
        stack.metadata["launcher_cleanup"] = {"containers": 0, "volumes": 0, "networks": 0}
        report_json = json.dumps(stack.metadata, indent=2) + "\n"
        if report_path:
            report_path.write_text(report_json)
            report_path.chmod(0o600)
            print(f"Acceptance report written to: {report_path}", flush=True)
        else:
            print(f"ACCEPTANCE_METADATA_JSON={report_json}", flush=True)

    sys.exit(exit_code)


if __name__ == "__main__":
    try:
        main()
    except LauncherInterrupted as error:
        print("Acceptance stack launcher interrupted; cleanup completed", file=sys.stderr)
        raise SystemExit(128 + int(error.args[0]))
