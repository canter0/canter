set -eu

umask 027
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

HOST_MEMORY_MIB=@@HOST_MEMORY_MIB@@
HOST_RESERVE_MIB=@@HOST_RESERVE_MIB@@
GUEST_COUNT=@@GUEST_COUNT@@
GUEST_MEMORY_MIB=@@GUEST_MEMORY_MIB@@
GUEST_VCPU=@@GUEST_VCPU@@
FIRECRACKER_VERSION=@@FIRECRACKER_VERSION@@
FIRECRACKER_URL='@@FIRECRACKER_URL@@'
FIRECRACKER_SHA256=@@FIRECRACKER_SHA256@@
UBUNTU_SNAPSHOT=@@UBUNTU_SNAPSHOT@@
ROOTFS_URL='@@ROOTFS_URL@@'
ROOTFS_SHA256=@@ROOTFS_SHA256@@
MYSQL_READINESS_PASSWORD=$(tr -d '-' </proc/sys/kernel/random/uuid)
[ -n "$MYSQL_READINESS_PASSWORD" ] || { printf 'FATAL: could not generate readiness credential\n' >&2; exit 1; }

STATE_DIR=/var/lib/canter-firecracker-mysql
BUILD_DIR=$STATE_DIR/build
GUEST_DIR=$STATE_DIR/guests
LOG_FILE=$STATE_DIR/bootstrap.log

fail() {
  printf 'FATAL: %s\n' "$*" >&2
  exit 1
}

log() {
  printf '%s\n' "$*" | tee -a "$LOG_FILE" >&2
}

verify_sha256() {
  expected=$1
  path=$2
  printf '%s  %s\n' "$expected" "$path" | sha256sum --check --status || fail "SHA-256 mismatch for $path"
}

install -d -m 0750 "$STATE_DIR" "$BUILD_DIR" "$GUEST_DIR" /run/canter-mysql
: >"$LOG_FILE"

[ "$(uname -m)" = x86_64 ] || fail "this pinned guest build requires an x86_64 host"
. /etc/os-release
[ "$ID" = ubuntu ] && [ "$VERSION_ID" = 24.04 ] || fail "host must be Ubuntu 24.04"

# Firecracker's documented host requirement is real read/write access to KVM.
# This gate intentionally runs before downloading or constructing anything.
if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
  fail "nested KVM unavailable: Firecracker requires read/write /dev/kvm"
fi

memtotal_kib=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
# A nominal 1024 MiB flavor reports slightly less to Linux after firmware/kernel reservations.
[ "$memtotal_kib" -ge 900000 ] && [ "$memtotal_kib" -le 1100000 ] || fail "c1 host is not in the required 1024 MiB memory envelope"
[ $((HOST_MEMORY_MIB - HOST_RESERVE_MIB - GUEST_COUNT * GUEST_MEMORY_MIB)) -eq 140 ] || fail "invalid 384 MiB host reserve or guest memory budget"

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends ca-certificates coreutils cpu-checker curl e2fsprogs iproute2 mysql-client-core-8.0 procps tar xz-utils

# /dev/kvm can exist but still be unusable when the provider has not enabled nested KVM.
# kvm-ok performs a host capability check; Firecracker startup below is the final ioctl test.
if ! kvm-ok >>"$LOG_FILE" 2>&1; then
  fail "nested KVM unavailable: kvm-ok rejected this host"
fi

log "downloading and verifying Firecracker $FIRECRACKER_VERSION"
curl --fail --location --proto '=https' --tlsv1.2 --output "$BUILD_DIR/firecracker.tgz" "$FIRECRACKER_URL"
verify_sha256 "$FIRECRACKER_SHA256" "$BUILD_DIR/firecracker.tgz"
tar -xzf "$BUILD_DIR/firecracker.tgz" -C "$BUILD_DIR"
install -m 0755 "$BUILD_DIR/release-v${FIRECRACKER_VERSION}-x86_64/firecracker-v${FIRECRACKER_VERSION}-x86_64" /usr/local/bin/firecracker

