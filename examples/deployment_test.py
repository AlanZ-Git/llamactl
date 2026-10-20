#!/usr/bin/env python3
"""One finite deployment test through the existing authenticated instance API.

Only a registered logical model, finite patch and server-owned synthetic fixture
are sent. No inference retry, owner resume, recovery or configuration restoration.
The report goes to stdout; retain the printed submit envelope if the POST response
is lost. Never replace its boot/key automatically after a restart.
"""

import argparse
import json
import os
import sys
import time
import urllib.parse
import uuid

from control_client import Client, ControlError


def unique_object(pairs):
    result = {}
    for name, value in pairs:
        if name in result:
            raise ControlError("duplicate JSON field")
        result[name] = value
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--instance", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--patch", default="{}")
    parser.add_argument("--measured-generations", type=int, choices=(1, 2, 3), default=3)
    parser.add_argument("--warmup", action="store_true")
    parser.add_argument("--timeout-seconds", type=int, default=600)
    parser.add_argument("--format", choices=("json", "csv"), default="json")
    args = parser.parse_args(argv)
    if not 1 <= args.timeout_seconds <= 600:
        raise ControlError("timeout-seconds must be 1..600")
    patch = json.loads(args.patch, object_pairs_hook=unique_object)
    if not isinstance(patch, dict):
        raise ControlError("patch must be an object")
    client = Client(args.base_url, args.instance, os.environ.get("LLAMACTL_CONTROL_KEY"))
    snapshot = client.status()
    if snapshot["recovery_required"] or snapshot["admission"] != "allowed":
        raise ControlError("administrator/owner action required; no recovery or resume attempted")
    if args.model not in snapshot["models"]:
        raise ControlError("model is not registered")
    if any(name not in snapshot["parameters"] for name in patch):
        raise ControlError("unsupported patch field")
    for name, value in patch.items():
        spec = snapshot["parameters"][name]
        valid = value is None and spec["nullable"]
        if spec["type"] == "integer" and type(value) is int:
            valid = spec["minimum"] <= value <= spec["maximum"]
        elif spec["type"] == "boolean" and type(value) is bool:
            valid = True
        elif spec["type"] == "string" and isinstance(value, str):
            valid = value in spec["enum"]
        if not valid:
            raise ControlError("invalid patch value")
    payload = {
        "boot_id": snapshot["boot_id"], "expected_revision": snapshot["revision"],
        "idempotency_key": uuid.uuid4().hex, "model": args.model,
        "fixture_id": "synthetic-text-v1", "patch": patch,
        "measured_generations": args.measured_generations, "warmup": args.warmup,
        "max_output_tokens": 256, "timeout_seconds": args.timeout_seconds,
        "concurrency": 1, "seed": 0,
    }
    # This envelope has no credential, filesystem path, prompt or reply. Keep it
    # unchanged to explicitly retrieve the same run after an uncertain POST.
    data = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    print("Retain original submit envelope: " + data.decode("utf-8"), file=sys.stderr)
    report = client.json_request("POST", client.root + "/tests", data)
    print("Accepted test " + report["id"] + "; not yet proof of success", file=sys.stderr)
    path = client.root + "/tests/" + urllib.parse.quote(report["id"], safe="")
    wait_until = time.monotonic() + args.timeout_seconds + 10
    while report["ended_at"] is None:
        if time.monotonic() >= wait_until:
            raise ControlError("wait expired; outcome unknown; no replay or automatic cancellation")
        time.sleep(0.2)
        report = client.json_request("GET", path)
    if report["boot_id"] != payload["boot_id"]:
        raise ControlError("epoch changed; report cannot resume the previous run")
    if args.format == "csv":
        with client.open("GET", path + "?format=csv") as response:
            sys.stdout.write(response.read().decode("utf-8"))
    else:
        print(json.dumps(report, ensure_ascii=False, indent=2))
    if report["state"] != "succeeded":
        print("Test did not succeed: " + report.get("error_code", report["state"]), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ControlError, ValueError, KeyError, OSError):
        # Network exception strings can contain local addresses; do not log them.
        print("Deployment example failed or outcome unknown; inspect current status. No automatic replay.", file=sys.stderr)
        sys.exit(1)
