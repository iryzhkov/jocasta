#!/usr/bin/env python3
"""Operator-only Authentik canary launcher. No token cache or JWT verification."""

import contextlib
import http.client
import json
import os
import re
import secrets
import signal
import ssl
import stat
import sys
import urllib.parse

MAX_RESPONSE = 32768
MAX_TOKEN = 16384
TIMEOUT = 10
CONFIG_KEYS = {
    "expected_issuer", "client_id", "token_url", "client_secret_file",
    "private_dir", "jocasta_binary", "jocasta_url",
}
# Deliberately smaller than the CLI's global flagset. No local output files.
FLAGS = {
    "get": {"id", "revision", "lines", "json"},
    "exists": {"id", "json"},
    "ls": {"prefix", "project", "kind", "include-archived", "limit", "cursor", "json"},
    "search": {"mode", "prefix", "project", "kind", "include-archived", "limit", "cursor", "json"},
    "history": {"id", "limit", "cursor", "json"},
    "diff": {"id", "from", "to", "json"},
    "put": {"id", "file", "if-revision", "create", "json"},
}
BOOL_FLAGS = {"json", "include-archived", "create"}


class CanaryError(Exception):
    """Carries only fixed, public diagnostics."""


def require(condition, message):
    if not condition:
        raise CanaryError(message)


def absolute(path):
    require(isinstance(path, str) and path.startswith("/") and
            path == os.path.normpath(path) and not path.startswith("//"),
            "paths must be absolute and normalized")
    return path


@contextlib.contextmanager
def directory(path):
    """Walk every component with no symlink traversal; retain the final dirfd."""
    absolute(path)
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.split("/")[1:]:
            if part:
                next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                                  dir_fd=fd)
                os.close(fd)
                fd = next_fd
        yield fd
    finally:
        os.close(fd)


def private_regular(info, modes=(0o400, 0o600)):
    require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and
            stat.S_IMODE(info.st_mode) in modes and info.st_nlink == 1,
            "file must be an owner-only regular file with one link")


def read_private(path, limit):
    absolute(path)
    with directory(os.path.dirname(path)) as parent:
        fd = os.open(os.path.basename(path), os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                     dir_fd=parent)
        with os.fdopen(fd, "rb") as source:
            private_regular(os.fstat(source.fileno()))
            data = source.read(limit + 1)
    require(len(data) <= limit, "private file exceeds size limit")
    return data


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON field")
        result[key] = value
    return result


def https_url(value):
    require(isinstance(value, str) and value.isascii() and
            not any(c.isspace() or ord(c) < 32 for c in value) and
            "?" not in value and "#" not in value and "\\" not in value,
            "invalid HTTPS URL")
    url = urllib.parse.urlsplit(value)
    require(url.scheme == "https" and url.hostname and url.username is None and
            url.password is None and url.path.startswith("/"), "invalid HTTPS URL")
    require(url.port is None or 1 <= url.port <= 65535, "invalid HTTPS URL")
    return url


def load_config(path):
    config = json.loads(read_private(path, 16384), object_pairs_hook=unique_object)
    require(isinstance(config, dict) and set(config) == CONFIG_KEYS,
            "invalid helper configuration fields")
    require(all(isinstance(v, str) and 0 < len(v) <= 4096 and
                not any(ord(c) < 32 or ord(c) == 127 for c in v)
                for v in config.values()), "invalid helper configuration values")
    issuer = https_url(config["expected_issuer"])
    token = https_url(config["token_url"])
    require(issuer.netloc == token.netloc, "issuer and token origins must match")
    server = urllib.parse.urlsplit(config["jocasta_url"])
    require(config["jocasta_url"] in (
        "http://127.0.0.1:" + str(server.port),
        "http://[::1]:" + str(server.port),
    ) and server.port is not None and 1 <= server.port <= 65535,
            "Jocasta URL must be a literal loopback HTTP origin with an explicit port")
    for name in ("client_secret_file", "private_dir", "jocasta_binary"):
        absolute(config[name])
    outputs = {config["private_dir"] + "/" + name
               for name in ("read-token", "read-client.json")}
    require(not outputs.intersection({absolute(path), config["client_secret_file"],
                                      config["jocasta_binary"]}),
            "output paths must not replace inputs")
    return config


def cli_args(args):
    require(args and args[0] in FLAGS, "unsupported canary command")
    verb = args[0]
    allowed = FLAGS[verb]
    require(len(args) <= 64 and all(0 < len(a) <= 8192 and
            not any(ord(c) < 32 or ord(c) == 127 for c in a) for a in args),
            "invalid canary arguments")
    seen = set()
    i = 1
    while i < len(args):
        arg = args[i]
        if arg.startswith("-"):
            name, sep, value = arg[2:].partition("=")
            require(arg.startswith("--") and name in allowed and name not in seen,
                    "unsupported or duplicate canary option")
            seen.add(name)
            if name in BOOL_FLAGS:
                require(not sep, "boolean canary flags take no value")
            else:
                if not sep:
                    i += 1
                    require(i < len(args), "missing canary option value")
                    value = args[i]
                require(value and (not value.startswith("-") or
                        (name == "file" and value == "-")),
                        "invalid canary option value")
                if name == "file":
                    require(value == "-", "put accepts stdin only")
        i += 1
    if verb == "put":
        require("file" in seen, "put requires --file -")
    return list(args)


