#!/usr/bin/env python3
import argparse
import concurrent.futures
import json
import random
import statistics
import threading
import time
import urllib.error
import urllib.request
from collections import Counter


def percentile(values, p):
    if not values:
        return None
    ordered = sorted(values)
    return round(ordered[min(len(ordered) - 1, int((len(ordered) - 1) * p))], 2)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base", required=True)
    parser.add_argument("--duration", type=float, default=90)
    parser.add_argument("--workers", type=int, default=8)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    base = args.base.rstrip("/")
    lock = threading.Lock()
    records = []
    users = [f"neighbor-{i:04d}" for i in range(1, 241)]

    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(op, method, path, payload=None):
        body = json.dumps(payload).encode() if payload is not None else None
        headers = {"Content-Type": "application/json"} if body else {}
        req = urllib.request.Request(base + path, data=body, headers=headers, method=method)
        started = time.monotonic()
        status, error = 0, None
        try:
            with opener.open(req, timeout=5) as response:
                response.read()
                status = response.status
        except urllib.error.HTTPError as exc:
            status = exc.code
            exc.read()
        except Exception as exc:
            error = type(exc).__name__ + ": " + str(exc)
        latency = (time.monotonic() - started) * 1000
        with lock:
            records.append({"op": op, "status": status, "latencyMs": latency, "error": error})

    with concurrent.futures.ThreadPoolExecutor(max_workers=16) as executor:
        list(executor.map(lambda user: request("account", "POST", "/accounts", {"id": user}), users))
    records.clear()
    stop = time.monotonic() + args.duration

    def worker(worker_id):
        rng = random.Random(918273 + worker_id)
        while time.monotonic() < stop:
            user = rng.choice(users)
            roll = rng.random()
            if roll < 0.12:
                request("availability", "GET", "/availability")
            elif roll < 0.56:
                request("claim", "POST", "/claims/" + user)
            elif roll < 0.73:
                request("cancel", "DELETE", "/claims/" + user)
            elif roll < 0.88:
                request("confirm", "POST", "/claims/" + user + "/confirm")
            elif roll < 0.96:
                request("proof", "GET", "/proof")
            else:
                request("account_retry", "POST", "/accounts", {"id": user})
            time.sleep(rng.uniform(0.04, 0.14))

    started_wall = time.time()
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as executor:
        list(executor.map(worker, range(args.workers)))
    finished_wall = time.time()
    statuses = Counter(str(r["status"]) for r in records if r["status"])
    operations = Counter(r["op"] for r in records)
    business = sum(r["status"] in (404, 409) and r["op"] in ("claim", "cancel", "confirm") for r in records)
    server_errors = sum(r["status"] >= 500 for r in records)
    transport = sum(r["error"] is not None for r in records)
    latencies = [r["latencyMs"] for r in records]
    result = {
        "base": base,
        "startedEpoch": started_wall,
        "finishedEpoch": finished_wall,
        "durationSeconds": round(finished_wall - started_wall, 3),
        "requestCount": len(records),
        "statuses": dict(sorted(statuses.items())),
        "operationMix": dict(sorted(operations.items())),
        "businessConflicts": business,
        "transportErrors": transport,
        "serverErrors": server_errors,
        "failureCount": transport + server_errors,
        "latencyMs": {"p50": percentile(latencies, .50), "p95": percentile(latencies, .95), "p99": percentile(latencies, .99)},
        "transportErrorSamples": [r["error"] for r in records if r["error"]][:10],
    }
    with open(args.output, "w", encoding="utf-8") as handle:
        json.dump(result, handle, indent=2, sort_keys=True)
    print(json.dumps(result, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
