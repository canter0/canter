#!/bin/sh
# Bootstrap only; ordinary releases install the runtime through pull-release.py.
set -eu
if [ "$(id -u)" -ne 0 ]; then
  echo "Run install-harness.sh as root." >&2
  exit 1
fi
release_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
/usr/bin/node -e 'const [major, minor] = process.versions.node.split(".").map(Number); if (major < 22 || (major === 22 && minor < 13)) process.exit(1)' || { echo "Node >=22.13 is required." >&2; exit 1; }
if [ ! -f "$release_root/harness/node_modules/just-bash/package.json" ]; then
  echo "Use the CI release, or run npm ci --prefix harness --ignore-scripts as an unprivileged user first." >&2
  exit 1
fi
install -d -o root -g root -m 0755 /opt/canter-harness/releases
runtime=$(mktemp -d /opt/canter-harness/releases/bootstrap-XXXXXXXX)
cp -a "$release_root/harness/runner.mjs" "$release_root/harness/package.json" "$release_root/harness/package-lock.json" "$release_root/harness/node_modules" "$runtime/"
chown -R root:root "$runtime"
chmod -R go+rX "$runtime"
if systemctl is-active --quiet canter-harness.socket; then
  systemctl stop canter-harness.socket
fi
systemctl stop 'canter-harness@*.service'
ln -s "$runtime" /opt/canter-harness/current.pending
mv -Tf /opt/canter-harness/current.pending /opt/canter-harness/current
install -o root -g root -m 0644 "$release_root/deploy/canter-harness.socket" "$release_root/deploy/canter-harness@.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now canter-harness.socket
echo "Command service installed. The next control-plane startup verifies and enables it."
