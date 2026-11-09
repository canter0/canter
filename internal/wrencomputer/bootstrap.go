package wrencomputer

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/base64"
	"fmt"
	"net/netip"
	"strings"

	"golang.org/x/crypto/ssh"
)

//go:embed guest.py
var guestHelper []byte

//go:embed canter-apt
var canterApt []byte

// BootstrapConfig contains only one computer's public SSH key and the exact
// source network allowed to reach SSH. It never includes provider credentials.
type BootstrapConfig struct {
	SSHPublicKey  string
	SSHSourceCIDR string
	// Desktop selects the full graphical environment used by Wren computers.
	// Desktop=false installs a lean headless image for sandboxes.
	Desktop bool
	// Egress is "restricted" (free tier: only 80/443/53 out) or "standard"
	// (default allow minus private ranges and mail). Empty preserves the
	// existing Wren computer default-allow policy.
	Egress string
	// ImageVersion, when set for a sandbox, lets the bootstrap skip the
	// package install entirely on VMs booted from a prebuilt sandbox image
	// stamped with the same version. Missing/mismatched versions fall back
	// to the full install.
	ImageVersion string
	// Sandbox marks a Canter sandbox (not a Wren computer): with Desktop it
	// layers the sandbox package/tools install onto the desktop environment.
	Sandbox bool
}

const bootstrapDesktopPackages = `xvfb x11-utils x11-xserver-utils dmz-cursor-theme xdotool scrot xclip openbox dbus-x11 fonts-liberation libreoffice-writer libreoffice-calc`

const bootstrapSandboxPackages = `python3-venv python3-pip git curl jq zip unzip build-essential nodejs npm`

