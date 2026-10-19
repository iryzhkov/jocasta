"""Synthetic only. Run with the asserted scratch HOME described in the canary doc."""

import contextlib
import io
import json
import os
from pathlib import Path
import ssl
import stat
import tempfile
import unittest
from unittest import mock
import urllib.parse

import authentik_canary_client as helper


CLIENT_SECRET = "synthetic-provider-secret&+="
TOKEN = "synthetic-access-token"


def response(**changes):
    result = {"access_token": TOKEN, "token_type": "Bearer",
              "expires_in": 300, "scope": "jocasta.read"}
    result.update(changes)
    return json.dumps(result).encode()


class CanaryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        expected = str(Path.cwd() / ".cache/authentik-helper-home")
        if os.environ.get("HOME") != expected or os.environ.get("TMPDIR") != expected + "/tmp":
            raise RuntimeError("tests require the documented scratch HOME and TMPDIR")
        if not Path(expected).is_dir() or Path(expected).is_symlink():
            raise RuntimeError("scratch HOME must already exist without a symlink")

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=os.environ["HOME"])
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.private = self.root / "private"
        self.private.mkdir(mode=0o700)
        self.client_secret = self.root / "client-secret"
        self.client_secret.write_text(CLIENT_SECRET + "\n")
        self.client_secret.chmod(0o600)
        self.binary = self.root / "reviewed-jocasta"
        self.binary.write_text("synthetic binary placeholder")
        self.binary.chmod(0o700)
        self.path = self.root / "helper.json"
        self.config = {
            "expected_issuer": "https://auth.example/application/o/jocasta-canary/",
            "client_id": "jocasta-canary",
            "token_url": "https://auth.example/application/o/token/",
            "client_secret_file": str(self.client_secret),
            "private_dir": str(self.private),
            "jocasta_binary": str(self.binary),
            "jocasta_url": "http://127.0.0.1:18081",
        }
        self.save_config()

    def save_config(self):
        self.path.write_text(json.dumps(self.config))
        self.path.chmod(0o600)

    def launch(self, args=None):
        helper.launch(str(self.path), args or ["get", "canaryA/allowed"])

    def test_exact_form_fresh_issuance_and_fixed_exec(self):
        with mock.patch.object(helper, "post_token", side_effect=[
                response(), response(access_token="second-token")]) as transport, \
                mock.patch.object(helper.os, "execve") as execute:
            self.launch()
            self.launch(["exists", "canaryB/denied", "--json"])
        self.assertEqual(transport.call_count, 2)
        for call in transport.call_args_list:
            url, form = call.args
            self.assertEqual(url, self.config["token_url"])
            self.assertEqual(urllib.parse.parse_qs(form.decode(), strict_parsing=True), {
                "grant_type": ["client_credentials"], "client_id": ["jocasta-canary"],
                "client_secret": [CLIENT_SECRET],
                "scope": ["jocasta.read"],
            })
        self.assertEqual((self.private / "read-token").read_text(), "second-token")
        client_path = self.private / "read-client.json"
        self.assertEqual(json.loads(client_path.read_text()), {
            "url": self.config["jocasta_url"], "allow_loopback_http": True,
            "token_file": str(self.private / "read-token"),
        })
        for name in ("read-token", "read-client.json"):
            self.assertEqual(stat.S_IMODE((self.private / name).stat().st_mode), 0o600)
        binary, argv, env = execute.call_args.args
        self.assertEqual(binary, str(self.binary))
        self.assertEqual(argv, [str(self.binary), "--config", str(client_path),
                                "exists", "canaryB/denied", "--json"])
        self.assertEqual(env, {"HOME": str(self.private), "TMPDIR": str(self.private),
                               "LANG": "C.UTF-8"})
        self.assertNotIn(CLIENT_SECRET, repr(execute.call_args))
        self.assertNotIn(TOKEN, repr(execute.call_args))

    def test_failure_never_launches_with_stale_token_and_redacts(self):
        stale = self.private / "read-token"
        stale.write_text("old-token")
        stale.chmod(0o600)
        for error in (OSError(CLIENT_SECRET + TOKEN), TimeoutError(CLIENT_SECRET)):
            with self.subTest(error=type(error).__name__), \
                    mock.patch.object(helper, "post_token", side_effect=error), \
                    mock.patch.object(helper.os, "execve") as execute:
                with self.assertRaises(helper.CanaryError) as caught:
                    self.launch()
                self.assertEqual(str(caught.exception), "access token acquisition failed")
                execute.assert_not_called()
        self.assertEqual(stale.read_text(), "old-token")
        out, err = io.StringIO(), io.StringIO()
        with mock.patch.object(helper, "post_token", return_value=CLIENT_SECRET.encode()), \
                mock.patch.object(helper.os, "execve") as execute, \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            self.assertEqual(helper.main([str(self.path), "get", "canaryA/allowed"]), 1)
        self.assertEqual(out.getvalue(), "")
        self.assertEqual(err.getvalue(), "authentik canary: request refused or failed\n")
        execute.assert_not_called()

    def test_metadata_and_size_fail_closed(self):
        invalid = [
            response(access_token=""), response(access_token="x" * (helper.MAX_TOKEN + 1)),
            response(access_token="has\nnewline"), response(access_token="has space"),
            response(token_type="Basic"), response(token_type=None),
            response(expires_in=True), response(expires_in="300"), response(expires_in=0),
            response(expires_in=-1), response(expires_in=301), response(expires_in=1.5),
            response(scope="jocasta.read other"), response(scope=None),
            b"[]", b"{bad json", b"x" * (helper.MAX_RESPONSE + 1),
            b'{"access_token":"one","access_token":"two"}',
            json.dumps({"id_token": TOKEN, "refresh_token": TOKEN}).encode(),
        ]
        for data in invalid:
            with self.subTest(data_length=len(data)), \
                    mock.patch.object(helper, "post_token", return_value=data), \
                    mock.patch.object(helper.os, "execve") as execute:
                with self.assertRaises(helper.CanaryError):
                    self.launch()
                execute.assert_not_called()
        # ID/refresh fields never substitute for the access token or trigger another request.
        with mock.patch.object(helper, "post_token",
                               return_value=response(id_token="ignored", refresh_token="ignored")) as post:
            self.assertEqual(helper.acquire(self.config), TOKEN)
            post.assert_called_once()

    def test_https_transport_no_redirect_proxy_or_error_body(self):
        for status in (200, 301, 302, 303, 307, 308, 400):
            with self.subTest(status=status), \
                    mock.patch.dict(os.environ, {"HTTPS_PROXY": "http://proxy.invalid"}), \
                    mock.patch.object(helper.http.client, "HTTPSConnection") as factory:
                connection = factory.return_value
                reply = connection.getresponse.return_value
                reply.status = status
                reply.read.return_value = response()
                if status == 200:
                    self.assertEqual(helper.post_token(self.config["token_url"], b"synthetic"), response())
                    reply.read.assert_called_once_with(helper.MAX_RESPONSE + 1)
                else:
                    with self.assertRaises(helper.CanaryError):
                        helper.post_token(self.config["token_url"], b"synthetic")
                    reply.read.assert_not_called()
                factory.assert_called_once()
                self.assertEqual(factory.call_args.args, ("auth.example", None))
                tls = factory.call_args.kwargs["context"]
                self.assertEqual(tls.verify_mode, ssl.CERT_REQUIRED)
                self.assertTrue(tls.check_hostname)
                self.assertEqual(factory.call_args.kwargs["timeout"], helper.TIMEOUT)
                connection.request.assert_called_once_with(
                    "POST", "/application/o/token/", body=b"synthetic", headers={
                        "Content-Type": "application/x-www-form-urlencoded",
                        "Accept": "application/json",
                    })
                connection.close.assert_called_once()
        with mock.patch.object(helper.http.client, "HTTPSConnection") as factory:
            factory.return_value.getresponse.return_value.status = 200
            factory.return_value.getresponse.return_value.read.return_value = b"x" * (helper.MAX_RESPONSE + 1)
            with self.assertRaises(helper.CanaryError):
                helper.post_token(self.config["token_url"], b"synthetic")
        with self.assertRaises(helper.CanaryError):
            helper.alarm_expired(None, None)

    def test_config_urls_fields_and_input_output_binding(self):
        cases = [
            ("token_url", "http://auth.example/application/o/token/"),
            ("token_url", "https://user:password@auth.example/application/o/token/"),
            ("token_url", "https://auth.example/application/o/token/?q=1"),
            ("token_url", "https://auth.example/application/o/token/?"),
            ("token_url", "https://auth.example/application/o/token/#fragment"),
            ("token_url", "https://elsewhere.example/application/o/token/"),
            ("expected_issuer", "http://auth.example/application/o/jocasta-canary/"),
            ("jocasta_url", "http://localhost:18081"),
            ("jocasta_url", "http://example.com:18081"),
            ("jocasta_url", "http://127.0.0.1:18081/other"),
            ("jocasta_url", "http://127.0.0.1:18081?query"),
            ("jocasta_url", "http://u:p@127.0.0.1:18081"),
            ("client_secret_file", str(self.private / "read-token")),
            ("password", "literal-password-is-forbidden"),
            ("client_secret", "forbidden"),
        ]
        original = self.config.copy()
        for key, value in cases:
            with self.subTest(key=key, value=value):
                self.config = dict(original, **{key: value})
                self.save_config()
                with mock.patch.object(helper, "post_token") as post, \
                        mock.patch.object(helper.os, "execve") as execute:
                    with self.assertRaises((helper.CanaryError, ValueError)):
                        self.launch()
                    post.assert_not_called()
                    execute.assert_not_called()

    def test_private_input_modes_types_symlinks_and_parent_symlink(self):
        for target in (self.path, self.client_secret):
            for mode in (0o644, 0o660, 0o700):
                with self.subTest(target=target.name, mode=mode):
                    target.chmod(mode)
                    with self.assertRaises(helper.CanaryError):
                        helper.read_private(str(target), 16384)
            target.chmod(0o600)
            link = self.root / (target.name + "-link")
            link.symlink_to(target)
            with self.assertRaises(OSError):
                helper.read_private(str(link), 16384)
            link.unlink()
            os.link(target, link)
            with self.assertRaises(helper.CanaryError):
                helper.read_private(str(target), 16384)
            link.unlink()
        link = self.root / "parent-link"
        link.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(OSError):
            helper.read_private(str(link / "client-secret"), 4096)
        fifo = self.root / "fifo"
        os.mkfifo(fifo, mode=0o600)
        with self.assertRaises(helper.CanaryError):
            helper.read_private(str(fifo), 4096)
        self.assertEqual(helper.read_private(str(self.client_secret), 4096), (CLIENT_SECRET + "\n").encode())
        with mock.patch.object(helper.os, "getuid", return_value=os.getuid() + 1):
            with self.assertRaises(helper.CanaryError):
                helper.read_private(str(self.path), 16384)

    def test_directory_output_and_binary_safety(self):
        for mode in (0o755, 0o750):
            self.private.chmod(mode)
            with self.assertRaises(helper.CanaryError):
                self.launch()
        self.private.chmod(0o700)
        for name in ("read-token", "read-client.json"):
            target = self.private / name
            for kind in ("symlink", "directory", "public", "hardlink"):
                with self.subTest(name=name, kind=kind):
                    if kind == "symlink":
                        target.symlink_to(self.client_secret)
                    elif kind == "directory":
                        target.mkdir()
                    elif kind == "hardlink":
                        os.link(self.client_secret, target)
                    else:
                        target.write_text("unsafe")
                        target.chmod(0o644)
                    with mock.patch.object(helper, "post_token") as post, \
                            mock.patch.object(helper.os, "execve") as execute:
                        with self.assertRaises(helper.CanaryError):
                            self.launch()
                        post.assert_not_called()
                        execute.assert_not_called()
                    target.rmdir() if kind == "directory" else target.unlink()
        binary_link = self.root / "binary-link"
        binary_link.symlink_to(self.binary)
        self.config["jocasta_binary"] = str(binary_link)
        self.save_config()
        with self.assertRaises(OSError):
            self.launch()
        self.assertEqual(self.client_secret.read_text(), CLIENT_SECRET + "\n")

    def test_atomic_replace_failure_cleans_temp_and_preserves_existing(self):
        target = self.private / "read-token"
        target.write_text("old-token")
        target.chmod(0o600)
        with helper.directory(str(self.private)) as fd, \
                mock.patch.object(helper.os, "replace", side_effect=OSError("synthetic")):
            with self.assertRaises(OSError):
                helper.atomic_private(fd, "read-token", b"new-token")
        self.assertEqual(target.read_text(), "old-token")
        self.assertEqual(sorted(p.name for p in self.private.iterdir()), ["read-token"])

    def test_modified_client_config_is_replaced_with_fixed_values(self):
        client = self.private / "read-client.json"
        client.write_text(json.dumps({"url": "https://elsewhere.invalid",
                                      "token_file": str(self.client_secret)}))
        client.chmod(0o600)
        with mock.patch.object(helper, "post_token", return_value=response()), \
                mock.patch.object(helper.os, "execve"):
            self.launch()
        self.assertEqual(json.loads(client.read_text())["url"], self.config["jocasta_url"])
        self.assertEqual(json.loads(client.read_text())["token_file"], str(self.private / "read-token"))

    def test_argument_allowlist(self):
        accepted = [
            ["get", "canaryA/allowed", "--lines", "1:2", "--json"],
            ["exists", "--id=fixture", "--json"],
            ["ls", "--include-archived", "--limit", "10"],
            ["search", "marker", "--mode", "literal", "--cursor=synthetic"],
            ["history", "canaryA/allowed"], ["diff", "--id", "fixture", "--from", "1", "--to", "2"],
            ["put", "canaryA/allowed", "--if-revision", "1", "--file", "-"],
            ["put", "canaryA/new", "--create", "--file=-", "--json"],
        ]
        for args in accepted:
            self.assertEqual(helper.cli_args(args), args)
        rejected = [
            [], ["sh", "-c", "anything"], ["get", "x", "--config", "other"],
            ["get", "x", "-config=other"], ["get", "x", "--config=other"],
            ["get", "x", "--url", "https://elsewhere.invalid"],
            ["get", "x", "--token-file", "other"], ["get", "x", "--output", str(self.client_secret)],
            ["get", "x", "--overwrite"], ["get", "x", "--", "--config=other"],
            ["get", "x", "--id", "--config=other"], ["get", "x", "--id=--config=other"],
            ["get", "x", "--json=false"], ["get", "x", "--json", "--json"],
            ["put", "x", "--create", "--file", str(self.client_secret)],
            ["put", "x", "--create"], ["import", "--dry-run"], ["version"], ["list"],
        ]
        for args in rejected:
            with self.subTest(args=args), mock.patch.object(helper, "post_token") as post, \
                    mock.patch.object(helper.os, "execve") as execute:
                with self.assertRaises(helper.CanaryError):
                    self.launch(args) if args else helper.launch(str(self.path), [])
                post.assert_not_called()
                execute.assert_not_called()


if __name__ == "__main__":
    unittest.main()
