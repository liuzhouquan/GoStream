import queue
import socket
import struct
import threading
import unittest

from rabbitmq_ack_proxy import AckGateProxy


def read_exact(sock, size):
    chunks = bytearray()
    while len(chunks) < size:
        chunk = sock.recv(size - len(chunks))
        if not chunk:
            raise EOFError("peer closed")
        chunks.extend(chunk)
    return bytes(chunks)


def read_frame(sock):
    header = read_exact(sock, 7)
    size = int.from_bytes(header[3:7], "big")
    return header + read_exact(sock, size + 1)


def method_frame(class_id, method_id, channel=1, suffix=b""):
    payload = struct.pack(">HH", class_id, method_id) + suffix
    return b"\x01" + channel.to_bytes(2, "big") + len(payload).to_bytes(4, "big") + payload + b"\xce"


class AckGateProxyTest(unittest.TestCase):
    def setUp(self):
        self.broker = socket.socket()
        self.broker.bind(("127.0.0.1", 0))
        self.broker.listen()
        self.frames = queue.Queue()
        self.accepted = threading.Event()
        self.server = threading.Thread(target=self._serve_broker, daemon=True)
        self.server.start()
        self.proxy = AckGateProxy("127.0.0.1", self.broker.getsockname()[1])
        self.client = socket.create_connection(self.proxy.address, timeout=2)
        self.assertTrue(self.accepted.wait(2), "proxy did not connect to broker")

    def tearDown(self):
        self.client.close()
        self.proxy.close()
        self.broker.close()
        self.server.join(timeout=2)

    def _serve_broker(self):
        try:
            conn, _ = self.broker.accept()
            with conn:
                self.accepted.set()
                self.frames.put(read_exact(conn, 8))
                while True:
                    self.frames.put(read_frame(conn))
        except (EOFError, OSError):
            return

    def forwarded(self, expected):
        self.assertEqual(self.frames.get(timeout=2), expected)

    def test_holds_basic_ack_but_forwards_following_frames(self):
        protocol_header = b"AMQP\x00\x00\x09\x01"
        self.client.sendall(protocol_header)
        self.forwarded(protocol_header)

        channel_open = method_frame(20, 10)
        self.client.sendall(channel_open)
        self.forwarded(channel_open)

        self.proxy.arm()
        reject = method_frame(60, 90, suffix=struct.pack(">QB", 1, 0))
        self.client.sendall(reject)
        self.forwarded(reject)

        malformed_ack = method_frame(60, 80)
        self.client.sendall(malformed_ack)
        self.forwarded(malformed_ack)

        ack = method_frame(60, 80, suffix=struct.pack(">QB", 1, 0))
        self.client.sendall(ack)
        self.assertTrue(self.proxy.ack_held.wait(2), "basic.ack was not held")

        heartbeat = b"\x08\x00\x00\x00\x00\x00\x00\xce"
        self.client.sendall(heartbeat)
        self.forwarded(heartbeat)
        with self.assertRaises(queue.Empty):
            self.frames.get(timeout=0.1)


if __name__ == "__main__":
    unittest.main()