log "downloading and verifying the fixed Ubuntu Noble guest root tree"
curl --fail --location --proto '=https' --tlsv1.2 --output "$BUILD_DIR/ubuntu-root.tar.xz" "$ROOTFS_URL"
verify_sha256 "$ROOTFS_SHA256" "$BUILD_DIR/ubuntu-root.tar.xz"

ROOT_TREE=$BUILD_DIR/root-tree
install -d -m 0755 "$ROOT_TREE"
tar -xJpf "$BUILD_DIR/ubuntu-root.tar.xz" -C "$ROOT_TREE"

# All guest packages are resolved from an immutable Ubuntu archive timestamp.
# apt validates signed Release metadata and each package hash from that snapshot.
cat >"$ROOT_TREE/etc/apt/sources.list.d/canter-snapshot.sources" <<EOF_SOURCES
Types: deb
URIs: https://snapshot.ubuntu.com/ubuntu/${UBUNTU_SNAPSHOT}/
Suites: noble noble-updates noble-security
Components: main restricted universe
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
Check-Valid-Until: no
EOF_SOURCES
: >"$ROOT_TREE/etc/apt/sources.list"
printf '#!/bin/sh\nexit 101\n' >"$ROOT_TREE/usr/sbin/policy-rc.d"
chmod 0755 "$ROOT_TREE/usr/sbin/policy-rc.d"
cp --remove-destination /etc/resolv.conf "$ROOT_TREE/etc/resolv.conf"

cleanup_mounts() {
  umount "$ROOT_TREE/dev/pts" 2>/dev/null || true
  umount "$ROOT_TREE/dev" 2>/dev/null || true
  umount "$ROOT_TREE/proc" 2>/dev/null || true
  umount "$ROOT_TREE/sys" 2>/dev/null || true
}
trap cleanup_mounts EXIT INT TERM
mount -t proc proc "$ROOT_TREE/proc"
mount -t sysfs sys "$ROOT_TREE/sys"
mount --bind /dev "$ROOT_TREE/dev"
mount --bind /dev/pts "$ROOT_TREE/dev/pts"

log "installing signed snapshot packages into the guest root tree"
chroot "$ROOT_TREE" /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get update
chroot "$ROOT_TREE" /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends iproute2 linux-image-virtual mysql-server
chroot "$ROOT_TREE" apt-get clean
cleanup_mounts
trap - EXIT INT TERM

kernel_path=$(readlink -f "$ROOT_TREE/boot/vmlinuz")
initrd_path=$(readlink -f "$ROOT_TREE/boot/initrd.img")
[ -f "$kernel_path" ] && [ -f "$initrd_path" ] || fail "snapshot kernel or initramfs was not installed"
cp "$kernel_path" "$GUEST_DIR/vmlinux"
cp "$initrd_path" "$GUEST_DIR/initrd.img"

cat >"$ROOT_TREE/etc/mysql/mysql.conf.d/90-canter-low-memory.cnf" <<'EOF_MYSQL'
[mysqld]
bind-address=0.0.0.0
mysqlx=OFF
skip-name-resolve
performance-schema=OFF
disable-log-bin
max-connections=10
table-open-cache=128
table-definition-cache=128
thread-cache-size=0
innodb-buffer-pool-size=32M
innodb-log-buffer-size=4M
key-buffer-size=8M
tmp-table-size=8M
max-heap-table-size=8M
sort-buffer-size=256K
read-buffer-size=256K
read-rnd-buffer-size=256K
innodb-flush-method=O_DIRECT
EOF_MYSQL
chroot "$ROOT_TREE" /usr/sbin/mysqld --validate-config
if chroot "$ROOT_TREE" dpkg-query --show mariadb-server >/dev/null 2>&1; then
  fail "MariaDB was installed; this contract requires Oracle MySQL"
fi

