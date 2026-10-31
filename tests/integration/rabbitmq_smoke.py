#!/usr/bin/env python3
"""Run RabbitMQ/MySQL failure tests and a real API/worker lifecycle smoke.

Creates a random schema/user and a disposable broker; never writes demo tables.
Requires an existing MySQL container exposing MYSQL_ROOT_PASSWORD internally.
"""
import argparse
import concurrent.futures
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

    paused = False
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
             "-e", "RABBITMQ_SERVER_ADDITIONAL_ERL_ARGS=+S 2:2 -rabbit dead_letter_worker_publisher_confirm_timeout 1000", "rabbitmq:4.3.7-management"], capture_output=True)
        started = True
        wait("broker management HTTP", lambda: reachable(f"http://127.0.0.1:{http_port}"))
        retry_setting = run(["docker", "exec", container, "rabbitmqctl", "eval",
                             "application:get_env(rabbit, dead_letter_worker_publisher_confirm_timeout)."],
                            capture_output=True).stdout.strip()
        if retry_setting != "{ok,1000}":
            raise RuntimeError("isolated broker DLQ retry setting did not apply")
        print("isolated_dlq_retry_ms=1000", flush=True)
        dsn = f"{user}:{password}@tcp({args.mysql_addr})/{schema}?charset=utf8mb4&parseTime=True&loc=Local"
        broker_url = f"amqp://gostream-test:{broker_password}@127.0.0.1:{amqp_port}/"
        env = dict(os.environ)
        env.update(GOSTREAM_TEST_MYSQL_DSN=dsn, GOSTREAM_TEST_RABBITMQ_MYSQL_DSN=dsn,
                   GOSTREAM_TEST_RABBITMQ_URL=broker_url, GOSTREAM_TEST_RABBITMQ_CONTAINER=container,
                   GOSTREAM_TEST_RABBITMQ_DLQ_WAIT="15s")
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

            # Scenario 2: Claimed source SIGKILL while broker traffic is paused and worker recovery
            # Pause broker container to block AMQP confirm traffic, forcing worker into publisher confirm wait
            run(["docker", "pause", container], capture_output=True)
            paused = True
            actor2 = call("/api/users", {"username": "process_actor2", "nickname": "Actor2", "password": "fixture-password-123"})
            call(f"/api/users/me/following/{owner['user']['id']}", None, actor2["access_token"], "PUT")
            wait("worker claimed row during broker pause", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE topic='notification.created' AND published_at IS NULL AND attempts >= 1;") == "1")

            # Forcibly kill API process with SIGKILL after SQL claim during a broker pause (not an observed confirm boundary)
            api.send_signal(signal.SIGKILL)
            if api.wait(timeout=10) != -signal.SIGKILL:
                raise RuntimeError("API did not exit via SIGKILL")
            api = None

            # Verify in SQL: published_at is STILL NULL; row was not prematurely marked sent
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE topic='notification.created' AND published_at IS NULL AND attempts >= 1;") != "1":
                raise AssertionError("unconfirmed event prematurely marked published before SIGKILL")

            # Unpause broker and reset lease available_at for immediate restart pickup
            run(["docker", "unpause", container], capture_output=True)
            paused = False
            mysql(f"UPDATE `{schema}`.outbox_events SET available_at=UTC_TIMESTAMP(3) WHERE published_at IS NULL;")

            api = subprocess.Popen([str(work / "gostream")], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT)
            wait("restarted API readiness", lambda: reachable(base + "/ready"))
            wait("recovery of claimed source after SIGKILL", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events o JOIN `{schema}`.notifications n "
                f"ON o.event_key=CONCAT('broker-delivery:notification:',n.id) WHERE n.user_id={owner['user']['id']} AND o.published_at IS NOT NULL;") == "2")

            # Scenario 3: Finite backlog drain of 50 events while worker is offline
            actor3 = call("/api/users", {"username": "process_actor3", "nickname": "Actor3", "password": "fixture-password-123"})
            api.send_signal(signal.SIGTERM)
            if api.wait(timeout=10) != 0:
                raise RuntimeError("API shutdown before backlog drain failed")
            api = None

            const_backlog = 50
            for i in range(const_backlog):
                mysql(f"INSERT INTO `{schema}`.notifications(user_id,actor_id,kind,text,event_key,created_at) "
                      f"VALUES({owner['user']['id']},{actor3['user']['id']},'follow','sustained-backlog-{i}','sustained-key-{tag}-{i}',UTC_TIMESTAMP(3));")
                nid = mysql(f"SELECT id FROM `{schema}`.notifications WHERE event_key='sustained-key-{tag}-{i}';")
                mysql(f"INSERT INTO `{schema}`.outbox_events(event_key,topic,payload,attempts,available_at,created_at) "
                      f"VALUES('notification:{nid}','notification.created','{{\"notification_id\":{nid}}}',0,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3));")

            api = subprocess.Popen([str(work / "gostream")], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT)
            wait("API readiness for backlog drain", lambda: reachable(base + "/ready"))
            expected_deliveries = 2 + const_backlog
            wait(f"drain finite backlog of {const_backlog} events", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE topic='notification.deliver' AND published_at IS NOT NULL;") == str(expected_deliveries), seconds=60)

            # Assert all source events marked published and zero dead letters
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE topic='notification.created' AND published_at IS NOT NULL;") != str(expected_deliveries):
                raise AssertionError("not all source backlog events were published")
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE dead_at IS NOT NULL;") != "0":
                raise AssertionError("unexpected dead outbox events during backlog drain")

            # Verify queue depth returns to 0 via RabbitMQ Management HTTP API
            auth_handler = urllib.request.HTTPBasicAuthHandler()
            auth_handler.add_password(realm="RabbitMQ Management", uri=f"http://127.0.0.1:{http_port}", user="gostream-test", passwd=broker_password)
            opener = urllib.request.build_opener(auth_handler)
            def queue_depth(qname=f"gostream.process.{tag}"):
                try:
                    with opener.open(f"http://127.0.0.1:{http_port}/api/queues/%2f/{qname}", timeout=2) as r:
                        data = json.load(r)
                        return data.get("messages", -1)
                except Exception:
                    return -1
            wait("broker queue depth returns to 0", lambda: queue_depth() == 0, seconds=15)

            def publish_envelope(event_id, payload):
                request = urllib.request.Request(
                    f"http://127.0.0.1:{http_port}/api/exchanges/%2f/gostream.process.{tag}.exchange/publish",
                    data=json.dumps({"properties": {"delivery_mode": 2, "message_id": event_id},
                                     "routing_key": "notification.created", "payload": payload,
                                     "payload_encoding": "string"}).encode(),
                    headers={"Content-Type": "application/json"}, method="POST")
                with opener.open(request, timeout=5) as response:
                    if not json.load(response).get("routed", False):
                        raise AssertionError("broker did not route fixture " + event_id)

            def seed_source(label, with_delivery=False):
                # Both source and optional dedup row become visible atomically.
                # Source is already marked sent: the running SQL publisher must
                # not race this manual management-API producer.
                delivery = ("INSERT INTO `{schema}`.outbox_events(event_key,topic,payload,attempts,available_at,created_at) "
                            "VALUES(CONCAT('broker-delivery:notification:',@nid),'notification.deliver',"
                            "CONCAT('{{\"notification_id\":',@nid,'}}'),0,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3));").format(schema=schema) if with_delivery else ""
                nid = mysql(f"START TRANSACTION; INSERT INTO `{schema}`.notifications(user_id,actor_id,kind,text,event_key,created_at) "
                            f"VALUES({owner['user']['id']},{actor3['user']['id']},'follow','{label}','{label}-{tag}',UTC_TIMESTAMP(3)); "
                            "SET @nid=LAST_INSERT_ID(); "
                            f"INSERT INTO `{schema}`.outbox_events(event_key,topic,payload,attempts,available_at,created_at,published_at) "
                            "VALUES(CONCAT('notification:',@nid),'notification.created',"
                            "CONCAT('{\"notification_id\":',@nid,'}'),1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3)); "
                            + delivery + "SELECT @nid; COMMIT;")
                event_id = "notification:" + nid
                payload = json.dumps({"version": 1, "event_id": event_id, "topic": "notification.created", "notification_id": int(nid)})
                return event_id, payload

            def duplicate_metric():
                with urllib.request.urlopen(base + "/metrics", timeout=2) as response:
                    body = response.read().decode()
                match = re.search(r'^gostream_rabbitmq_messages_total\{result="duplicate"\} ([0-9.eE+\-]+)$', body, re.M)
                return float(match.group(1)) if match else 0

            # Scenario 4 is deduplication against a precommitted fixture, not
            # a process kill or a delivery held between commit and basic.ack.
            preack_id, payload = seed_source("precommitted", with_delivery=True)
            before_dup = duplicate_metric()
            publish_envelope(preack_id, payload)
            wait("actual consumer deduplication metric", lambda: duplicate_metric() >= before_dup + 1)
            wait("deduplicated delivery dispatch", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='broker-delivery:{preack_id}' AND published_at IS NOT NULL;") == "1")
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='broker-delivery:{preack_id}';") != "1":
                raise AssertionError("duplicate delivery event created in SQL")
            wait("queue drains after deduplication", lambda: queue_depth() == 0, seconds=15)

            # Scenario 5: independent valid identities and poison envelopes are
            # submitted concurrently. No repeated idempotent follow is counted
            # as a new event. Final dispatch is checked separately from accept.
            valid = [seed_source("mixed-" + str(i)) for i in range(10)]
            poison = [(f"poison-{tag}-{i}", "{corrupt-json-poison-envelope") for i in range(3)]
            publications = []
            for i, message in enumerate(valid):
                publications.append(message)
                if i < len(poison):
                    publications.append(poison[i])
            with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
                futures = [pool.submit(publish_envelope, *message) for message in publications]
                for future in concurrent.futures.as_completed(futures):
                    future.result()
            for event_id, _ in valid:
                wait("exact mixed delivery " + event_id, lambda event_id=event_id: mysql(
                    f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='broker-delivery:{event_id}' AND published_at IS NOT NULL;") == "1")
                if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='broker-delivery:{event_id}';") != "1":
                    raise AssertionError("mixed delivery not unique")
            dead_qname = f"gostream.process.{tag}.dead"
            wait("exact poison queue depth", lambda: queue_depth(dead_qname) == len(poison), seconds=20)
            get_request = urllib.request.Request(
                f"http://127.0.0.1:{http_port}/api/queues/%2f/{dead_qname}/get",
                data=json.dumps({"count": len(poison), "ackmode": "ack_requeue_true", "encoding": "auto"}).encode(),
                headers={"Content-Type": "application/json"}, method="POST")
            # Management fetches the whole batch before requeueing it, retaining
            # the same messages for the operator CLI replay below.
            with opener.open(get_request, timeout=5) as response:
                dead = json.load(response)
            expected_poison = {event_id for event_id, _ in poison}
            if len(dead) != len(poison) or {m["properties"].get("message_id") for m in dead} != expected_poison:
                raise AssertionError("poison identities missing or duplicated")
            for message in dead:
                props = message["properties"]
                if props.get("delivery_mode") != 2 or props.get("headers", {}).get("x-first-death-reason") != "rejected" or message.get("payload") != poison[0][1]:
                    raise AssertionError("unexpected poison durability/reason/payload")
            wait("main queue drains mixed traffic", lambda: queue_depth() == 0, seconds=20)

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
        replay = run([str(work / "gostream-events"), "-dead-letters", "3"], env=env, capture_output=True)
        if "replayed=3" not in replay.stdout.splitlines():
            raise AssertionError("operator did not replay exactly the three inspected poisons")
        wait("operator source copies", lambda: queue_depth() == 3 and queue_depth(dead_qname) == 0, seconds=15)
        print(json.dumps({"rabbitmq": "4.3.7", "mysql": "8.4", "real_broker_matrix": True,
                          "api_worker_lifecycle": True, "operator_cli": True}), flush=True)
    finally:
        cleanup_errors = []
        if paused:
            try:
                run(["docker", "unpause", container], capture_output=True)
            except RuntimeError as exc:
                cleanup_errors.append(str(exc))
        if api is not None:
            try:
                if api.poll() is None:
                    api.terminate()
                    try:
                        api.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        api.kill()
                        api.wait(timeout=5)
            except (OSError, subprocess.TimeoutExpired) as exc:
                cleanup_errors.append(str(exc))
        for needed, cleanup in [
            (started, lambda: run(["docker", "rm", "-f", "-v", container], capture_output=True)),
            (created_db, lambda: mysql(f"DROP DATABASE `{schema}`;")),
            (created_user, lambda: mysql(f"DROP USER '{user}'@'%';")),
        ]:
            if needed:
                try:
                    cleanup()
                except RuntimeError as exc:
                    cleanup_errors.append(str(exc))
        for name, query in [
            ("schema", f"SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='{schema}';"),
            ("user", f"SELECT COUNT(*) FROM mysql.user WHERE user='{user}';"),
        ]:
            try:
                remaining = mysql(query)
                print("remaining_" + name + "=" + remaining, flush=True)
                if remaining != "0":
                    cleanup_errors.append("isolated " + name + " survived cleanup")
            except RuntimeError as exc:
                cleanup_errors.append(str(exc))
        alive = api is not None and api.poll() is None
        print("remaining_api=" + str(bool(alive)).lower(), flush=True)
        if alive:
            cleanup_errors.append("isolated API survived cleanup")
        else:
            shutil.rmtree(work)
        if cleanup_errors:
            raise RuntimeError("; ".join(cleanup_errors))



if __name__ == "__main__":
    main()
