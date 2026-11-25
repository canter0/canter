#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "install-monitoring.sh must run as root" >&2
  exit 1
fi

script_dir=$(
  CDPATH=
  cd -- "$(dirname -- "$0")"
  pwd
)
config_dir=$script_dir/monitoring
for required in prometheus.yml prometheus.default prometheus-node-exporter.default grafana-datasource.yaml grafana-dashboard-provider.yaml grafana-server.service.conf canter-overview.json; do
  if [ ! -f "$config_dir/$required" ]; then
    echo "missing monitoring configuration: $config_dir/$required" >&2
    exit 1
  fi
done

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl gnupg

install -d -m 0755 /etc/apt/keyrings
key_tmp=$(mktemp)
trap 'rm -f "$key_tmp"' EXIT
trap 'exit 1' HUP INT TERM
curl -fsSL https://apt.grafana.com/gpg-full.key -o "$key_tmp"
primary_fingerprints=$(gpg --batch --show-keys --with-fingerprint --with-colons "$key_tmp" 2>/dev/null | awk -F: '$1 == "pub" { primary = 1 } $1 == "sub" { primary = 0 } $1 == "fpr" && primary { print $10 }')
for expected_fingerprint in 4E40DDF6D76E284A4A6780E48C8C34C524098CB6 0E22EB88E39E12277A7760AE9E439B102CF3C0C6 B53AE77BADB630A683046005963FA27710458545; do
  if ! printf '%s\n' "$primary_fingerprints" | grep -qx "$expected_fingerprint"; then
    echo "Grafana APT repository key fingerprints did not match the pinned key set" >&2
    exit 1
  fi
done
if [ "$(printf '%s\n' "$primary_fingerprints" | sed '/^$/d' | wc -l)" -ne 3 ]; then
  echo "Grafana APT repository keyring had an unexpected number of primary keys" >&2
  exit 1
fi
install -m 0644 "$key_tmp" /etc/apt/keyrings/grafana.asc
printf '%s\n' 'deb [signed-by=/etc/apt/keyrings/grafana.asc] https://apt.grafana.com stable main' > /etc/apt/sources.list.d/grafana.list

install -d -m 0755 /etc/prometheus /etc/systemd/system/grafana-server.service.d
install -m 0644 "$config_dir/prometheus.yml" /etc/prometheus/prometheus.yml
install -m 0644 "$config_dir/prometheus.default" /etc/default/prometheus
install -m 0644 "$config_dir/prometheus-node-exporter.default" /etc/default/prometheus-node-exporter
install -m 0644 "$config_dir/grafana-server.service.conf" /etc/systemd/system/grafana-server.service.d/10-canter.conf
install -d -m 0700 /etc/canter-monitoring
if [ ! -f /etc/canter-monitoring/grafana.env ]; then
  admin_password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
  secret_key=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
  (
    umask 077
    cat > /etc/canter-monitoring/grafana.env <<EOF_ENV
GF_SECURITY_ADMIN_PASSWORD=$admin_password
GF_SECURITY_SECRET_KEY=$secret_key
EOF_ENV
  )
  unset admin_password secret_key
fi
chmod 0600 /etc/canter-monitoring/grafana.env

apt-get update
grafana_version=13.3.1
apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold install -y --no-install-recommends prometheus prometheus-node-exporter "grafana=$grafana_version"

if ! getent passwd prometheus >/dev/null; then
  echo "the prometheus package did not create its prometheus service account" >&2
  exit 1
fi
if ! getent passwd grafana >/dev/null; then
  echo "the grafana package did not create its grafana service account" >&2
  exit 1
fi

install -m 0644 "$config_dir/prometheus.yml" /etc/prometheus/prometheus.yml
install -m 0644 "$config_dir/prometheus.default" /etc/default/prometheus
install -m 0644 "$config_dir/prometheus-node-exporter.default" /etc/default/prometheus-node-exporter

install -d -m 0750 -o root -g grafana /etc/canter-monitoring /etc/grafana/dashboards /etc/grafana/provisioning/datasources /etc/grafana/provisioning/dashboards
install -m 0640 -o root -g grafana "$config_dir/grafana-datasource.yaml" /etc/grafana/provisioning/datasources/canter.yaml
install -m 0640 -o root -g grafana "$config_dir/grafana-dashboard-provider.yaml" /etc/grafana/provisioning/dashboards/canter.yaml
install -m 0640 -o root -g grafana "$config_dir/canter-overview.json" /etc/grafana/dashboards/canter-overview.json
chown root:grafana /etc/canter-monitoring/grafana.env
chmod 0640 /etc/canter-monitoring/grafana.env

promtool check config /etc/prometheus/prometheus.yml >/dev/null
systemctl daemon-reload
systemctl enable prometheus prometheus-node-exporter grafana-server >/dev/null
systemctl restart prometheus prometheus-node-exporter grafana-server

wait_for_url() {
  name=$1
  url=$2
  attempt=0
  while [ "$attempt" -lt 30 ]; do
    if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 1
  done
  echo "$name did not become ready; inspect systemctl status and journalctl" >&2
  exit 1
}

systemctl is-active --quiet prometheus
systemctl is-active --quiet prometheus-node-exporter
systemctl is-active --quiet grafana-server
wait_for_url prometheus http://127.0.0.1:9090/-/ready
wait_for_url node-exporter http://127.0.0.1:9100/metrics
wait_for_url grafana http://127.0.0.1:3001/api/health

echo "Monitoring is active. Grafana listens only on 127.0.0.1:3001; credentials remain in root-only /etc/canter-monitoring/grafana.env."