cat >"$ROOT_TREE/usr/local/sbin/canter-network" <<'EOF_NETWORK_SCRIPT'
#!/bin/sh
set -eu
guest_ip=
for argument in $(cat /proc/cmdline); do
  case "$argument" in
    canter_ip=*) guest_ip=${argument#canter_ip=} ;;
  esac
done
[ -n "$guest_ip" ] || { echo 'missing canter_ip kernel argument' >&2; exit 1; }
ip link set eth0 up
ip address add "$guest_ip" dev eth0
EOF_NETWORK_SCRIPT
chmod 0755 "$ROOT_TREE/usr/local/sbin/canter-network"

cat >"$ROOT_TREE/etc/systemd/system/canter-network.service" <<'EOF_NETWORK_UNIT'
[Unit]
Description=Configure the isolated Canter TAP endpoint
Before=network.target mysql.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/canter-network
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF_NETWORK_UNIT

cat >"$ROOT_TREE/usr/local/sbin/canter-mysql-user" <<EOF_MYSQL_USER
#!/bin/sh
set -eu
i=0
until mysqladmin --protocol=socket -uroot ping >/dev/null 2>&1; do
  i=\$((i + 1))
  [ "\$i" -lt 60 ] || { echo 'local MySQL startup timed out' >&2; exit 1; }
  sleep 1
done
mysql --protocol=socket -uroot <<'EOF_SQL'
CREATE USER IF NOT EXISTS 'canter'@'10.200.%' IDENTIFIED WITH caching_sha2_password BY '${MYSQL_READINESS_PASSWORD}';
GRANT USAGE ON *.* TO 'canter'@'10.200.%';
FLUSH PRIVILEGES;
EOF_SQL
EOF_MYSQL_USER
chmod 0700 "$ROOT_TREE/usr/local/sbin/canter-mysql-user"

cat >"$ROOT_TREE/etc/systemd/system/canter-mysql-user.service" <<'EOF_MYSQL_USER_UNIT'
[Unit]
Description=Create the host-side SQL readiness principal
Requires=mysql.service canter-network.service
After=mysql.service canter-network.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/canter-mysql-user
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF_MYSQL_USER_UNIT

install -d -m 0755 "$ROOT_TREE/etc/systemd/system/mysql.service.d" "$ROOT_TREE/etc/systemd/system/multi-user.target.wants"
cat >"$ROOT_TREE/etc/systemd/system/mysql.service.d/canter-network.conf" <<'EOF_MYSQL_ORDER'
[Unit]
Requires=canter-network.service
After=canter-network.service
EOF_MYSQL_ORDER
ln -sfn ../canter-network.service "$ROOT_TREE/etc/systemd/system/multi-user.target.wants/canter-network.service"
ln -sfn ../canter-mysql-user.service "$ROOT_TREE/etc/systemd/system/multi-user.target.wants/canter-mysql-user.service"
rm -f "$ROOT_TREE/var/lib/mysql/auto.cnf"

log "constructing two independent ext4 guest disks"
truncate -s 2048M "$BUILD_DIR/rootfs-base.ext4"
mkfs.ext4 -F -q -L canter-mysql -d "$ROOT_TREE" "$BUILD_DIR/rootfs-base.ext4"
cp --reflink=auto --sparse=always "$BUILD_DIR/rootfs-base.ext4" "$GUEST_DIR/mysql-1.ext4"
cp --reflink=auto --sparse=always "$BUILD_DIR/rootfs-base.ext4" "$GUEST_DIR/mysql-2.ext4"

create_tap() {
  tap=$1
  host_cidr=$2
  if ip link show "$tap" >/dev/null 2>&1; then
    fail "refusing pre-existing TAP device $tap"
  fi
  ip tuntap add dev "$tap" mode tap
  ip address add "$host_cidr" dev "$tap"
  ip link set "$tap" up
}

# Separate, unbridged /30 TAP networks prevent guest-to-guest L2 communication.
create_tap tap-mysql-1 10.200.1.1/30
create_tap tap-mysql-2 10.200.2.1/30

write_config() {
  guest=$1
  tap=$2
  guest_ip=$3
  mac=$4
  cat >"$GUEST_DIR/mysql-${guest}.json" <<EOF_CONFIG
{
  "boot-source": {
    "kernel_image_path": "$GUEST_DIR/vmlinux",
    "initrd_path": "$GUEST_DIR/initrd.img",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw canter_ip=${guest_ip}/30"
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "path_on_host": "$GUEST_DIR/mysql-${guest}.ext4",
      "is_root_device": true,
      "is_read_only": false
    }
  ],
  "machine-config": {
    "vcpu_count": ${GUEST_VCPU},
    "mem_size_mib": ${GUEST_MEMORY_MIB},
    "smt": false,
    "track_dirty_pages": false
  },
  "network-interfaces": [
    {
      "iface_id": "eth0",
      "guest_mac": "$mac",
      "host_dev_name": "$tap"
    }
  ]
}
EOF_CONFIG
}

