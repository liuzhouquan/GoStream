#!/usr/bin/env python3
"""Run RabbitMQ/MySQL failure tests and a real API/worker lifecycle smoke.

Creates a random schema/user and a disposable broker; never writes demo tables.
Requires an existing MySQL container exposing MYSQL_ROOT_PASSWORD internally.
"""
import argparse
import json
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
import urllib.request


ROOT = Path(__file__).resolve().parents[2]


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mysql-container", required=True)
    parser.add_argument("--mysql-addr", default="127.0.0.1:3307")
    parser.add_argument("--go-bin", default="go")
    args = parser.parse_args()
    required = re.search(r"^go (\S+)$", (ROOT / "go.mod").read_text(), re.M).group(1)
    actual = subprocess.check_output([args.go_bin, "env", "GOVERSION"], text=True).strip()
    if actual != "go" + required:
        raise RuntimeError(f"requires Go {required}; configured tool reports {actual}")
    tag = secrets.token_hex(5)
    schema, user = "gostream_rabbit_" + tag, "gsrabbit_" + tag
    password, broker_password = secrets.token_hex(24), secrets.token_hex(24)
    container = "gostream-rabbit-test-" + tag
    work = Path(tempfile.mkdtemp(prefix="gostream-rabbit-"))
    created_db = created_user = started = False
    api = None

    def run(command, **kwargs):
        try:
            return subprocess.run(command, cwd=ROOT, check=True, text=True, **kwargs)
        except subprocess.CalledProcessError as exc:
            detail = (exc.stderr or "").replace(password, "<REDACTED>").replace(broker_password, "<REDACTED>")
            raise RuntimeError(f"{command[0]} exited {exc.returncode}: {detail}") from None

    def mysql(sql):
        return run(["docker", "exec", "-i", args.mysql_container, "sh", "-c",
                    'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names'],
                   input=sql, capture_output=True).stdout.strip()

    def wait(label, predicate, seconds=45):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            try:
                if predicate():
                    return
            except OSError:
                pass
            time.sleep(0.2)
        raise RuntimeError("timed out waiting for " + label)

    def reachable(url):
        with urllib.request.urlopen(url, timeout=1) as response:
            return response.status == 200

    try:
        mysql(f"CREATE DATABASE `{schema}`;")
        created_db = True
        mysql(f"CREATE USER '{user}'@'%' IDENTIFIED BY '{password}';")
        created_user = True
        mysql(f"GRANT ALL PRIVILEGES ON `{schema}`.* TO '{user}'@'%';")
        amqp_port, http_port = free_port(), free_port()
        while http_port == amqp_port:
            http_port = free_port()
        # Explicit host ports remain stable across docker stop/start. Docker's
        # automatic host-port allocation can change its assignment on restart.
        run(["docker", "run", "-d", "--name", container, "--hostname", "rabbit-test",
             "-p", f"127.0.0.1:{amqp_port}:5672", "-p", f"127.0.0.1:{http_port}:15672",
             "-e", "RABBITMQ_DEFAULT_USER=gostream-test", "-e", "RABBITMQ_DEFAULT_PASS=" + broker_password,
             "-e", "RABBITMQ_SERVER_ADDITIONAL_ERL_ARGS=+S 2:2", "rabbitmq:4.3.7-management"], capture_output=True)
        started = True
        wait("broker management HTTP", lambda: reachable(f"http://127.0.0.1:{http_port}"))
        dsn = f"{user}:{password}@tcp({args.mysql_addr})/{schema}?charset=utf8mb4&parseTime=True&loc=Local"
        broker_url = f"amqp://gostream-test:{broker_password}@127.0.0.1:{amqp_port}/"
        env = dict(os.environ)
        env.update(GOSTREAM_TEST_MYSQL_DSN=dsn, GOSTREAM_TEST_RABBITMQ_MYSQL_DSN=dsn,
                   GOSTREAM_TEST_RABBITMQ_URL=broker_url, GOSTREAM_TEST_RABBITMQ_CONTAINER=container)
        run([args.go_bin, "test", "./internal/migrations", "-run", "^TestApplyMySQLIntegration$", "-count=1"], env=env)
        run([args.go_bin, "test", "-race", "./internal/platform", "-run", "^TestRabbitMQNotificationIntegration$", "-count=1", "-v", "-timeout=4m"], env=env)
        env.update(DB_DRIVER="mysql", DB_DSN=dsn, RABBITMQ_URL=broker_url,
                   RABBITMQ_QUEUE="gostream.process." + tag, JWT_SECRET=secrets.token_hex(32),
                   ADDR=f"127.0.0.1:{free_port()}", MEDIA_DIR=str(work / "media"), REDIS_ADDR="", FFMPEG_BIN="")
        for executable in ("gostream", "gostream-events"):
            run([args.go_bin, "build", "-o", str(work / executable), "./cmd/" + executable])
        with (work / "api.log").open("w") as log:
            api = subprocess.Popen([str(work / "gostream")], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT)
            base = "http://" + env["ADDR"]
            wait("real API readiness", lambda: reachable(base + "/ready"))

            def call(path, body, token="", method="POST"):
                headers = {"Content-Type": "application/json"}
                if token:
                    headers["Authorization"] = "Bearer " + token
                req = urllib.request.Request(base + path, data=None if body is None else json.dumps(body).encode(), headers=headers, method=method)
                with urllib.request.urlopen(req, timeout=5) as response:
                    return json.load(response)

            owner = call("/api/users", {"username": "process_owner", "nickname": "Owner", "password": "fixture-password-123"})
            actor = call("/api/users", {"username": "process_actor", "nickname": "Actor", "password": "fixture-password-123"})
            call(f"/api/users/me/following/{owner['user']['id']}", None, actor["access_token"], "PUT")
            wait("automatic source and delivery worker completion", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events o JOIN `{schema}`.notifications n "
                f"ON o.event_key=CONCAT('broker-delivery:notification:',n.id) WHERE n.user_id={owner['user']['id']} AND o.published_at IS NOT NULL;") == "1")
            api.send_signal(signal.SIGTERM)
            if api.wait(timeout=10) != 0:
                raise RuntimeError("API graceful shutdown failed")
            api = None
        # Exercise the shipped operator executable against this isolated schema.
        mysql(f"INSERT INTO `{schema}`.outbox_events(event_key,topic,payload,attempts,available_at,dead_at,created_at) "
              "VALUES('operator-smoke','notification.created','{}',8,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3));")
        run([str(work / "gostream-events"), "-outbox-key", "operator-smoke"], env=env)
        if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='operator-smoke' AND attempts=0 AND dead_at IS NULL;") != "1":
            raise AssertionError("operator did not reset exactly the dead SQL row")
        run([str(work / "gostream-events"), "-dead-letters", "1"], env=env)
        print(json.dumps({"rabbitmq": "4.3.7", "mysql": "8.4", "real_broker_matrix": True,
                          "api_worker_lifecycle": True, "operator_cli": True}), flush=True)
    finally:
        if api is not None:
            api.terminate()
            try:
                api.wait(timeout=10)
            except subprocess.TimeoutExpired:
                api.kill()
                api.wait()
        if started:
            run(["docker", "rm", "-f", "-v", container], capture_output=True)
        if created_db:
            mysql(f"DROP DATABASE `{schema}`;")
        if created_user:
            mysql(f"DROP USER '{user}'@'%';")
        print("remaining_schema=" + mysql(f"SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='{schema}';"), flush=True)
        print("remaining_user=" + mysql(f"SELECT COUNT(*) FROM mysql.user WHERE user='{user}';"), flush=True)
        shutil.rmtree(work)


if __name__ == "__main__":
    main()