def alarm_expired(_signum, _frame):
    raise CanaryError("token request timed out")


def post_token(url, form):
    """Direct HTTPS only: no environment proxies, redirects, retry or discovery."""
    endpoint = https_url(url)
    context = ssl.create_default_context()
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    assert endpoint.hostname is not None  # https_url already requires a host.
    connection = http.client.HTTPSConnection(endpoint.hostname, endpoint.port,
                                             timeout=TIMEOUT, context=context)
    # Socket timeout alone does not bound a peer slowly feeding headers/body.
    previous = signal.signal(signal.SIGALRM, alarm_expired)
    previous_timer = signal.setitimer(signal.ITIMER_REAL, TIMEOUT)
    try:
        connection.request("POST", endpoint.path, body=form, headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "Accept": "application/json",
        })
        response = connection.getresponse()
        require(response.status == 200, "token endpoint refused request")
        data = response.read(MAX_RESPONSE + 1)
        require(len(data) <= MAX_RESPONSE, "token response exceeds size limit")
        return data
    finally:
        connection.close()
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)
        signal.setitimer(signal.ITIMER_REAL, *previous_timer)


def acquire(config):
    # Neither exceptions nor response bodies escape this boundary.
    try:
        client_secret = read_private(config["client_secret_file"], 4096).decode("utf-8")
        if client_secret.endswith("\n"):
            client_secret = client_secret[:-1]
        require(client_secret and not any(ord(c) < 32 or ord(c) == 127 for c in client_secret),
                "invalid client secret file")
        form = urllib.parse.urlencode({
            "grant_type": "client_credentials", "client_id": config["client_id"],
            "client_secret": client_secret, "scope": "jocasta.read",
        }).encode("utf-8")
        data = post_token(config["token_url"], form)
        require(len(data) <= MAX_RESPONSE, "token response exceeds size limit")
        result = json.loads(data, object_pairs_hook=unique_object)
        require(isinstance(result, dict), "invalid token response")
        token = result.get("access_token")
        require(isinstance(token, str) and 0 < len(token) <= MAX_TOKEN and
                re.fullmatch(r"[A-Za-z0-9._~+/-]+=*", token), "invalid access token")
        require(result.get("token_type") == "Bearer" and
                type(result.get("expires_in")) is int and
                0 < result["expires_in"] <= 300 and
                result.get("scope") == "jocasta.read", "invalid token metadata")
        return token
    except Exception:
        raise CanaryError("access token acquisition failed") from None


def check_output(fd, name):
    try:
        info = os.stat(name, dir_fd=fd, follow_symlinks=False)
    except FileNotFoundError:
        return
    private_regular(info, (0o600,))


def atomic_private(fd, name, data):
    check_output(fd, name)
    temp = ".canary-" + secrets.token_hex(16)
    out = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                  0o600, dir_fd=fd)
    try:
        with os.fdopen(out, "wb") as target:
            os.fchmod(target.fileno(), 0o600)
            target.write(data)
            target.flush()
            os.fsync(target.fileno())
        check_output(fd, name)
        os.replace(temp, name, src_dir_fd=fd, dst_dir_fd=fd)
        os.fsync(fd)
    finally:
        try:
            os.unlink(temp, dir_fd=fd)
        except FileNotFoundError:
            pass


def launch(config_path, args):
    args = cli_args(args)
    config = load_config(config_path)
    binary = config["jocasta_binary"]
    with directory(os.path.dirname(binary)) as parent:
        fd = os.open(os.path.basename(binary), os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                     dir_fd=parent)
        try:
            info = os.fstat(fd)
            require(stat.S_ISREG(info.st_mode) and info.st_mode & 0o111 and
                    not info.st_mode & 0o022, "unsafe Jocasta binary")
        finally:
            os.close(fd)
    private_dir = config["private_dir"]
    with directory(private_dir) as fd:
        info = os.fstat(fd)
        require(info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o700,
                "canary directory must be owned by this user at mode 0700")
        for name in ("read-token", "read-client.json"):
            check_output(fd, name)
        token = acquire(config)  # Failure never reaches exec, even if an old file exists.
        atomic_private(fd, "read-token", token.encode("ascii"))
        client = {
            "url": config["jocasta_url"], "allow_loopback_http": True,
            "token_file": private_dir + "/read-token",
        }
        atomic_private(fd, "read-client.json", json.dumps(client).encode("utf-8"))
    # Only public paths/settings enter argv/environment. No inherited proxy or auth env.
    os.execve(binary, [binary, "--config", private_dir + "/read-client.json"] + args,
              {"HOME": private_dir, "TMPDIR": private_dir, "LANG": "C.UTF-8"})


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    try:
        require(len(argv) >= 2, "usage: authentik_canary_client.py CONFIG_PATH VERB [ARGS]")
        launch(argv[0], argv[1:])
    except Exception:
        # Do not include exception text, paths, HTTP errors or response bodies.
        print("authentik canary: request refused or failed", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