write_config 1 tap-mysql-1 10.200.1.2 06:00:ac:10:00:01
write_config 2 tap-mysql-2 10.200.2.2 06:00:ac:10:00:02

cat >/etc/systemd/system/canter-mysql-firecracker@.service <<EOF_FIRECRACKER_UNIT
[Unit]
Description=Canter Oracle MySQL Firecracker guest %i
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/firecracker --api-sock /run/canter-mysql/mysql-%i.sock --config-file $GUEST_DIR/mysql-%i.json
Restart=no
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF_FIRECRACKER_UNIT

systemctl daemon-reload
systemctl enable canter-mysql-firecracker@1.service canter-mysql-firecracker@2.service
systemctl start canter-mysql-firecracker@1.service canter-mysql-firecracker@2.service

sql_ready() {
  guest_name=$1
  guest_ip=$2
  attempt=0
  while [ "$attempt" -lt 180 ]; do
    result=$(MYSQL_PWD="$MYSQL_READINESS_PASSWORD" mysql --protocol=TCP --ssl-mode=DISABLED --get-server-public-key --connect-timeout=2 --host="$guest_ip" --user=canter --batch --skip-column-names --execute='SELECT 1' 2>/dev/null || true)
    if [ "$result" = 1 ]; then
      printf '%s\n' "$result"
      return 0
    fi
    if ! systemctl is-active --quiet "canter-mysql-firecracker@${guest_name#mysql-}.service"; then
      journalctl --no-pager -u "canter-mysql-firecracker@${guest_name#mysql-}.service" >&2 || true
      fail "$guest_name stopped before SQL readiness"
    fi
    attempt=$((attempt + 1))
    sleep 2
  done
  fail "$guest_name did not return 1 for SELECT 1"
}

# These are deliberately two explicit host-side SQL acceptance checks.
mysql_1_result=$(sql_ready mysql-1 10.200.1.2)
mysql_2_result=$(sql_ready mysql-2 10.200.2.2)

firecracker_count=$(pgrep -fc '^/usr/local/bin/firecracker --api-sock /run/canter-mysql/mysql-[12]\.sock ' || true)
[ "$firecracker_count" -eq 2 ] || fail "expected exactly two live Firecracker processes, found $firecracker_count"

cat >"$STATE_DIR/acceptance.json" <<EOF_ACCEPTANCE
{"kvm":"read-write-and-kvm-ok","firecrackerVersion":"${FIRECRACKER_VERSION}","guestCount":2,"guestMemoryMiB":250,"guestVcpu":1,"mysql-1":{"address":"10.200.1.2:3306","query":"SELECT 1","result":${mysql_1_result}},"mysql-2":{"address":"10.200.2.2:3306","query":"SELECT 1","result":${mysql_2_result}}}
EOF_ACCEPTANCE
chmod 0640 "$STATE_DIR/acceptance.json"
log "live acceptance passed: two KVM Firecracker guests each returned 1 for SELECT 1"
