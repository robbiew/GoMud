#!/usr/bin/env python3
"""Exercise the real engine and door client with an isolated copied world.

Requires built binaries and a localhost TLS certificate/key. No third-party
Python packages are needed. The one-minute expiry check takes about 65 seconds.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import ssl
import struct
import subprocess
import tempfile
import threading
import time


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def recv_exact(conn, count):
    result = bytearray()
    while len(result) < count:
        chunk = conn.recv(count - len(result))
        if not chunk:
            raise RuntimeError("connection ended inside handshake")
        result.extend(chunk)
    return bytes(result)


def handshake(conn, token, alias, minutes=2):
    packet = json.dumps(dict(version=1, bbs_id="smoke-bbs", token=token,
                             alias=alias, node_num=1, time_left=minutes,
                             columns=80, rows=24)).encode()
    conn.sendall(struct.pack("!I", len(packet)) + packet)
    size = struct.unpack("!I", recv_exact(conn, 4))[0]
    assert 0 < size <= 8192, "server emitted game output before acknowledgment"
    return json.loads(recv_exact(conn, size))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-binary", required=True, type=Path)
    parser.add_argument("--client-binary", required=True, type=Path)
    parser.add_argument("--cert-file", required=True, type=Path)
    parser.add_argument("--key-file", required=True, type=Path)
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[1]
    args.server_binary = args.server_binary.resolve()
    args.client_binary = args.client_binary.resolve()
    args.cert_file = args.cert_file.resolve()
    args.key_file = args.key_file.resolve()
    with tempfile.TemporaryDirectory(prefix="gomud-bbs-smoke-") as temp:
        root = Path(temp)
        world = root / "world"
        shutil.copytree(repo / "_datafiles/world/default", world)
        port, telnet_port = free_port(), free_port()
        token = secrets.token_hex(32)
        credentials = root / "credentials.json"
        credentials.write_text(json.dumps({"smoke-bbs": [token]}))
        credentials.chmod(0o600)
        overrides = {
            "FilePaths.DataFiles": str(world), "Network.HttpPort": 0,
            "Network.HttpsPort": 0, "Network.LocalPort": 0,
            "Network.TelnetPort": [str(telnet_port)], "Network.SSHPort": 0,
            "Network.AI.Port": 0, "Network.BBS.Host": "127.0.0.1",
            "Network.BBS.Port": port,
            "Network.BBS.CertificateFile": str(args.cert_file),
            "Network.BBS.KeyFile": str(args.key_file),
            "Network.BBS.CredentialsFile": str(credentials),
            "Server.OnLoginCommands": [],
        }
        config = root / "overrides.yaml"
        config.write_text(json.dumps(overrides))  # JSON is also valid YAML.
        log = root / "server.log"
        server_env = dict(os.environ, CONFIG_PATH=str(config), LOG_PATH=str(log),
                          LOG_LEVEL="M", LOG_NOCOLOR="1")
        with (root / "console.log").open("wb") as console:
            server = subprocess.Popen([str(args.server_binary)], cwd=repo,
                                      env=server_env, stdout=console, stderr=console)
            client = None
            peers = []
            try:
                deadline = time.monotonic() + 30
                while time.monotonic() < deadline:
                    if server.poll() is not None:
                        raise RuntimeError("engine stopped during startup")
                    if log.exists() and "Server Ready" in log.read_text():
                        break
                    time.sleep(0.1)
                else:
                    raise RuntimeError("engine did not become ready")
                context = ssl.create_default_context(cafile=str(args.cert_file))

                def connect():
                    conn = context.wrap_socket(socket.create_connection(("127.0.0.1", port), 5),
                                               server_hostname="localhost")
                    conn.settimeout(5)
                    peers.append(conn)
                    return conn

                denied = connect()
                assert handshake(denied, "wrong-token", "Denied")["ok"] is False
                denied.close()
                with socket.create_connection(("127.0.0.1", telnet_port), 5) as telnet:
                    assert telnet.recv(4096), "standard Telnet listener produced no output"
                node = root / "node"
                node.mkdir()
                (node / "door32.sys").write_text(
                    "2\n0\n57600\nSmoke BBS\n1\nReal Name\nSmokeCaller\n255\n1\n1\n3\n")
                env = dict(os.environ, BBSMUD_TOKEN=token)
                started = time.monotonic()
                client = subprocess.Popen(
                    [str(args.client_binary), "-server", f"127.0.0.1:{port}",
                     "-bbs-id", "smoke-bbs", "-ca-file", str(args.cert_file),
                     "-server-name", "localhost", "-idle-seconds", "0"],
                    cwd=node, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE, bufsize=0)
                output = bytearray()
                ready = threading.Event()
                failures = []

                def read_terminal():
                    try:
                        answered = False
                        while True:
                            chunk = os.read(client.stdout.fileno(), 8192)
                            if not chunk:
                                return
                            output.extend(chunk)
                            if not answered and b"\x1b[c" in output:
                                client.stdin.write(b"\x1b[24;80R\x1b[?1;2c")
                                answered = True
                            if answered and len(output) > 100:
                                ready.set()
                    except Exception as error:
                        failures.append(type(error).__name__)

                reader = threading.Thread(target=read_terminal, daemon=True)
                reader.start()
                assert ready.wait(10), "door did not reach game output"
                client.stdin.write(b"look\r")
                time.sleep(0.2)
                client.stdin.write(b"who\r")
                print("real door reached game; waiting for one-minute expiry", flush=True)
                assert client.wait(timeout=70) == 0, "door did not return status 0"
                reader.join(2)
                assert not reader.is_alive() and not failures, "terminal worker stranded"
                elapsed = time.monotonic() - started
                assert 59 <= elapsed <= 67, f"incorrect session duration: {elapsed:.1f}s"
                assert b"\xff" not in output, "Telnet IAC leaked into BBS output"
                assert b"password:" not in output.lower(), "password login was not bypassed"
                records = [path for path in (world / "users").glob("*.yaml")
                           if "alias: SmokeCaller" in path.read_text()]
                assert len(records) == 1, "BBS character was not saved uniquely"
                record = records[0]
                persisted = record.read_text()
                assert "id: smoke-bbs" in persisted, "external identity not persisted"
                reconnected = connect()
                assert handshake(reconnected, token, "SmokeCaller")["ok"], "saved character could not reconnect"
                reconnected.close()
                time.sleep(0.3)
                records_after = [path for path in (world / "users").glob("*.yaml")
                                 if "alias: SmokeCaller" in path.read_text()]
                assert records_after == records, "reconnect created a different character"
                print(json.dumps(dict(result="passed", expiry_seconds=round(elapsed, 2),
                                      door_exit=client.returncode, output_bytes=len(output),
                                      saved_character=record.name, reconnect="same character",
                                      wrong_token="rejected", standard_telnet="responded")), flush=True)
            except Exception:
                if log.exists():
                    print("Engine log tail:\n" + "\n".join(log.read_text().splitlines()[-25:]))
                raise
            finally:
                for peer in peers:
                    peer.close()
                if client is not None and client.poll() is None:
                    client.kill()
                    client.wait()
                server.send_signal(signal.SIGTERM)
                try:
                    server.wait(15)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait()


if __name__ == "__main__":
    main()
