#!/usr/bin/env python3
import http.client
import http.server
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import unittest
import uuid


SOURCE = Path(__file__).resolve().parent
IMAGE = os.environ.get(
    "ROUTER_NGINX_IMAGE",
    "nginx@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10",
)
LIMIT = 100 * 1024 * 1024


def run(*arguments, **options):
    return subprocess.run(arguments, text=True, capture_output=True, **options)


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def fixture(directory):
    controller = Path(directory) / "deploy/cd/router.sh"
    controller.parent.joinpath("router").mkdir(parents=True)
    shutil.copyfile(SOURCE / "router.sh", controller)
    shutil.copyfile(SOURCE / "router/nginx.conf.template", controller.parent / "router/nginx.conf.template")
    return controller


def controller_run(controller, state, command, *arguments, **environment):
    return run(
        "bash", str(controller), command, "--state-dir", str(state), *arguments,
        env={**os.environ, "ROUTER_NGINX_IMAGE": IMAGE, "ROUTER_RELOAD_CMD": "true",
             "BACKEND_BLUE_PORT": "18081", "FRONTEND_BLUE_PORT": "13001",
             "BACKEND_GREEN_PORT": "18082", "FRONTEND_GREEN_PORT": "13002", **environment},
    )


class ControllerTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="router-controller-")
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.controller = fixture(self.root)
        self.state = self.root / "state"
        clock = self.root / "bin/date"
        clock.parent.mkdir()
        clock.write_text('#!/bin/sh\ncase "$*" in\n"-u +%Y%m%dT%H%M%SZ") echo 20260101T000000Z ;;\n*) exec /bin/date "$@" ;;\nesac\n')
        clock.chmod(0o755)
        self.environment = {"PATH": str(clock.parent) + os.pathsep + os.environ["PATH"]}

    def invoke(self, command, *arguments, **environment):
        return controller_run(self.controller, self.state, command, *arguments,
                              **{**self.environment, **environment})

    def select(self, colour="blue", **environment):
        result = self.invoke("select", "--colour", colour, **environment)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def pair(self):
        return tuple(self.state.joinpath("active." + suffix).read_bytes() for suffix in ("conf", "json"))

    def test_same_second_same_blue_preserves_retained_bytes_and_rollback(self):
        self.select()
        original_target = self.state.joinpath("active.conf").readlink()
        original = self.pair()
        result = self.invoke("select", "--colour", "blue", BACKEND_BLUE_PORT="18083")
        self.assertEqual(self.state.joinpath(original_target).read_bytes(), original[0],
                         "same-second select overwrote retained configuration")
        if result.returncode != 0:
            self.assertEqual(self.pair(), original)
            return
        self.assertNotEqual(self.state.joinpath("active.conf").readlink(), original_target)
        self.assertEqual(json.loads(self.pair()[1])["backend_port"], 18083)
        result = self.invoke("revert")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.pair(), original, "rollback must restore both original byte streams")

    def test_pruning_preserves_active_and_rollback_targets(self):
        self.select()
        original = self.pair()
        result = self.invoke("select", "--colour", "green", "--retained-count", "1")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        result = self.invoke("revert")
        self.assertEqual(result.returncode, 0, "pruning removed the retained rollback target")
        self.assertEqual(self.pair(), original)

    def test_retained_byte_or_metadata_tampering_fails_before_reload(self):
        for suffix in ("conf", "json"):
            with self.subTest(suffix=suffix):
                self.state = self.root / suffix
                self.select()
                self.select("green")
                before = self.pair()
                retained = self.state / self.state.joinpath("previous.conf.path").read_text().strip()
                retained = retained.with_suffix("." + suffix)
                retained.chmod(0o644)
                retained.write_bytes(retained.read_bytes() + b"\n ")
                result = self.invoke("revert", ROUTER_RELOAD_CMD=f'touch "{self.root}/unexpected-reload"')
                self.assertNotEqual(result.returncode, 0, "revert accepted changed retained bytes")
                self.assertEqual(self.pair(), before)
                self.assertFalse(self.root.joinpath("unexpected-reload").exists())

    def test_revert_detects_bytes_or_metadata_changed_during_reload(self):
        for suffix in ("conf", "json"):
            with self.subTest(suffix=suffix):
                self.state = self.root / ("reload-" + suffix)
                self.select()
                self.select("green")
                target = self.state / ("active." + suffix)
                result = self.invoke("revert", ROUTER_RELOAD_CMD=f'chmod u+w "{target}"; printf "\\n " >> "{target}"')
                self.assertNotEqual(result.returncode, 0, "revert reported success after restored bytes changed")
                self.assertNotIn("router reverted to previous generation", result.stdout)

    def test_missing_or_incoherent_retained_metadata_refuses_selection(self):
        self.select()
        before = self.state.joinpath("active.conf").readlink()
        metadata = self.state / self.state.joinpath("active.json").readlink()
        metadata.chmod(0o644)
        document = json.loads(metadata.read_text())
        document["backend_port"] = 18099
        metadata.write_text(json.dumps(document))
        result = self.invoke("select", "--colour", "green")
        self.assertNotEqual(result.returncode, 0, "select retained mismatched config/metadata")
        self.assertEqual(self.state.joinpath("active.conf").readlink(), before)
        metadata.unlink()
        result = self.invoke("select", "--colour", "green")
        self.assertNotEqual(result.returncode, 0, "select retained missing metadata")

    def test_revert_refuses_target_outside_generation_directory(self):
        self.select()
        self.select("green")
        shutil.copyfile(self.state / "active.conf", self.root / "outside.conf")
        shutil.copyfile(self.state / "active.json", self.root / "outside.json")
        self.state.joinpath("previous.conf.path").write_text("../outside.conf\n")
        result = self.invoke("revert")
        self.assertNotEqual(result.returncode, 0, "revert accepted an unretained target")

    def test_revert_binds_checksums_to_the_recorded_target(self):
        self.select()
        self.select("green")
        self.state.joinpath("previous.conf.path").write_text(str(self.state.joinpath("active.conf").readlink()) + "\n")
        result = self.invoke("revert")
        self.assertNotEqual(result.returncode, 0, "revert accepted a different target with unrelated checksums")


