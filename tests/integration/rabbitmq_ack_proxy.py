"""Test-only AMQP TCP proxy that can hold one consumer basic.ack frame."""

import socket
import threading


AMQP_PROTOCOL_HEADER_SIZE = 8
AMQP_FRAME_HEADER_SIZE = 7
AMQP_BASIC_ACK = b"\x00\x3c\x00\x50"


def _recv_exact(sock, size):
    data = bytearray()
    while len(data) < size:
        chunk = sock.recv(size - len(data))
        if not chunk:
            if data:
                raise ConnectionError("peer closed during an AMQP frame")
            return None
        data.extend(chunk)
    return bytes(data)


class AckGateProxy:
    """Forward AMQP traffic and suppress the first basic.ack after arm()."""

    def __init__(self, broker_host, broker_port):
        self._broker = (broker_host, broker_port)
        self._stopping = threading.Event()
        self._armed = threading.Event()
        self.ack_held = threading.Event()
        self._lock = threading.Lock()
        self._connections = set()
        self._threads = []

        self._listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._listener.bind(("127.0.0.1", 0))
        self._listener.listen()
        self._listener.settimeout(0.2)
        self.address = self._listener.getsockname()

        self._accept_thread = threading.Thread(target=self._accept, daemon=True)
        self._accept_thread.start()

    def arm(self):
        self._armed.set()

    def close(self):
        self._stopping.set()
        try:
            self._listener.close()
        except OSError:
            pass
        with self._lock:
            connections = list(self._connections)
            threads = list(self._threads)
        for conn in connections:
            self._close_socket(conn)
        self._accept_thread.join(timeout=2)
        for thread in threads:
            thread.join(timeout=2)

    def _accept(self):
        while not self._stopping.is_set():
            try:
                client, _ = self._listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            self._track(client)
            thread = threading.Thread(target=self._handle, args=(client,), daemon=True)
            with self._lock:
                self._threads.append(thread)
            thread.start()

    def _handle(self, client):
        upstream = None
        reverse = None
        try:
            upstream = socket.create_connection(self._broker, timeout=5)
            upstream.settimeout(None)
            client.settimeout(None)
            self._track(upstream)
            reverse = threading.Thread(target=self._copy_server_to_client,
                                       args=(upstream, client), daemon=True)
            reverse.start()
            self._copy_client_to_server(client, upstream)
        except OSError:
            pass
        finally:
            self._close_socket(client)
            if upstream is not None:
                self._close_socket(upstream)
            self._untrack(client)
            if upstream is not None:
                self._untrack(upstream)
            if reverse is not None:
                reverse.join(timeout=1)

    def _copy_client_to_server(self, client, upstream):
        protocol_header = _recv_exact(client, AMQP_PROTOCOL_HEADER_SIZE)
        if protocol_header is None:
            return
        upstream.sendall(protocol_header)

        while not self._stopping.is_set():
            header = _recv_exact(client, AMQP_FRAME_HEADER_SIZE)
            if header is None:
                return
            frame_size = int.from_bytes(header[3:7], "big")
            tail = _recv_exact(client, frame_size + 1)
            if tail is None:
                raise ConnectionError("peer closed during an AMQP frame")
            frame = header + tail
            is_basic_ack = (header[0] == 1 and header[1:3] != b"\x00\x00" and
                            frame_size == 13 and tail[:4] == AMQP_BASIC_ACK and
                            tail[-1:] == b"\xce")
            if is_basic_ack and self._armed.is_set():
                with self._lock:
                    hold_ack = not self.ack_held.is_set()
                    if hold_ack:
                        self.ack_held.set()
                if hold_ack:
                    continue
            upstream.sendall(frame)

    def _copy_server_to_client(self, upstream, client):
        try:
            while not self._stopping.is_set():
                data = upstream.recv(65536)
                if not data:
                    return
                client.sendall(data)
        except OSError:
            return
        finally:
            self._close_socket(upstream)
            self._close_socket(client)

    def _track(self, conn):
        with self._lock:
            self._connections.add(conn)

    def _untrack(self, conn):
        with self._lock:
            self._connections.discard(conn)

    @staticmethod
    def _close_socket(conn):
        try:
            conn.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        try:
            conn.close()
        except OSError:
            pass
