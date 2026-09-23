#!/usr/bin/env python3
"""Read Canter production status over Tailscale; never fall back to public SSH."""
import json
from pathlib import Path
import shutil
import subprocess
import sys
import urllib.request
from check_fonts import check_fonts

ALIAS = 'canter-production'
ROOT = Path(__file__).resolve().parents[1]
REMOTE_STATUS = r'''
import json, pathlib, shutil, subprocess, urllib.request
units = ['tailscaled', 'canter-controlplane', 'canter-web', 'canter-deploy.timer']
services = {}
for unit in units:
    result = subprocess.run(['systemctl', 'is-active', unit], text=True, capture_output=True)
    services[unit] = result.stdout.strip()
release = pathlib.Path('/var/lib/canter-deploy/current')
failed = pathlib.Path('/var/lib/canter-deploy/failed')
with urllib.request.urlopen('http://127.0.0.1:8081/readyz', timeout=5) as response:
    readiness = json.load(response)
print(json.dumps({
    'services': services,
    'readiness': readiness,
    'activeCommit': release.read_text().strip() if release.exists() else None,
    'failedCommit': failed.read_text().strip() if failed.exists() else None,
    'freeDiskGiB': round(shutil.disk_usage('/opt/canter').free / 2**30, 2),
    'harnessSocketPresent': pathlib.Path('/run/canter-harness.sock').is_socket(),
}))
'''


def command(args, **kwargs):
    result = subprocess.run(args, capture_output=True, text=True, timeout=25, **kwargs)
    if result.returncode:
        # Commands here never receive credentials. Avoid dumping remote output.
        raise RuntimeError(f'{args[0]} failed (exit {result.returncode}); check Tailscale login, SSH access, and production services')
    return result.stdout


def verify_private_target(status, ssh_config):
    if status.get('BackendState') != 'Running':
        raise RuntimeError('Tailscale is not connected. Sign in to the existing tailnet, then retry; do not open public SSH')
    peer = next((p for p in (status.get('Peer') or {}).values()
                 if p.get('HostName') == ALIAS or p.get('DNSName', '').split('.')[0] == ALIAS), None)
    if not peer or not peer.get('Online'):
        raise RuntimeError('The canter-production Tailscale peer is unavailable; do not fall back to its public IP')
    config = dict(line.split(None, 1) for line in ssh_config.splitlines() if ' ' in line)
    allowed = set(peer.get('TailscaleIPs') or []) | {ALIAS, peer.get('DNSName', '').rstrip('.')}
    hostname = config.get('hostname', '').rstrip('.')
    if hostname not in allowed or config.get('proxycommand', 'none') != 'none' or config.get('proxyjump', 'none') != 'none':
        raise RuntimeError('The production SSH alias does not use the verified Tailscale peer directly; inspect ~/.ssh/config')
    return peer


def main():
    tailscale = shutil.which('tailscale')
    if not tailscale:
        raise RuntimeError('Install the official Tailscale client and join the existing tailnet; see deploy/README.md')
    status = json.loads(command([tailscale, 'status', '--json']))
    ssh_config = command(['ssh', '-G', ALIAS])
    peer = verify_private_target(status, ssh_config)
    # Pin this connection to an address returned by the current tailnet, rather
    # than relying on potentially different public DNS for a short hostname.
    address = next((ip for ip in peer.get('TailscaleIPs', []) if ':' not in ip), None)
    if not address:
        raise RuntimeError('The production peer has no Tailscale IPv4 address')
    remote = json.loads(command(['ssh', '-T', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10',
                                 '-o', 'StrictHostKeyChecking=yes', '-o', 'HostName=' + address,
                                 ALIAS, 'python3', '-'], input=REMOTE_STATUS))
    public = {}
    for name in ['release.json', 'readyz']:
        with urllib.request.urlopen('https://canter.dev/' + name, timeout=10) as response:
            public[name] = json.load(response)
    local = command(['git', '-C', str(ROOT), 'rev-parse', 'HEAD']).strip()
    dirty = bool(command(['git', '-C', str(ROOT), 'status', '--porcelain']).strip())
    public['fonts'] = check_fonts()
    report = {'access': 'SSH over Tailscale', 'target': ALIAS, 'localCommit': local,
              'uncommittedChanges': dirty, 'production': remote, 'public': public}
    report['healthy'] = (all(state == 'active' for state in remote['services'].values())
                         and remote['readiness'].get('status') == 'ready'
                         and public['readyz'].get('status') == 'ready'
                         and all(font['healthy'] for font in public['fonts'].values())
                         and remote['activeCommit'] == public['release.json'].get('commit'))
    print(json.dumps(report, indent=2))
    return 0 if report['healthy'] else 1


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired) as exc:
        print(f'Production check failed: {exc}', file=sys.stderr)
        sys.exit(1)
