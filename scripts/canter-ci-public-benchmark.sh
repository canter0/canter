#!/bin/bash
set -euo pipefail

test "$(id -u)" != 0
test "$(uname -s)" = Linux
test "$(uname -m)" = x86_64
test ! -e "$HOME/public-ci-benchmark"
test ! -e "$HOME/public-ci-results"
test ! -e "$HOME/public-ci-node"
mkdir "$HOME/public-ci-results" "$HOME/public-ci-node"

sudo apt-get update
sudo apt-get install -y --no-install-recommends php-cli php-xml php-zip php-curl unzip build-essential
for unit in phoromatic-client phoromatic-server phoronix-result-server; do
    sudo systemctl mask "$unit.service"
done

cd "$HOME/public-ci-results"
curl -fL --retry 3 --connect-timeout 15 --max-time 180 -o pts.deb \
    https://github.com/phoronix-test-suite/phoronix-test-suite/releases/download/v10.8.4/phoronix-test-suite_10.8.4_all.deb
printf '%s\n' 'be81f71fc0382a7725dc88f4a18f013d1c3f6939d440629231d392a11816feca  pts.deb' | sha256sum -c -
sudo dpkg -i pts.deb
curl -fL --retry 3 --connect-timeout 15 --max-time 180 -o node.tar.gz \
    https://nodejs.org/dist/v22.23.1/node-v22.23.1-linux-x64.tar.gz
printf '%s\n' '7a8cb04b4a1df4eaf432125324b81b29a088e73570a23259a8de1c65d07fc129  node.tar.gz' | sha256sum -c -
tar -xzf node.tar.gz --strip-components=1 -C "$HOME/public-ci-node"
export PATH="$HOME/public-ci-node/bin:/opt/canter-ci/bin:$PATH"
test "$(node --version)" = v22.23.1

git clone --quiet https://github.com/starslingdev/hpc-sandbox-benchmarks.git "$HOME/public-ci-benchmark"
git -C "$HOME/public-ci-benchmark" checkout --quiet d02dfe89375e80f2a2814b2e8bea1e5f24646d95
test "$(git -C "$HOME/public-ci-benchmark" rev-parse HEAD)" = d02dfe89375e80f2a2814b2e8bea1e5f24646d95
node --version > "$HOME/public-ci-results/node-version.txt"
phoronix-test-suite version > "$HOME/public-ci-results/pts-version.txt"
export BENCHMARK_RESULTS_DIR="$HOME/public-ci-results"
export PTS_RESPECT_TIMES_TO_RUN=1 FORCE_TIMES_TO_RUN=2 SUDO=sudo
unset MONITOR PERFORMANCE_PER_WATT BENCH_PTS_CONVERGE
bash "$HOME/public-ci-benchmark/.mise/tasks/benchmark/cpu/pts/node-web-tooling"
test -s "$HOME/public-ci-results/pts_node-web-tooling.xml"
python3 - <<'PY'
import json
import math
import os
from pathlib import Path
import xml.etree.ElementTree as ET

directory = Path(os.environ['HOME']) / 'public-ci-results'
root = ET.parse(directory / 'pts_node-web-tooling.xml').getroot()
results = root.findall('.//Result')
matching = [result for result in results
            if result.findtext('Identifier') == 'pts/node-web-tooling-1.0.1'
            and result.findtext('Scale') == 'runs/s']
if len(matching) != 1:
    raise SystemExit('Missing or ambiguous Node Web Tooling result; no score claimed')
entries = matching[0].findall('./Data/Entry')
if len(entries) != 1:
    raise SystemExit('Missing or ambiguous trial entry; no score claimed')
raw = entries[0].findtext('RawString')
if not raw:
    raise SystemExit('Raw PTS trial values absent; no score claimed')
samples = [float(value) for value in raw.split(':')]
if len(samples) != 2 or not all(math.isfinite(value) and value > 0 for value in samples):
    raise SystemExit('Expected two successful finite positive trials; no score claimed')
evidence = {
    'schemaVersion': 1,
    'suite': 'pts/node-web-tooling-1.0.1',
    'sourceCommit': 'd02dfe89375e80f2a2814b2e8bea1e5f24646d95',
    'nodeVersion': '22.23.1',
    'ptsVersion': '10.8.4',
    'samples': samples,
    'unit': 'runs/s',
    'vmReplicates': 1,
    'caveat': 'Two trials on one VM; published reference uses five VMs. CPU workload only, not end-to-end CI.',
}
(directory / 'samples.json').write_text(json.dumps(evidence, indent=2) + '\n')
print(json.dumps(evidence, indent=2))
PY