// Bootstrap returns a fixed guest setup script for the existing SDK boot-proof
// wrapper. The final stdout line is the guest host-key SHA256 fingerprint.
// The caller must obtain that proof through its trusted M1 boot receipt, not
// accept an unverified host key discovered on the network.
func compressedBase64(raw []byte) string {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	writer.Write(raw)
	writer.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func Bootstrap(config BootstrapConfig) (string, error) {
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(config.SSHPublicKey))
	if err != nil || len(rest) != 0 || len(options) != 0 {
		return "", fmt.Errorf("one SSH public key without options is required")
	}
	source, err := netip.ParsePrefix(config.SSHSourceCIDR)
	if err != nil || !source.Addr().Is4() || source.Bits() < 24 {
		return "", fmt.Errorf("computer SSH requires an explicit IPv4 source range of /24 or smaller")
	}
	publicKey := base64.StdEncoding.EncodeToString(ssh.MarshalAuthorizedKey(key))
	// guest.py exceeds the safe user-data budget uncompressed; the helper is
	// gzip'd before base64 so the bootstrap stays under the cap. The file
	// lands byte-identical after base64|gzip -d.
	helper := compressedBase64(guestHelper)
	aptHelper := base64.StdEncoding.EncodeToString(canterApt)
	script := `canter_computer_install_log=/var/log/canter-computer-install.log
trap 'canter_computer_rc=$?; if [ "$canter_computer_rc" -ne 0 ]; then tail -c 1800 "$canter_computer_install_log" >&2; fi' EXIT
(
set -eu
export DEBIAN_FRONTEND=noninteractive
`
	if config.Desktop || config.ImageVersion == "" {
		script += `apt-get update -qq
`
	}
	if config.Desktop {
		desktopPackages := bootstrapDesktopPackages
		if config.Sandbox {
			desktopPackages += " " + bootstrapSandboxPackages
		}
		script += `apt-get install -y --no-install-recommends python3 python-is-python3 python3-venv python3-pip python3-openpyxl python3-docx python3-pil python3-requests python3-pypdf python3-reportlab poppler-utils git zip unzip jq openssh-server ufw ca-certificates curl ` + desktopPackages + `
if ! command -v google-chrome >/dev/null; then
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb -o /tmp/canter-chrome.deb
  apt-get install -y /tmp/canter-chrome.deb
  rm -f /tmp/canter-chrome.deb
fi
`
	} else if config.ImageVersion != "" {
		script += `if [ "$(cat /opt/canter/sandbox-image-version 2>/dev/null)" != "` + config.ImageVersion + `" ]; then
apt-get update -qq
apt-get install -y --no-install-recommends python3 python-is-python3 ` + bootstrapSandboxPackages + ` openssh-server ufw ca-certificates
printf '%s' '` + aptHelper + `' | base64 -d > /usr/local/sbin/canter-apt
chown root:root /usr/local/sbin/canter-apt
chmod 755 /usr/local/sbin/canter-apt
printf 'wren ALL=(root) NOPASSWD: /usr/local/sbin/canter-apt\n' > /etc/sudoers.d/canter-apt
chmod 440 /etc/sudoers.d/canter-apt
visudo -cf /etc/sudoers.d/canter-apt
install -d -m 755 -o root -g root /opt/canter
touch /opt/canter/sandbox
printf '%s\n' '` + config.ImageVersion + `' > /opt/canter/sandbox-image-version
fi
`
	} else {
		script += `apt-get install -y --no-install-recommends python3 python-is-python3 ` + bootstrapSandboxPackages + ` openssh-server ufw ca-certificates
`
	}
	script += `id wren >/dev/null 2>&1 || useradd -m -s /bin/bash wren
passwd -l wren >/dev/null
install -d -m 700 -o wren -g wren /home/wren/.ssh /home/wren/.canter
install -d -m 700 -o wren -g wren /workspace
printf '%s' 'restrict,command="python3 /opt/canter/computer-agent.py" ' > /home/wren/.ssh/authorized_keys
printf '%s' '` + publicKey + `' | base64 -d >> /home/wren/.ssh/authorized_keys
chown wren:wren /home/wren/.ssh/authorized_keys
chmod 600 /home/wren/.ssh/authorized_keys
install -d -m 755 -o root -g root /opt/canter
printf '%s' '` + helper + `' | base64 -d | gzip -d > /opt/canter/computer-agent.py
chown root:root /opt/canter/computer-agent.py
chmod 755 /opt/canter/computer-agent.py
printf '%s' '` + compressedBase64(sandboxSnapshotHelper) + `' | base64 -d | gzip -d > /opt/canter/sandbox-snapshot.py
chown root:root /opt/canter/sandbox-snapshot.py
chmod 644 /opt/canter/sandbox-snapshot.py
`
	if (!config.Desktop && config.ImageVersion == "") || (config.Desktop && config.Sandbox) {
		script += `printf '%s' '` + aptHelper + `' | base64 -d > /usr/local/sbin/canter-apt
chown root:root /usr/local/sbin/canter-apt
chmod 755 /usr/local/sbin/canter-apt
printf 'wren ALL=(root) NOPASSWD: /usr/local/sbin/canter-apt\n' > /etc/sudoers.d/canter-apt
chmod 440 /etc/sudoers.d/canter-apt
visudo -cf /etc/sudoers.d/canter-apt
touch /opt/canter/sandbox
`
	}
	if config.Desktop {
		script += `cat > /opt/canter/desktop.sh <<'CANTER_DESKTOP'
#!/bin/bash
set -euo pipefail
export DISPLAY=:99
Xvfb :99 -screen 0 1280x800x24 -nolisten tcp &
for attempt in $(seq 1 30); do xdpyinfo -display :99 >/dev/null 2>&1 && break; sleep 1; done
export XCURSOR_THEME=DMZ-White
export XCURSOR_SIZE=24
printf 'Xcursor.theme: DMZ-White\nXcursor.size: 24\n' | xrdb -merge
xsetroot -solid '#24272b' -cursor_name left_ptr
openbox >/dev/null 2>&1 &
canter_wm_pid=$!
for attempt in $(seq 1 30); do xprop -root _NET_SUPPORTING_WM_CHECK 2>/dev/null | grep -q 'window id' && break; sleep 0.1; done
google-chrome --user-data-dir=/workspace/.browser --no-first-run --no-default-browser-check --disable-dev-shm-usage --password-store=basic --start-maximized --window-size=1280,800 about:blank >/dev/null 2>&1 &
# Keep the desktop service alive when Chrome is deliberately quiesced for a
# profile checkpoint. Browser exit must not restart the X server underneath it.
wait "$canter_wm_pid"
CANTER_DESKTOP
chmod 755 /opt/canter/desktop.sh
cat > /etc/systemd/system/canter-desktop.service <<'CANTER_UNIT'
[Unit]
Description=Private Wren desktop
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
User=wren
Group=wren
Environment=HOME=/home/wren
Environment=DISPLAY=:99
WorkingDirectory=/workspace
ExecStart=/opt/canter/desktop.sh
Restart=always
RestartSec=3
KillMode=control-group
[Install]
WantedBy=multi-user.target
CANTER_UNIT
`
	}
	if !config.Desktop || config.Sandbox {
		script += `mkdir -p /etc/systemd/resolved.conf.d
printf '[Resolve]\nDNS=1.1.1.1 9.9.9.9\nDomains=~.\n' > /etc/systemd/resolved.conf.d/canter.conf
systemctl restart systemd-resolved
`
	}
	script += `printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\nAllowUsers wren\nAllowTcpForwarding no\nX11Forwarding no\nPermitTunnel no\n' > /etc/ssh/sshd_config.d/00-canter-computer.conf
ufw default deny incoming
`
	if config.Egress == "" || config.Egress == "standard" {
		script += `ufw default allow outgoing
`
	}
	script += `ufw allow from ` + source.Masked().String() + ` to any port 22 proto tcp
# A guest must never use its metadata endpoint to retrieve bootstrap material.
ufw deny out to 169.254.169.254
`
	if config.Egress == "restricted" || config.Egress == "standard" {
		script += `ufw deny out to 10.0.0.0/8
ufw deny out to 172.16.0.0/12
ufw deny out to 192.168.0.0/16
ufw deny out to 100.64.0.0/10
ufw deny out to any port 25 proto tcp
`
	}
	if config.Egress == "restricted" {
		script += `ufw default deny outgoing
ufw allow out to any port 80 proto tcp
ufw allow out to any port 443 proto tcp
ufw allow out to any port 53
ufw allow out to any port 123 proto udp
`
	}
	script += `ufw --force enable
install -d -m 755 -o root -g root /run/sshd
sshd -t
systemctl daemon-reload
`
	if config.Desktop {
		script += `systemctl enable --now canter-desktop.service
`
	}
	script += `systemctl restart ssh
systemctl is-active --quiet ssh
`
	if config.Desktop {
		script += `systemctl is-active --quiet canter-desktop.service
`
	}
	script += `python3 -m py_compile /opt/canter/computer-agent.py
python3 -m py_compile /opt/canter/sandbox-snapshot.py
) > "$canter_computer_install_log" 2>&1
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256 | awk '{print $2}'
`
	if len(script) > 60000 {
		return "", fmt.Errorf("computer bootstrap exceeds safe user-data size")
	}
	return strings.TrimSpace(script), nil
}

