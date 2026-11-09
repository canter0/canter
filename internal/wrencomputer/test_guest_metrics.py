"""Fixture test for the lead-authored resource_metrics() in guest.py."""
import os, sys, tempfile, types
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).parent))
import importlib.util
spec = importlib.util.spec_from_file_location("guest", str(Path(__file__).parent / "guest.py"))
guest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guest)


def fake_statvfs(*args, **kwargs):
    return types.SimpleNamespace(f_blocks=1000, f_bfree=300, f_bavail=200, f_frsize=4096)


def test_metrics():
    with tempfile.TemporaryDirectory() as proc:
        Path(proc, "meminfo").write_text("MemTotal: 1000 kB\nMemAvailable: 400 kB\n")
        Path(proc, "loadavg").write_text("1.25 2.50 3.75 1/100 12345\n")
        Path(proc, "uptime").write_text("12.50 40.0\n")
        patches = [mock.patch.dict(os.environ, {"CANTER_COMPUTER_PROC": proc}),
                   mock.patch("os.statvfs", fake_statvfs)]
        if hasattr(os, "sched_getaffinity"):
            patches.append(mock.patch("os.sched_getaffinity", lambda pid: {0, 1}))
            cpus = 2
        else:
            patches.append(mock.patch("os.cpu_count", lambda: 2))
            cpus = 2
        with patches[0], patches[1], patches[2]:
            m = guest.resource_metrics()
    assert m["memory"] == {"totalBytes": 1024000, "availableBytes": 409600, "usedBytes": 614400}
    assert m["disk"] == {"totalBytes": 4096000, "usedBytes": 2867200, "availableBytes": 819200}
    assert m["cpu"] == {"count": cpus, "load1": 1.25, "load5": 2.5, "load15": 3.75}
    assert m["uptimeSeconds"] == 12.5
    # Reserved blocks are NOT misreported as used: used=(1000-300)*4096, not (1000-200)*4096.
    assert m["disk"]["usedBytes"] == 2867200


if __name__ == "__main__":
    test_metrics()
    print("metrics fixture OK")
