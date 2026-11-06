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
import select
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

from rabbitmq_ack_proxy import AckGateProxy
from launcher_lifecycle import LauncherInterrupted, cleanup_on_interrupt, finish_process_start


ROOT = Path(__file__).resolve().parents[2]


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def start_outbox_row_lock(mysql_container, schema, row_id):
    client = None
    try:
        with finish_process_start():
            client = subprocess.Popen(
                ["docker", "exec", "-i", mysql_container, "sh", "-c",
                 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names --unbuffered'],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                text=True, bufsize=1,
            )
        client.stdin.write(
            f"START TRANSACTION; SELECT id FROM `{schema}`.outbox_events "
            f"WHERE id={int(row_id)} FOR UPDATE;\n"
        )
        client.stdin.flush()
        ready, _, _ = select.select([client.stdout], [], [], 10)
        if not ready or client.stdout.readline().strip() != str(row_id):
            raise RuntimeError("MySQL did not acquire the requested outbox row lock")
        return client
    except Exception:
        release_outbox_row_lock(client)
        raise


def release_outbox_row_lock(client):
    if client is None:
        return
    if client.poll() is None:
        try:
            client.stdin.write("ROLLBACK;\n")
            client.stdin.flush()
            client.stdin.close()
            client.wait(timeout=10)
        except (BrokenPipeError, OSError, subprocess.TimeoutExpired):
            client.kill()
            client.wait(timeout=5)
    if client.stdout is not None:
        client.stdout.close()


@cleanup_on_interrupt()
def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mysql-container", required=True)
    parser.add_argument("--mysql-addr", default="127.0.0.1:3307")
    parser.add_argument("--go-bin", default="go")
    parser.add_argument("--rabbitmq-cluster", action="store_true",
                         help="run the same acceptance against a disposable 3-node RabbitMQ cluster")
    parser.add_argument("--cluster-only", action="store_true",
                         help="run only the API follower-loss scenario; requires --rabbitmq-cluster")
    parser.add_argument("--cluster-leader-only", action="store_true",
                         help="run only the API leader-loss scenario; requires --rabbitmq-cluster")
    parser.add_argument("--cluster-majority-only", action="store_true",
                         help="run only the API delivery recovery scenario after quorum majority loss")
    args = parser.parse_args()
    cluster_modes = [args.cluster_only, args.cluster_leader_only, args.cluster_majority_only]
    if sum(cluster_modes) > 1:
        parser.error("cluster-only modes are mutually exclusive")
    if any(cluster_modes) and not args.rabbitmq_cluster:
        parser.error("cluster-only modes require --rabbitmq-cluster")
    required = re.search(r"^go (\S+)$", (ROOT / "go.mod").read_text(), re.M).group(1)
    actual = subprocess.check_output([args.go_bin, "env", "GOVERSION"], text=True).strip()
    if actual != "go" + required:
        raise RuntimeError(f"requires Go {required}; configured tool reports {actual}")
    tag = secrets.token_hex(5)
    api_queue = "gostream.process." + tag
    dead_qname = api_queue + ".dead"
    capacity_policy = "gostream-dlq-capacity-" + tag
    schema, user = "gostream_rabbit_" + tag, "gsrabbit_" + tag
    password, broker_password = secrets.token_hex(24), secrets.token_hex(24)
    container = "gostream-rabbit-test-" + tag
    cluster_network = container + "-net"
    cluster_cookie = secrets.token_hex(16)
    cluster_containers = [container] + [container + f"-{index}" for index in (2, 3)] if args.rabbitmq_cluster else [container]
    cluster_nodes = [f"rabbit-test-{index}" for index in range(1, 4)] if args.rabbitmq_cluster else []
    created_containers = []
    work = Path(tempfile.mkdtemp(prefix="gostream-rabbit-"))
    created_db = created_user = started = capacity_policy_active = cluster_network_created = False
    api = None
    ack_proxy = None
    sql_lock = None
    stopped_cluster_follower = None
    stopped_cluster_nodes = []

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
            except LauncherInterrupted:
                raise
            except OSError:
                pass
            time.sleep(0.2)
        raise RuntimeError("timed out waiting for " + label)

    def reachable(url):
        with urllib.request.urlopen(url, timeout=1) as response:
            return response.status == 200

    paused = False
    try:
        created_db = True
        mysql(f"CREATE DATABASE `{schema}`;")
        created_user = True
        mysql(f"CREATE USER '{user}'@'%' IDENTIFIED BY '{password}';")
        mysql(f"GRANT ALL PRIVILEGES ON `{schema}`.* TO '{user}'@'%';")
        amqp_port, http_port = free_port(), free_port()
        while http_port == amqp_port:
            http_port = free_port()
        cluster_http_ports = [http_port]
        cluster_amqp_ports = [amqp_port]
        if args.rabbitmq_cluster:
            used_ports = {amqp_port, http_port}
            while len(cluster_http_ports) < 3:
                candidate = free_port()
                if candidate not in used_ports:
                    cluster_http_ports.append(candidate)
                    used_ports.add(candidate)
            while len(cluster_amqp_ports) < 3:
                candidate = free_port()
                if candidate not in used_ports:
                    cluster_amqp_ports.append(candidate)
                    used_ports.add(candidate)
        # Explicit host ports remain stable across docker stop/start. Docker's
        # automatic host-port allocation can change its assignment on restart.
        if args.rabbitmq_cluster:
            cluster_network_created = True
            run(["docker", "network", "create", cluster_network], capture_output=True)
            for index, name in enumerate(cluster_containers):
                node = cluster_nodes[index]
                command = ["docker", "run", "-d", "--name", name, "--hostname", node,
                           "--network", cluster_network, "--network-alias", node,
                           "-e", "RABBITMQ_NODENAME=rabbit@" + node,
                           "-e", "RABBITMQ_ERLANG_COOKIE=" + cluster_cookie,
                           "-e", "RABBITMQ_DEFAULT_USER=gostream-test",
                           "-e", "RABBITMQ_DEFAULT_PASS=" + broker_password,
                           "-e", "RABBITMQ_SERVER_ADDITIONAL_ERL_ARGS=+S 2:2 -rabbit dead_letter_worker_publisher_confirm_timeout 1000"]
                command += ["-p", f"127.0.0.1:{cluster_http_ports[index]}:15672"]
                command += ["-p", f"127.0.0.1:{cluster_amqp_ports[index]}:5672"]
                command.append("rabbitmq:4.3.7-management")
                created_containers.append(name)
                run(command, capture_output=True)
                if index == 0:
                    started = True
                    wait("broker management HTTP", lambda: reachable(f"http://127.0.0.1:{http_port}"))
                    continue

                # Avoid racing the server's first Erlang cookie initialization
                # with rabbitmqctl's own Erlang CLI startup.
                time.sleep(9)
                startup_deadline = time.monotonic() + 60
                while time.monotonic() < startup_deadline:
                    ready = subprocess.run(["docker", "exec", name, "rabbitmqctl", "await_startup"],
                                           capture_output=True, text=True)
                    if ready.returncode == 0:
                        break
                    state = subprocess.run(["docker", "inspect", "--format", "{{.State.Status}}", name],
                                           capture_output=True, text=True).stdout.strip()
                    if state != "running":
                        logs = subprocess.run(["docker", "logs", name], capture_output=True, text=True)
                        raise RuntimeError(f"RabbitMQ cluster node exited: {name}: {(logs.stdout + logs.stderr)[-3000:]}")
                    time.sleep(1)
                else:
                    raise RuntimeError(f"RabbitMQ cluster node did not start: {name}")
                run(["docker", "exec", name, "rabbitmqctl", "stop_app"], capture_output=True)
                run(["docker", "exec", name, "rabbitmqctl", "join_cluster", "rabbit@" + cluster_nodes[0]], capture_output=True)
                run(["docker", "exec", name, "rabbitmqctl", "start_app"], capture_output=True)

            cluster_status = run(["docker", "exec", container, "rabbitmqctl", "cluster_status"], capture_output=True).stdout
            if not all("rabbit@" + node in cluster_status for node in cluster_nodes):
                raise RuntimeError("RabbitMQ did not form the expected three-node cluster")
        else:
            created_containers.append(container)
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
        broker_urls = ",".join(
            f"amqp://gostream-test:{broker_password}@127.0.0.1:{port}/"
            for port in cluster_amqp_ports
        ) if args.rabbitmq_cluster else broker_url
        env = dict(os.environ)
        env.update(GOSTREAM_TEST_MYSQL_DSN=dsn, GOSTREAM_TEST_RABBITMQ_MYSQL_DSN=dsn,
                   GOSTREAM_TEST_RABBITMQ_URL=broker_url, GOSTREAM_TEST_RABBITMQ_CONTAINER=container,
                   GOSTREAM_TEST_RABBITMQ_DLQ_WAIT="15s")
        run([args.go_bin, "test", "./internal/migrations", "-run", "^TestApplyMySQLIntegration$", "-count=1"], env=env)
        cluster_only_mode = any(cluster_modes)
        if not cluster_only_mode:
            run([args.go_bin, "test", "-race", "./internal/platform", "-run", "^TestRabbitMQNotificationIntegration$", "-count=1", "-v", "-timeout=4m"], env=env)
        # Apply the temporary capacity guard before the API declares its quorum
        # queues, so the broker enforces it from queue creation onward.
        if not cluster_only_mode:
            run(["docker", "exec", container, "rabbitmqctl", "set_operator_policy",
                 capacity_policy, "^" + re.escape(dead_qname) + "$",
                 '{"max-length":1,"overflow":"reject-publish"}',
                 "--priority", "100", "--apply-to", "quorum_queues"], capture_output=True)
            capacity_policy_active = True
        env.update(DB_DRIVER="mysql", DB_DSN=dsn, RABBITMQ_URL=broker_url,
                   RABBITMQ_URLS=broker_urls,
                   RABBITMQ_QUEUE=api_queue, JWT_SECRET=secrets.token_hex(32),
                   ADDR=f"127.0.0.1:{free_port()}", MEDIA_DIR=str(work / "media"), REDIS_ADDR="", FFMPEG_BIN="")
        for executable in ("gostream", "gostream-events"):
            run([args.go_bin, "build", "-o", str(work / executable), "./cmd/" + executable])
        with (work / "api.log").open("w") as log:
            with finish_process_start():
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

            if cluster_only_mode:
                auth_handler = urllib.request.HTTPBasicAuthHandler()
                for management_port in cluster_http_ports:
                    auth_handler.add_password(realm="RabbitMQ Management", uri=f"http://127.0.0.1:{management_port}",
                                              user="gostream-test", passwd=broker_password)
                opener = urllib.request.build_opener(auth_handler)

                def cluster_queue_info():
                    last_error = None
                    for management_port in cluster_http_ports:
                        try:
                            with opener.open(f"http://127.0.0.1:{management_port}/api/queues/%2f/{api_queue}", timeout=3) as response:
                                return json.load(response)
                        except (urllib.error.URLError, TimeoutError) as error:
                            last_error = error
                    if last_error is not None:
                        raise last_error
                    raise RuntimeError("no RabbitMQ management endpoint configured")

                current_queue = cluster_queue_info()
                members = current_queue.get("members", [])
                original_leader = current_queue.get("leader")
                if args.cluster_majority_only:
                    other_nodes = [node for node in members if node != original_leader]
                    failure_nodes = [original_leader] + other_nodes[:1]
                    failure_mode = "majority"
                elif args.cluster_leader_only:
                    failure_nodes = [original_leader]
                    failure_mode = "leader"
                else:
                    candidates = [node for node in members
                                  if node != original_leader
                                  and node != "rabbit@" + cluster_nodes[0]]
                    failure_nodes = candidates[:1]
                    failure_mode = "follower"
                if len(members) != 3 or len(failure_nodes) != (2 if args.cluster_majority_only else 1) or not set(failure_nodes) <= set(members):
                    raise RuntimeError(f"expected a {failure_mode}: {current_queue}")
                failure_containers = [
                    cluster_containers[cluster_nodes.index(node.split("@", 1)[1])]
                    for node in failure_nodes
                ]
                try:
                    for failure_container in failure_containers:
                        run(["docker", "stop", "--time=2", failure_container], capture_output=True)
                        stopped_cluster_nodes.append(failure_container)
                    if args.cluster_leader_only:
                        wait("quorum elects a replacement leader", lambda: (
                            cluster_queue_info().get("leader") != original_leader
                            and original_leader not in cluster_queue_info().get("online", [])), seconds=30)
                    cluster_actor = call("/api/users", {
                        "username": "cluster_actor", "nickname": "Cluster Actor",
                        "password": "fixture-password-123",
                    })
                    call(f"/api/users/me/following/{owner['user']['id']}", None,
                         cluster_actor["access_token"], "PUT")
                    if args.cluster_majority_only:
                        wait("source outbox claim during majority loss", lambda: mysql(
                            f"SELECT COUNT(*) FROM `{schema}`.notifications n "
                            f"JOIN `{schema}`.outbox_events s ON s.event_key=CONCAT('notification:',n.id) "
                            f"WHERE n.user_id={owner['user']['id']} AND n.actor_id={cluster_actor['user']['id']} "
                            "AND s.published_at IS NULL AND s.dead_at IS NULL AND s.attempts >= 1;") == "1",
                             seconds=30)
                        # Let the publisher's bounded confirm attempt fail while
                        # only one of the three quorum members is available.
                        time.sleep(6)
                        pending_source = mysql(
                            f"SELECT COUNT(*) FROM `{schema}`.notifications n "
                            f"JOIN `{schema}`.outbox_events s ON s.event_key=CONCAT('notification:',n.id) "
                            f"WHERE n.user_id={owner['user']['id']} AND n.actor_id={cluster_actor['user']['id']} "
                            "AND s.published_at IS NULL AND s.dead_at IS NULL;")
                        delivery_rows = mysql(
                            f"SELECT COUNT(*) FROM `{schema}`.notifications n "
                            f"JOIN `{schema}`.outbox_events d ON d.event_key=CONCAT('broker-delivery:notification:',n.id) "
                            f"WHERE n.user_id={owner['user']['id']} AND n.actor_id={cluster_actor['user']['id']};")
                        if pending_source != "1" or delivery_rows != "0":
                            raise AssertionError("majority loss must retain the source event without a delivery side effect")

                        recovery_container = failure_containers[-1]
                        run(["docker", "start", recovery_container], capture_output=True)
                        stopped_cluster_nodes.remove(recovery_container)

                        def quorum_online_again():
                            state = cluster_queue_info()
                            online = set(state.get("online", []))
                            return len(set(members) & online) >= 2 and state.get("leader") in online

                        wait("quorum restored with two members", quorum_online_again, seconds=60)
                        wait("outbox delivery resumes after majority restoration", lambda: mysql(
                            f"SELECT COUNT(*) FROM `{schema}`.notifications n "
                            f"JOIN `{schema}`.outbox_events s ON s.event_key=CONCAT('notification:',n.id) "
                            f"JOIN `{schema}`.outbox_events d ON d.event_key=CONCAT('broker-delivery:notification:',n.id) "
                            f"WHERE n.user_id={owner['user']['id']} AND n.actor_id={cluster_actor['user']['id']} "
                            "AND s.published_at IS NOT NULL AND d.published_at IS NOT NULL;") == "1",
                             seconds=90)
                    else:
                        wait("API source and delivery complete with quorum node down", lambda: mysql(
                            f"SELECT COUNT(*) FROM `{schema}`.notifications n "
                            f"JOIN `{schema}`.outbox_events s ON s.event_key=CONCAT('notification:',n.id) "
                            f"JOIN `{schema}`.outbox_events d ON d.event_key=CONCAT('broker-delivery:notification:',n.id) "
                            f"WHERE n.user_id={owner['user']['id']} AND n.actor_id={cluster_actor['user']['id']} "
                            "AND s.published_at IS NOT NULL AND d.published_at IS NOT NULL;") == "1",
                             seconds=60)
                finally:
                    for failure_container in reversed(failure_containers):
                        if failure_container in stopped_cluster_nodes:
                            run(["docker", "start", failure_container], capture_output=True)
                            stopped_cluster_nodes.remove(failure_container)

                def all_cluster_replicas_online():
                    state = cluster_queue_info()
                    return set(state.get("members", [])) == set(members) and set(members) <= set(state.get("online", []))

                wait("failed node rejoins and all queue replicas are online", all_cluster_replicas_online, seconds=60)
                api.send_signal(signal.SIGTERM)
                try:
                    shutdown_status = api.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    # SIGQUIT writes goroutine stacks to api.log before exit.
                    api.send_signal(signal.SIGQUIT)
                    api.wait(timeout=5)
                    print("cluster_follower_shutdown_log_tail=" + (work / "api.log").read_text()[-12000:], flush=True)
                    raise RuntimeError("API graceful shutdown after quorum follower test timed out")
                if shutdown_status != 0:
                    raise RuntimeError(f"API graceful shutdown after quorum follower test exited {shutdown_status}")
                api = None
                final_queue = cluster_queue_info()
                result = {"rabbitmq": "4.3.7", "cluster_nodes": 3,
                          "cluster_failure_mode": failure_mode,
                          "cluster_failure_nodes": failure_nodes,
                          "cluster_leader_before": original_leader,
                          "cluster_leader_after": final_queue.get("leader"),
                          "cluster_failure_recovery": True,
                          "source_and_delivery_completed_during_outage": not args.cluster_majority_only}
                if args.cluster_majority_only:
                    result["source_retained_during_majority_loss"] = True
                    result["delivery_resumed_after_majority_restore"] = True
                elif args.cluster_leader_only:
                    result["cluster_leader_failure_recovery"] = True
                else:
                    result["cluster_follower"] = failure_nodes[0]
                    result["cluster_follower_failure_recovery"] = True
                print(json.dumps(result), flush=True)
                return

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

            with finish_process_start():
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

            with finish_process_start():
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
            def management(method, path, body=None):
                request = urllib.request.Request(
                    f"http://127.0.0.1:{http_port}{path}",
                    data=None if body is None else json.dumps(body).encode(),
                    headers={"Content-Type": "application/json"}, method=method)
                with opener.open(request, timeout=5) as response:
                    raw = response.read()
                    return json.loads(raw) if raw else None

            def queue_info(qname):
                try:
                    return management("GET", f"/api/queues/%2f/{qname}")
                except LauncherInterrupted:
                    raise
                except Exception:
                    return {}

            def queue_depth(qname=f"gostream.process.{tag}"):
                return queue_info(qname).get("messages", -1)
            wait("broker queue depth returns to 0", lambda: queue_depth() == 0, seconds=15)

            cluster_follower_recovery = False
            if args.rabbitmq_cluster:
                source_info = queue_info(api_queue)
                leader = source_info.get("leader")
                members = source_info.get("members", [])
                candidates = [node for node in members
                              if node != leader and node != "rabbit@" + cluster_nodes[0]]
                if len(members) != 3 or not candidates:
                    raise RuntimeError(f"expected a non-leader quorum follower: {source_info}")
                follower_node = candidates[0]
                follower_index = cluster_nodes.index(follower_node.split("@", 1)[1])
                stopped_cluster_follower = cluster_containers[follower_index]
                run(["docker", "stop", "--time=2", stopped_cluster_follower], capture_output=True)
                try:
                    cluster_actor = call("/api/users", {
                        "username": "cluster_actor", "nickname": "Cluster Actor",
                        "password": "fixture-password-123",
                    })
                    call(f"/api/users/me/following/{owner['user']['id']}", None,
                         cluster_actor["access_token"], "PUT")
                    try:
                        wait("API notification confirms while a quorum follower is down",
                             lambda: mysql(
                             f"SELECT COUNT(*) FROM `{schema}`.notifications n "
                             f"JOIN `{schema}`.outbox_events s ON s.event_key=CONCAT('notification:',n.id) "
                             f"JOIN `{schema}`.outbox_events d ON d.event_key=CONCAT('broker-delivery:notification:',n.id) "
                             f"WHERE n.user_id={owner['user']['id']} AND n.actor_id={cluster_actor['user']['id']} AND n.kind='follow' "
                             "AND n.text='started following you' "
                             "AND s.published_at IS NOT NULL AND d.published_at IS NOT NULL;") == "1",
                             seconds=30)
                    except RuntimeError:
                        print("cluster_follower_outage_state=" + mysql(
                            f"SELECT CONCAT(n.id,':',n.actor_id,':',n.text,':',COALESCE(s.attempts,-1),':',"
                            "COALESCE(s.published_at IS NOT NULL,0),':',COALESCE(s.last_error,''),':',"
                            "COALESCE(d.attempts,-1),':',COALESCE(d.published_at IS NOT NULL,0),':',"
                            "COALESCE(d.last_error,'')) FROM `{schema}`.notifications n "
                            f"LEFT JOIN `{schema}`.outbox_events s ON s.event_key=CONCAT('notification:',n.id) "
                            f"LEFT JOIN `{schema}`.outbox_events d ON d.event_key=CONCAT('broker-delivery:notification:',n.id) "
                            f"WHERE n.user_id={owner['user']['id']} AND n.actor_id={cluster_actor['user']['id']} "
                            "AND n.kind='follow' ORDER BY n.id DESC LIMIT 5;"
                        ), flush=True)
                        print("cluster_follower_api_log_tail=" + (work / "api.log").read_text()[-5000:], flush=True)
                        raise
                    wait("source queue drains while follower is down", lambda: queue_depth() == 0, seconds=10)
                finally:
                    run(["docker", "start", stopped_cluster_follower], capture_output=True)
                    stopped_cluster_follower = None

                wait("quorum follower rejoins and all replicas are online",
                     lambda: set(queue_info(api_queue).get("members", [])) <=
                             set(queue_info(api_queue).get("online", [])), seconds=60)
                if not set(members) <= set(queue_info(api_queue).get("online", [])):
                    raise AssertionError("quorum queue did not restore all original online members")
                cluster_follower_recovery = True
                print("cluster_follower_failure_recovery=true", flush=True)

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

            def publish_dead_letter(event_id, payload):
                result = management("POST", f"/api/exchanges/%2f/gostream.process.{tag}.dlx/publish",
                                    {"properties": {"delivery_mode": 2, "message_id": event_id},
                                     "routing_key": "notification.created", "payload": payload,
                                     "payload_encoding": "string"})
                if not result.get("routed", False):
                    raise AssertionError("broker did not route DLQ capacity fixture " + event_id)

            def fetch_queue(qname, count, ackmode):
                return management("POST", f"/api/queues/%2f/{qname}/get",
                                  {"count": count, "ackmode": ackmode, "encoding": "auto"})

            def drain_queue(qname, seconds=15):
                drained, deadline = [], time.monotonic() + seconds
                empty_since = None
                while time.monotonic() < deadline:
                    depth = queue_depth(qname)
                    if depth < 0:
                        raise RuntimeError("could not inspect queue while draining " + qname)
                    if depth == 0:
                        empty_since = empty_since or time.monotonic()
                        if time.monotonic() - empty_since >= 1:
                            return drained
                        time.sleep(0.2)
                        continue
                    empty_since = None
                    batch = fetch_queue(qname, depth, "ack_requeue_false")
                    if batch:
                        drained.extend(batch)
                    else:
                        time.sleep(0.2)
                raise RuntimeError(f"queue {qname} did not remain empty; final depth={queue_depth(qname)}")

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

            def broker_metric(result):
                with urllib.request.urlopen(base + "/metrics", timeout=2) as response:
                    body = response.read().decode()
                match = re.search(r'^gostream_rabbitmq_messages_total\{result="' + re.escape(result) + r'"\} ([0-9.eE+\-]+)$', body, re.M)
                return float(match.group(1)) if match else 0

            def duplicate_metric():
                return broker_metric("duplicate")

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

            # Scenario 5: kill the real API after its SQL consumer commit but
            # before its AMQP basic.ack reaches RabbitMQ.
            api.send_signal(signal.SIGTERM)
            if api.wait(timeout=10) != 0:
                raise RuntimeError("API shutdown before pre-ACK process-kill test failed")
            api = None

            with finish_process_start():
                ack_proxy = AckGateProxy("127.0.0.1", amqp_port)
            env["RABBITMQ_URL"] = (
                f"amqp://gostream-test:{broker_password}@127.0.0.1:{ack_proxy.address[1]}/"
            )
            env["RABBITMQ_URLS"] = env["RABBITMQ_URL"]
            with finish_process_start():
                api = subprocess.Popen([str(work / "gostream")], cwd=ROOT, env=env,
                                       stdout=log, stderr=subprocess.STDOUT)
            wait("API readiness through ACK gate", lambda: reachable(base + "/ready"))
            actor4 = call("/api/users", {"username": "process_actor4", "nickname": "Actor4",
                                          "password": "fixture-password-123"})
            ack_proxy.arm()
            call(f"/api/users/me/following/{owner['user']['id']}", None,
                 actor4["access_token"], "PUT")
            if not ack_proxy.ack_held.wait(45):
                raise RuntimeError("production consumer did not emit basic.ack through the test gate")

            preack_notification_id = mysql(
                f"SELECT id FROM `{schema}`.notifications WHERE user_id={owner['user']['id']} "
                f"AND actor_id={actor4['user']['id']} AND kind='follow';")
            if not preack_notification_id:
                raise AssertionError("follow notification was not committed before basic.ack")
            preack_event_id = "notification:" + preack_notification_id
            delivery_key = "broker-delivery:" + preack_event_id
            wait("consumer SQL commit before held basic.ack", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='{delivery_key}' "
                "AND topic='notification.deliver';") == "1")
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.notifications WHERE user_id={owner['user']['id']} "
                     f"AND actor_id={actor4['user']['id']} AND kind='follow';") != "1":
                raise AssertionError("pre-ACK notification was not unique before process kill")

            api.send_signal(signal.SIGKILL)
            if api.wait(timeout=10) != -signal.SIGKILL:
                raise RuntimeError("API did not exit via SIGKILL while basic.ack was held")
            api = None
            ack_proxy.close()
            ack_proxy = None

            env["RABBITMQ_URL"] = broker_url
            env["RABBITMQ_URLS"] = broker_urls
            with finish_process_start():
                api = subprocess.Popen([str(work / "gostream")], cwd=ROOT, env=env,
                                       stdout=log, stderr=subprocess.STDOUT)
            wait("API readiness after pre-ACK SIGKILL", lambda: reachable(base + "/ready"))
            wait("redelivery classified as duplicate after pre-ACK SIGKILL",
                 lambda: duplicate_metric() >= 1, seconds=45)
            wait("pre-ACK delivery dispatch completes after redelivery", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='{delivery_key}' "
                "AND topic='notification.deliver' AND published_at IS NOT NULL;") == "1")
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.notifications WHERE user_id={owner['user']['id']} "
                     f"AND actor_id={actor4['user']['id']} AND kind='follow';") != "1":
                raise AssertionError("pre-ACK redelivery duplicated the business notification")
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='{delivery_key}';") != "1":
                raise AssertionError("pre-ACK redelivery duplicated its durable delivery row")
            wait("queue drains after pre-ACK redelivery", lambda: queue_depth() == 0, seconds=15)

            # Scenario 6: hold the SQL source row after RabbitMQ confirms the
            # publication, then SIGKILL before published_at can be committed.
            actor5 = call("/api/users", {"username": "process_actor5", "nickname": "Actor5",
                                          "password": "fixture-password-123"})
            published_before = broker_metric("published")
            run(["docker", "pause", container], capture_output=True)
            paused = True
            call(f"/api/users/me/following/{owner['user']['id']}", None,
                 actor5["access_token"], "PUT")
            producer_notification_id = mysql(
                f"SELECT id FROM `{schema}`.notifications WHERE user_id={owner['user']['id']} "
                f"AND actor_id={actor5['user']['id']} AND kind='follow';")
            if not producer_notification_id:
                raise AssertionError("producer fault fixture did not commit its notification")
            producer_event_id = "notification:" + producer_notification_id
            wait("producer outbox claim during broker pause", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='{producer_event_id}' "
                "AND topic='notification.created' AND published_at IS NULL AND attempts >= 1;") == "1")
            producer_row_id = mysql(
                f"SELECT id FROM `{schema}`.outbox_events WHERE event_key='{producer_event_id}' "
                "AND topic='notification.created';")
            if not producer_row_id:
                raise AssertionError("producer source outbox row is missing")
            with finish_process_start():
                sql_lock = start_outbox_row_lock(args.mysql_container, schema, producer_row_id)

            run(["docker", "unpause", container], capture_output=True)
            paused = False
            wait("publisher confirm received before SQL completion", lambda:
                 broker_metric("published") >= published_before + 1, seconds=45)
            delivery_key = "broker-delivery:" + producer_event_id
            wait("consumer receives confirmed publisher event", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='{delivery_key}' "
                "AND topic='notification.deliver';") == "1")

            def app_waiting_to_complete_source():
                return mysql(
                    "SELECT COUNT(*) FROM performance_schema.data_lock_waits w "
                    "JOIN performance_schema.data_locks req "
                    "ON req.ENGINE=w.ENGINE AND req.ENGINE_LOCK_ID=w.REQUESTING_ENGINE_LOCK_ID "
                    "JOIN performance_schema.data_locks blk "
                    "ON blk.ENGINE=w.ENGINE AND blk.ENGINE_LOCK_ID=w.BLOCKING_ENGINE_LOCK_ID "
                    "JOIN performance_schema.threads rt ON rt.THREAD_ID=w.REQUESTING_THREAD_ID "
                    "JOIN performance_schema.threads bt ON bt.THREAD_ID=w.BLOCKING_THREAD_ID "
                    f"WHERE rt.PROCESSLIST_USER='{user}' AND rt.PROCESSLIST_INFO LIKE 'UPDATE%' "
                    f"AND bt.PROCESSLIST_USER='root' AND req.OBJECT_SCHEMA='{schema}' "
                    "AND req.OBJECT_NAME='outbox_events' AND blk.OBJECT_SCHEMA=req.OBJECT_SCHEMA "
                    "AND blk.OBJECT_NAME=req.OBJECT_NAME AND blk.INDEX_NAME='PRIMARY';"
                ) != "0"

            wait("confirmed publisher blocked on source published_at update",
                 app_waiting_to_complete_source, seconds=30)
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE id={producer_row_id} "
                     "AND published_at IS NULL AND attempts=1;") != "1":
                raise AssertionError("source row changed before publisher SQL completion")

            api.send_signal(signal.SIGKILL)
            if api.wait(timeout=10) != -signal.SIGKILL:
                raise RuntimeError("API did not exit via SIGKILL after confirm and before SQL completion")
            api = None
            release_outbox_row_lock(sql_lock)
            sql_lock = None
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE id={producer_row_id} "
                     "AND published_at IS NULL AND attempts=1;") != "1":
                raise AssertionError("confirmed-but-uncommitted source state was not preserved after SIGKILL")
            mysql(f"UPDATE `{schema}`.outbox_events SET available_at=UTC_TIMESTAMP(3) WHERE id={producer_row_id};")

            with finish_process_start():
                api = subprocess.Popen([str(work / "gostream")], cwd=ROOT, env=env,
                                       stdout=log, stderr=subprocess.STDOUT)
            wait("API readiness after producer post-confirm SIGKILL", lambda: reachable(base + "/ready"))
            wait("redelivery classified as duplicate after producer SIGKILL",
                 lambda: duplicate_metric() >= 1, seconds=45)
            wait("producer source completed after confirmed republish", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE id={producer_row_id} "
                "AND published_at IS NOT NULL AND attempts >= 2;") == "1")
            wait("producer redelivery dispatch completes", lambda: mysql(
                f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='{delivery_key}' "
                "AND topic='notification.deliver' AND published_at IS NOT NULL;") == "1")
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.notifications WHERE user_id={owner['user']['id']} "
                     f"AND actor_id={actor5['user']['id']} AND kind='follow';") != "1":
                raise AssertionError("publisher retry duplicated the business notification")
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE event_key='{delivery_key}';") != "1":
                raise AssertionError("publisher retry duplicated its durable delivery row")
            wait("queue drains after confirmed publisher retry", lambda: queue_depth() == 0, seconds=15)

            # Scenario 7: maintain a real 30-second SQL outbox arrival stream
            # while production publisher, consumer, and delivery workers run.
            sustained_batches = 30
            events_per_batch = 10
            sustained_total = sustained_batches * events_per_batch
            sustained_started = time.monotonic()
            for batch in range(sustained_batches):
                values, event_keys = [], []
                for item in range(events_per_batch):
                    event_key = f"sustained-{tag}-{batch}-{item}"
                    values.append(
                        f"({owner['user']['id']},{actor3['user']['id']},'follow',"
                        f"'sustained-{batch}-{item}','{event_key}',UTC_TIMESTAMP(3))"
                    )
                    event_keys.append("'" + event_key + "'")
                inserted = mysql(
                    f"START TRANSACTION; INSERT INTO `{schema}`.notifications "
                    "(user_id,actor_id,kind,text,event_key,created_at) VALUES "
                    + ",".join(values) + "; "
                    f"INSERT INTO `{schema}`.outbox_events "
                    "(event_key,topic,payload,attempts,available_at,created_at) "
                    f"SELECT CONCAT('notification:',n.id),'notification.created',"
                    "JSON_OBJECT('notification_id',n.id),0,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3) "
                    f"FROM `{schema}`.notifications n WHERE n.event_key IN ({','.join(event_keys)}); "
                    "SELECT ROW_COUNT(); COMMIT;"
                )
                if inserted != str(events_per_batch):
                    raise AssertionError(
                        f"sustained batch {batch} created {inserted} source rows, expected {events_per_batch}"
                    )
                if batch + 1 < sustained_batches:
                    target = sustained_started + batch + 1
                    time.sleep(max(0, target - time.monotonic()))
            sustained_seconds = time.monotonic() - sustained_started
            if sustained_seconds < sustained_batches - 1.5:
                raise AssertionError(f"outbox arrival stream lasted only {sustained_seconds:.3f}s")

            def sustained_source_count():
                return mysql(
                    f"SELECT COUNT(*) FROM `{schema}`.outbox_events o JOIN `{schema}`.notifications n "
                    "ON o.event_key=CONCAT('notification:',n.id) "
                    f"WHERE n.event_key LIKE 'sustained-{tag}-%' "
                    "AND o.topic='notification.created' AND o.published_at IS NOT NULL;"
                )

            def sustained_delivery_count():
                return mysql(
                    f"SELECT COUNT(*) FROM `{schema}`.outbox_events d JOIN `{schema}`.notifications n "
                    "ON d.event_key=CONCAT('broker-delivery:notification:',n.id) "
                    f"WHERE n.event_key LIKE 'sustained-{tag}-%' "
                    "AND d.topic='notification.deliver' AND d.published_at IS NOT NULL;"
                )

            drain_started = time.monotonic()
            wait("all sustained source events receive publisher confirms",
                 lambda: sustained_source_count() == str(sustained_total), seconds=60)
            wait("all sustained consumer deliveries are dispatched",
                 lambda: sustained_delivery_count() == str(sustained_total), seconds=60)
            if mysql(
                f"SELECT COUNT(*) FROM `{schema}`.notifications WHERE event_key LIKE 'sustained-{tag}-%';"
            ) != str(sustained_total):
                raise AssertionError("sustained stream notification count is not exact")
            if mysql(f"SELECT COUNT(*) FROM `{schema}`.outbox_events WHERE dead_at IS NOT NULL;") != "0":
                raise AssertionError("sustained stream produced dead SQL outbox rows")
            wait("RabbitMQ queue drains after sustained stream", lambda: queue_depth() == 0, seconds=30)
            sustained_drain_seconds = time.monotonic() - drain_started

            # Scenario 8: saturate the actual quorum DLQ using a temporary
            # operator policy. At-least-once dead lettering must retain poison
            # in the source queue until the DLQ accepts it.
            try:
                wait("DLQ operator policy becomes effective",
                     lambda: queue_info(dead_qname).get("effective_policy_definition", {}).get("max-length") == 1,
                     seconds=15)
                filler_id = f"dlq-capacity-filler-{tag}"
                publish_dead_letter(filler_id, "capacity-filler")
                wait("DLQ capacity fixture fills its one-message limit",
                     lambda: queue_depth(dead_qname) == 1, seconds=10)

                full_poison = [(f"dlq-full-{tag}-{i}", "{poison-while-dlq-full") for i in range(2)]
                dead_before = broker_metric("dead")
                for event_id, payload in full_poison:
                    publish_envelope(event_id, payload)
                wait("production consumer rejects poison while DLQ is full",
                     lambda: broker_metric("dead") >= dead_before + len(full_poison), seconds=15)
                retained_until = time.monotonic() + 10
                while time.monotonic() < retained_until and queue_depth() < 1:
                    time.sleep(0.2)
                if queue_depth() < 1:
                    source_state = queue_info(f"gostream.process.{tag}")
                    dead_state = queue_info(dead_qname)
                    fields = ("messages", "messages_ready", "messages_unacknowledged",
                              "effective_policy_definition", "state")
                    raise AssertionError(
                        "at-least-once DLQ retention not visible at source; "
                        f"source={json.dumps({key: source_state.get(key) for key in fields})}, "
                        f"dead={json.dumps({key: dead_state.get(key) for key in fields})}, "
                        f"dead_metric_delta={broker_metric('dead') - dead_before}"
                    )
                for _ in range(10):
                    if queue_depth() < 1:
                        raise AssertionError("full DLQ did not retain poison at its source")
                    time.sleep(0.2)

                expected_full_poison = {event_id for event_id, _ in full_poison}
                occupied = fetch_queue(dead_qname, queue_depth(dead_qname), "ack_requeue_false")
                occupied_ids = [item.get("properties", {}).get("message_id") for item in occupied]
                if filler_id not in occupied_ids:
                    raise AssertionError("could not remove the expected DLQ capacity filler")
                if not set(occupied_ids).issubset(expected_full_poison | {filler_id}):
                    raise AssertionError("unexpected message occupied the saturated DLQ")
                recovered_ids = occupied_ids
                # The DLX worker may already have filled the released slot with
                # one poison message. Free all occupied target capacity before
                # waiting for the source-retained message to retry.
                run(["docker", "exec", container, "rabbitmqctl", "clear_operator_policy",
                     capacity_policy], capture_output=True)
                capacity_policy_active = False
                missing = len(expected_full_poison - set(recovered_ids))
                try:
                    wait("retained poison transfers after DLQ capacity recovers",
                         lambda: queue_depth(dead_qname) >= missing and queue_depth() == 0,
                         seconds=60)
                except RuntimeError:
                    fields = ("messages", "messages_ready", "messages_unacknowledged",
                              "effective_policy_definition", "state", "leader", "members", "online")
                    print("dlq_capacity_recovery_state=" + json.dumps({
                        "source": {key: queue_info(api_queue).get(key) for key in fields},
                        "dead": {key: queue_info(dead_qname).get(key) for key in fields},
                        "missing_expected_ids": sorted(expected_full_poison - set(recovered_ids)),
                        "api_log_tail": (work / "api.log").read_text()[-5000:],
                    }), flush=True)
                    raise
                recovered = drain_queue(dead_qname)
                recovered_ids.extend(item.get("properties", {}).get("message_id") for item in recovered)
                if not expected_full_poison.issubset(set(recovered_ids)):
                    raise AssertionError("DLQ recovery lost an original poison identity")
                expected_payloads = dict(full_poison)
                for item in occupied + recovered:
                    props = item.get("properties", {})
                    event_id = props.get("message_id")
                    if event_id == filler_id:
                        continue
                    if (item.get("payload") != expected_payloads.get(event_id)
                            or props.get("delivery_mode") != 2
                            or props.get("headers", {}).get("x-first-death-reason") != "rejected"):
                        raise AssertionError("DLQ recovery changed poison payload or durable death metadata")
                if queue_depth() != 0:
                    raise AssertionError("recovered poison remained at the source after DLQ drain")
                print("dlq_capacity_backpressure_recovery=true", flush=True)
            finally:
                if capacity_policy_active:
                    run(["docker", "exec", container, "rabbitmqctl", "clear_operator_policy",
                         capacity_policy], capture_output=True)

            # Scenario 9: independent valid identities and poison envelopes are
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
            wait("exact poison queue depth", lambda: queue_depth(dead_qname) == len(poison), seconds=20)
            # Management fetches the whole batch before requeueing it, retaining
            # the same messages for the operator CLI replay below.
            dead = fetch_queue(dead_qname, len(poison), "ack_requeue_true")
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
        try:
            wait("operator source copies", lambda: queue_depth() == 3 and queue_depth(dead_qname) == 0, seconds=15)
        except RuntimeError:
            fields = ("messages", "messages_ready", "messages_unacknowledged",
                      "effective_policy_definition", "state")
            print("operator_replay_queue_state=" + json.dumps({
                "source": {key: queue_info(api_queue).get(key) for key in fields},
                "dead": {key: queue_info(dead_qname).get(key) for key in fields},
            }), flush=True)
            raise
        print(json.dumps({"rabbitmq": "4.3.7", "mysql": "8.4", "real_broker_matrix": True,
                          "consumer_pre_ack_process_kill": True,
                          "producer_post_confirm_process_kill": True,
                          "sustained_source_events": sustained_total,
                          "sustained_injection_seconds": round(sustained_seconds, 3),
                          "sustained_drain_seconds": round(sustained_drain_seconds, 3),
                          "cluster_follower_failure_recovery": cluster_follower_recovery,
                          "api_worker_lifecycle": True, "operator_cli": True}), flush=True)
    finally:
        # A second signal must not interrupt deletion of isolated resources.
        for number in (signal.SIGINT, signal.SIGTERM):
            signal.signal(number, signal.SIG_IGN)
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
        if sql_lock is not None:
            try:
                release_outbox_row_lock(sql_lock)
            except (OSError, subprocess.TimeoutExpired) as exc:
                cleanup_errors.append(str(exc))
        if ack_proxy is not None:
            try:
                ack_proxy.close()
            except OSError as exc:
                cleanup_errors.append(str(exc))
        # All broker containers belong to this disposable run; they can be
        # removed directly, including nodes stopped by a fault scenario.
        for name in created_containers:
            try:
                existing = run(["docker", "ps", "-a", "--filter", "name=^/" + name + "$",
                                "--format", "{{.ID}}"], capture_output=True).stdout.strip()
                if existing:
                    run(["docker", "rm", "-f", "-v", name], capture_output=True)
            except RuntimeError as exc:
                cleanup_errors.append(str(exc))
        if cluster_network_created:
            try:
                existing = run(["docker", "network", "ls", "--filter", "name=^" + cluster_network + "$",
                                "--format", "{{.ID}}"], capture_output=True).stdout.strip()
                if existing:
                    run(["docker", "network", "rm", cluster_network], capture_output=True)
            except RuntimeError as exc:
                cleanup_errors.append(str(exc))
        for needed, cleanup in [
            (created_db, lambda: mysql(f"DROP DATABASE IF EXISTS `{schema}`;")),
            (created_user, lambda: mysql(f"DROP USER IF EXISTS '{user}'@'%';")),
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
        for kind, command in [
            ("brokers", ["docker", "ps", "-a", "--filter", "name=^/" + container + "(-[23])?$", "--format", "{{.ID}}"]),
            ("network", ["docker", "network", "ls", "--filter", "name=^" + cluster_network + "$", "--format", "{{.ID}}"]),
        ]:
            try:
                remaining = run(command, capture_output=True).stdout.strip()
                print("remaining_" + kind + "=" + str(bool(remaining)).lower(), flush=True)
                if remaining:
                    cleanup_errors.append("isolated " + kind + " survived cleanup")
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
    try:
        main()
    except LauncherInterrupted as error:
        print("RabbitMQ launcher interrupted; cleanup completed", file=sys.stderr)
        raise SystemExit(128 + int(error.args[0]))