// ImageBuildBootstrap is the user-data script for building a prebuilt
// sandbox image: the full headless package set, the wren account layout, and
// canter-apt, but no per-sandbox material (no keys, helper, CIDR or egress
// rules). It ends by cleaning the machine for imaging and powering off.
func ImageBuildBootstrap(version string) string {
	aptHelper := base64.StdEncoding.EncodeToString(canterApt)
	return `#!/bin/bash
canter_computer_install_log=/var/log/canter-computer-install.log
trap 'canter_computer_rc=$?; if [ "$canter_computer_rc" -ne 0 ]; then tail -c 1800 "$canter_computer_install_log" >&2; fi' EXIT
(
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends python3 python-is-python3 ` + bootstrapSandboxPackages + ` openssh-server ufw ca-certificates
id wren >/dev/null 2>&1 || useradd -m -s /bin/bash wren
passwd -l wren >/dev/null
install -d -m 700 -o wren -g wren /home/wren/.ssh /home/wren/.canter
install -d -m 700 -o wren -g wren /workspace
install -d -m 755 -o root -g root /opt/canter
printf '%s' '` + aptHelper + `' | base64 -d > /usr/local/sbin/canter-apt
chown root:root /usr/local/sbin/canter-apt
chmod 755 /usr/local/sbin/canter-apt
printf 'wren ALL=(root) NOPASSWD: /usr/local/sbin/canter-apt\n' > /etc/sudoers.d/canter-apt
chmod 440 /etc/sudoers.d/canter-apt
visudo -cf /etc/sudoers.d/canter-apt
touch /opt/canter/sandbox
printf '%s\n' '` + version + `' > /opt/canter/sandbox-image-version
apt-get clean
if command -v cloud-init >/dev/null; then cloud-init clean --logs --machine-id || true; fi
rm -f /etc/ssh/ssh_host_*
truncate -s 0 /var/log/wtmp /var/log/btmp /var/log/lastlog /var/log/faillog 2>/dev/null || true
) > "$canter_computer_install_log" 2>&1
poweroff
`
}