class Backend(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_arguments):
        pass

    def do_GET(self):
        self.do_POST()

    def do_POST(self):
        size = int(self.headers.get("Content-Length", "0"))
        remaining = size
        beginning = b""
        ending = b""
        while remaining:
            chunk = self.rfile.read(min(65536, remaining))
            if not chunk:
                break
            beginning = (beginning + chunk)[:256]
            ending = (ending + chunk)[-256:]
            remaining -= len(chunk)
        self.server.receipts.append({"bytes": size, "request_id": self.headers.get("X-Request-ID")})
        status = 200
        if size > LIMIT or (self.command == "POST" and
                            (not beginning.startswith(b"--fixture\r\n") or not ending.endswith(b"\r\n--fixture--\r\n"))):
            status = 400
        if self.headers.get("Upgrade") == "websocket":
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "upgrade")
            self.end_headers()
            self.close_connection = True
            return
        body = json.dumps({"backend": self.server.label, "bytes": size,
                           "request_id": self.headers.get("X-Request-ID"),
                           "headers": dict(self.headers)}).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("X-Request-ID", "untrusted-backend")
        self.send_header("Set-Cookie", "fixture=ok; HttpOnly")
        self.end_headers()
        self.wfile.write(body)


class UploadTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.directory = tempfile.TemporaryDirectory(prefix="router-upload-")
        cls.addClassCleanup(cls.directory.cleanup)
        cls.root = Path(cls.directory.name)
        cls.controller = fixture(cls.root)
        cls.state = cls.root / "state"
        cls.api_port, cls.web_port = free_port(), free_port()
        cls.backends = []
        for label in ("original", "replacement"):
            backend = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Backend)
            backend.label, backend.receipts = label, []
            thread = threading.Thread(target=backend.serve_forever, daemon=True)
            thread.start()
            cls.backends.append(backend)
            cls.addClassCleanup(backend.server_close)
            cls.addClassCleanup(backend.shutdown)
        template = cls.controller.parent / "router/nginx.conf.template"
        template.write_text(template.read_text().replace("127.0.0.1:8081", f"127.0.0.1:{cls.api_port}")
                            .replace("127.0.0.1:3000", f"127.0.0.1:{cls.web_port}"))
        cls.container = "router-fixture-" + uuid.uuid4().hex
        cls.environment = {"BACKEND_BLUE_PORT": str(cls.backends[0].server_port),
                           "FRONTEND_BLUE_PORT": str(cls.backends[0].server_port)}
        selected = controller_run(cls.controller, cls.state, "select", "--colour", "blue", **cls.environment)
        if selected.returncode:
            raise RuntimeError(selected.stdout + selected.stderr)
        started = run("docker", "run", "--rm", "-d", "--network", "host", "--name", cls.container,
                      "--log-driver", "json-file", "--log-opt", "max-size=1m", "--log-opt", "max-file=2",
                      "-v", f"{cls.state}:/etc/nginx/router-state:ro", IMAGE,
                      "nginx", "-c", "/etc/nginx/router-state/active.conf", "-g", "daemon off;")
        if started.returncode:
            raise RuntimeError(started.stderr)
        cls.addClassCleanup(run, "docker", "stop", "--time", "2", cls.container)
        for attempt in range(60):
            try:
                connection = socket.create_connection(("127.0.0.1", cls.api_port), timeout=1)
                connection.close()
                break
            except OSError:
                if attempt == 59:
                    raise
                time.sleep(0.05)

    def request(self, size=512, *, chunked=False, path="/api/upload-file", port=None,
                malformed=False, headers=None, method="POST", direct=False):
        prefix = b'--fixture\r\nContent-Disposition: form-data; name="file"; filename="private-filename"\r\n\r\nprivate-body'
        suffix = b"\r\n--fixture--\r\n"

        def body():
            if malformed:
                yield b"malformed"
                return
            yield prefix
            remaining = size - len(prefix) - len(suffix)
            while remaining > 0:
                count = min(65536, remaining)
                yield b"x" * count
                remaining -= count
            yield suffix

        request_headers = {"Content-Type": "multipart/form-data; boundary=fixture", **(headers or {})}
        if not chunked:
            request_headers["Content-Length"] = str(9 if malformed else size)
        connection = http.client.HTTPConnection("127.0.0.1", port or self.api_port, timeout=30)
        try:
            if size > LIMIT and not chunked and not direct:
                connection.putrequest(method, path)
                for name, value in request_headers.items():
                    connection.putheader(name, value)
                connection.endheaders()
            else:
                try:
                    connection.request(method, path, body=body() if method == "POST" else None,
                                       headers=request_headers, encode_chunked=chunked)
                except (BrokenPipeError, ConnectionResetError):
                    pass
            response = connection.getresponse()
            return response.status, dict(response.getheaders()), response.read()
        finally:
            connection.close()

    def audit(self):
        logs = run("docker", "logs", self.container)
        self.assertEqual(logs.returncode, 0, logs.stderr)
        text = logs.stdout + logs.stderr
        records = [json.loads(line) for line in text.splitlines() if line.startswith('{"request_id":')]
        return records, text

    def test_declared_and_chunked_whole_request_boundaries(self):
        for chunked in (False, True):
            for size in (512, 1024 * 1024 + 1, LIMIT, LIMIT + 1):
                with self.subTest(chunked=chunked, size=size):
                    count = sum(len(backend.receipts) for backend in self.backends)
                    status, headers, body = self.request(size, chunked=chunked)
                    self.assertEqual(status, 413 if size > LIMIT else 200)
                    self.assertRegex(headers.get("X-Request-ID", ""), r"^[a-f0-9]{32}$")
                    records, _text = self.audit()
                    record = next(record for record in records if record["request_id"] == headers["X-Request-ID"])
                    self.assertEqual(record["status"], status)
                    self.assertEqual(record["endpoint"], "/api/upload-file")
                    self.assertTrue(record["selected_upstream"])
                    if size > LIMIT:
                        self.assertIn(record["upstream_status"], ("", "-"))
                        self.assertIn(record["upstream_addr"], ("", "-"))
                        self.assertEqual(sum(len(backend.receipts) for backend in self.backends), count)
                    else:
                        self.assertEqual(record["upstream_status"], "200")
                        self.assertEqual(json.loads(body)["bytes"], size)
                        self.assertEqual(sum(len(backend.receipts) for backend in self.backends), count + 1)

    def test_malformed_backend_rejection_and_finite_backend_limit(self):
        status, headers, _body = self.request(malformed=True)
        self.assertEqual(status, 400)
        self.assertRegex(headers.get("X-Request-ID", ""), r"^[a-f0-9]{32}$")
        records, _text = self.audit()
        self.assertEqual(next(record for record in records if record["request_id"] == headers["X-Request-ID"])["upstream_status"], "400")
        status, _headers, _body = self.request(LIMIT + 1, port=self.backends[0].server_port, direct=True)
        self.assertEqual(status, 400)

    def test_private_markers_and_caller_ids_are_not_retained(self):
        markers = ("private-query", "private-cookie", "private-authorization", "private-agent",
                   "private-referrer", "private-caller-id", "private-filename", "private-body", "private-person")
        for size in (512, LIMIT + 1):
            status, headers, body = self.request(size, path="/api/upload-file?secret=private-query&user=private-person",
                                                headers={"Cookie": markers[1], "Authorization": markers[2],
                                                         "User-Agent": markers[3], "Referer": markers[4],
                                                         "X-Request-ID": markers[5]})
            self.assertEqual(status, 413 if size > LIMIT else 200)
            self.assertRegex(headers.get("X-Request-ID", ""), r"^[a-f0-9]{32}$")
            if status == 200:
                document = json.loads(body)
                self.assertEqual(document["request_id"], headers["X-Request-ID"])
                self.assertEqual(document["headers"]["Cookie"], markers[1])
                self.assertEqual(document["headers"]["Authorization"], markers[2])
                self.assertEqual(headers["Set-Cookie"], "fixture=ok; HttpOnly")
        records, logs = self.audit()
        self.assertTrue(records, "Docker did not collect sanitized upload audit records")
        for marker in markers:
            self.assertNotIn(marker, logs)
        for record in records:
            self.assertRegex(record["time"], r"^\d{4}-\d\d-\d\dT")
            self.assertGreaterEqual(float(record["request_time"]), 0)
        config = json.loads(run("docker", "inspect", self.container).stdout)[0]["HostConfig"]["LogConfig"]
        self.assertEqual(config, {"Type": "json-file", "Config": {"max-size": "1m", "max-file": "2"}})

    def test_sibling_api_web_caps_and_websocket_forwarding_stay_unchanged(self):
        for path, port in (("/api/issues", self.api_port), ("/api/upload-file/", self.api_port),
                           ("/api/upload-file", self.web_port)):
            self.assertEqual(self.request(2 * 1024 * 1024, path=path, port=port)[0], 413)
        for path in ("/ws", "/api/daemon/ws"):
            status, headers, _body = self.request(0, path=path, method="GET",
                                                 headers={"Upgrade": "websocket", "Connection": "Upgrade"})
            self.assertEqual(status, 101)
            self.assertEqual(headers["Upgrade"], "websocket")

    def test_same_blue_live_selection_and_exact_rollback(self):
        original = tuple(self.state.joinpath("active." + suffix).read_bytes() for suffix in ("conf", "json"))
        environment = {**self.environment, "BACKEND_BLUE_PORT": str(self.backends[1].server_port),
                       "ROUTER_RELOAD_CMD": f"docker exec {self.container} nginx -s reload -c /etc/nginx/router-state/active.conf"}
        result = controller_run(self.controller, self.state, "select", "--colour", "blue", **environment)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.wait_backend("replacement")
        result = controller_run(self.controller, self.state, "revert", **environment)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.wait_backend("original")
        self.assertEqual(tuple(self.state.joinpath("active." + suffix).read_bytes() for suffix in ("conf", "json")), original)

    def wait_backend(self, expected):
        for attempt in range(60):
            status, _headers, body = self.request()
            if status == 200 and json.loads(body)["backend"] == expected:
                return
            time.sleep(0.05)
        self.fail("reloaded router did not select " + expected)


if __name__ == "__main__":
    unittest.main(verbosity=2)
