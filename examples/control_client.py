#!/usr/bin/env python3
"""Finite p3a/1 example: read -> apply -> confirmed ready -> synthetic inference -> stop.

Uses only Python's standard library. LLAMACTL_CONTROL_KEY must already contain an
approved scoped-controller credential. No credential issuance or recovery is done.
An error never triggers automatic rollback, boot replacement or owner resume.
"""

import argparse
import json
import os
import socket
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


class ControlError(RuntimeError):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Never send credentials/ready capability to a redirected origin.
        return None


class Client:
    def __init__(self, base_url, instance, key, http_timeout=15):
        parsed = urllib.parse.urlsplit(base_url)
        if (parsed.scheme not in ("http", "https") or not parsed.hostname
                or parsed.username or parsed.password or parsed.query or parsed.fragment
                or parsed.path not in ("", "/")):
            raise ControlError("base-url must be an explicit HTTP(S) origin without credentials")
        if not key:
            raise ControlError("set LLAMACTL_CONTROL_KEY using your approved credential storage")
        if not instance or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-" for c in instance):
            raise ControlError("instance must be a simple configured instance name")
        self.base_url = base_url.rstrip("/")
        self.instance = instance
        self.root = "/api/v1/instances/" + urllib.parse.quote(instance, safe="")
        self.key = key
        self.http_timeout = http_timeout
        # Direct connection; do not leak credentials via an environment HTTP proxy.
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def open(self, method, path, data=None, token=None):
        headers = {"Authorization": "Bearer " + self.key, "Accept": "application/json"}
        if data is not None:
            headers["Content-Type"] = "application/json"
        if token is not None:
            headers["X-Ready-Token"] = token
        request = urllib.request.Request(self.base_url + path, data=data, headers=headers, method=method)
        try:
            return self.opener.open(request, timeout=self.http_timeout)
        except urllib.error.HTTPError as error:
            # Do not print response bodies, credentials or inference inputs in errors.
            code = "http_error"
            try:
                parsed = json.loads(error.read(65536))
                if isinstance(parsed.get("error"), str):
                    code = parsed["error"]
                elif isinstance(parsed.get("error"), dict):
                    code = parsed["error"].get("type", code)
            except (ValueError, AttributeError):
                pass
            finally:
                error.close()
            raise ControlError(f"HTTP {error.code}: {code}; inspect state, do not blindly replay") from None

    def json_request(self, method, path, data=None):
        with self.open(method, path, data) as response:
            return json.load(response)

    def status(self):
        snapshot = self.json_request("GET", self.root + "/control")
        if snapshot.get("protocol_version") != "p3a/1":
            raise ControlError("unsupported control protocol")
        return snapshot

    def operation(self, snapshot, action, timeout_seconds=None, **extra):
        payload = {"boot_id": snapshot["boot_id"], "expected_revision": snapshot["revision"],
                   "idempotency_key": uuid.uuid4().hex, "action": action, **extra}
        if timeout_seconds is not None:
            payload["timeout_seconds"] = timeout_seconds
        data = json.dumps(payload, separators=(",", ":"), sort_keys=True).encode("utf-8")
        # Both attempts send the SAME bytes/boot/revision/key. No business-error retries.
        budget = timeout_seconds or snapshot["operation_budget_seconds"]
        deadline = time.monotonic() + budget + 10
        for attempt in range(2):
            try:
                op = self.json_request("POST", self.root + "/operations", data)
                break
            except (urllib.error.URLError, TimeoutError, socket.timeout):
                if attempt or time.monotonic() >= deadline:
                    raise ControlError("POST outcome unknown; retain original payload/key for investigation") from None
        while op["state"] == "running":
            if time.monotonic() >= deadline:
                raise ControlError("client wait expired; operation outcome unknown, no automatic replay")
            time.sleep(0.1)
            op = self.json_request("GET", self.root + "/operations/" + urllib.parse.quote(op["id"], safe=""))
        if op["boot_id"] != snapshot["boot_id"] or op["state"] != "succeeded":
            raise ControlError("operation did not succeed: " + str(op.get("error_code", op["state"])))
        return op

    def infer(self, snapshot, model, stream):
        body = {"model": self.instance + "/" + model,
                "messages": [{"role": "user", "content": "Reply with OK."}],
                "max_tokens": 8, "stream": stream}
        with self.open("POST", "/v1/chat/completions", json.dumps(body).encode("utf-8"),
                       snapshot["ready_token"]) as response:
            if stream:
                done = False
                for line in response:
                    if line.strip() == b"data: [DONE]":
                        done = True
                    # Keep standard SSE frames intact, including tools/reasoning/usage.
                    sys.stdout.buffer.write(line)
                sys.stdout.buffer.flush()
                if not done:
                    raise ControlError("SSE ended without [DONE]; inference was interrupted")
            else:
                result = json.load(response)
                if "error" in result or "choices" not in result:
                    raise ControlError("inference returned no successful choices")
                print(json.dumps(result, ensure_ascii=False))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--instance", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--patch", default="{}", help="finite per-model JSON patch (default: {})")
    parser.add_argument("--timeout-seconds", type=int, help="shorten server operation budget")
    parser.add_argument("--http-timeout", type=float, default=15, help="per HTTP exchange seconds")
    parser.add_argument("--stream", action="store_true")
    args = parser.parse_args(argv)
    if args.http_timeout <= 0:
        raise ControlError("http-timeout must be positive")
    patch = json.loads(args.patch)
    if not isinstance(patch, dict):
        raise ControlError("patch must be a JSON object")
    client = Client(args.base_url, args.instance, os.environ.get("LLAMACTL_CONTROL_KEY"), args.http_timeout)
    snapshot = client.status()
    if snapshot["recovery_required"] or snapshot["admission"] != "allowed":
        raise ControlError("administrator/owner action required; example will not recover or resume")
    if snapshot.get("active_operation"):
        raise ControlError("another operation is active")
    if args.model not in snapshot["models"]:
        raise ControlError("model is not in the trusted logical model mapping")
    if args.timeout_seconds is not None and not 1 <= args.timeout_seconds <= snapshot["operation_budget_seconds"]:
        raise ControlError("timeout-seconds can only shorten the server budget")
    op = client.operation(snapshot, "apply", args.timeout_seconds, model=args.model, patch=patch)
    ready = client.status()
    if (ready["boot_id"] != op["boot_id"] or ready["revision"] != op["revision"]
            or ready["state"] != "ready" or ready.get("actual_model") != args.model
            or not ready.get("ready_token") or ready["ready_token"] != op.get("ready_token")):
        raise ControlError("ready generation changed; do not infer or stop another client's generation")
    client.infer(ready, args.model, args.stream)
    current = client.status()
    if any(current.get(k) != ready.get(k) for k in ("boot_id", "revision", "ready_token", "process_generation", "load_generation")):
        raise ControlError("generation changed after inference; no automatic stop")
    client.operation(current, "stop", args.timeout_seconds)
    print("Synthetic request completed; owned stop succeeded (not a VRAM measurement).", file=sys.stderr)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ControlError, ValueError, KeyError, urllib.error.URLError, TimeoutError, OSError) as error:
        print("Control example failed: " + str(error), file=sys.stderr)
        sys.exit(1)
