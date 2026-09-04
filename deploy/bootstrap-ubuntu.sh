#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "bootstrap-ubuntu.sh must run as root" >&2
  exit 1
fi

if [ -z "${CANTER_SSH_CIDR:-}" ]; then
  echo "CANTER_SSH_CIDR is required (for example, 203.0.113.8/32)" >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y ca-certificates curl gnupg postgresql postgresql-client ufw unzip

if ! command -v aws >/dev/null 2>&1; then
  aws_bundle=/tmp/awscliv2.zip
  aws_unpack=/tmp/aws
  curl -fsSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o "$aws_bundle"
  rm -rf "$aws_unpack"
  unzip -q "$aws_bundle" -d /tmp
  /tmp/aws/install --bin-dir /usr/local/bin --install-dir /usr/local/aws-cli
  rm -rf "$aws_bundle" "$aws_unpack"
fi

if ! command -v node >/dev/null 2>&1 || [ "$(node -p 'Number(process.versions.node.split(`.`)[0])')" -lt 22 ]; then
  curl -fsSL https://deb.nodesource.com/setup_22.x | sh -
  apt-get install -y nodejs
fi

if ! command -v caddy >/dev/null 2>&1; then
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update
  apt-get install -y caddy
fi

id canter >/dev/null 2>&1 || useradd --system --create-home --home-dir /var/lib/canter --shell /usr/sbin/nologin canter
install -d -o canter -g canter -m 0750 /opt/canter /opt/canter/bin /opt/canter/web /opt/canter/deploy /var/lib/canter
install -d -o root -g canter -m 0750 /etc/canter

if ! swapon --show --noheadings | grep -q .; then
  fallocate -l 2G /swapfile
  chmod 600 /swapfile
  mkswap /swapfile
  swapon /swapfile
  grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi

ufw allow from "$CANTER_SSH_CIDR" to any port 22 proto tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable
