package wrencomputer

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func bootstrapKey(t *testing.T) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

// The desktop script is frozen: sandboxes must not perturb Wren computer
// bootstrap output. Regenerate the golden only with a reviewed bootstrap change.
const desktopGoldenKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDX2kUH6DHHr2R95j9bjQbFilpvTnShzlIsofO/FIcM3 bootstrap-golden"

func TestBootstrapDesktopGolden(t *testing.T) {
	out, err := Bootstrap(BootstrapConfig{SSHPublicKey: desktopGoldenKey, SSHSourceCIDR: "203.0.113.0/24", Desktop: true})
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/bootstrap_desktop.golden")
	if err != nil {
		t.Fatal(err)
	}
	if out != strings.TrimSpace(string(golden)) {
		t.Fatalf("desktop bootstrap changed; diff against testdata/bootstrap_desktop.golden")
	}
}

func TestBootstrapSandbox(t *testing.T) {
	for _, egress := range []string{"restricted", "standard"} {
		out, err := Bootstrap(BootstrapConfig{SSHPublicKey: bootstrapKey(t), SSHSourceCIDR: "203.0.113.0/24", Egress: egress})
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range []string{"python-is-python3", "nodejs", "build-essential", "ufw deny out to 169.254.169.254", "ufw deny out to 10.0.0.0/8", "ufw deny out to 172.16.0.0/12", "ufw deny out to 192.168.0.0/16", "ufw deny out to 100.64.0.0/10", "ufw deny out to any port 25 proto tcp", "AllowUsers wren", "python3 -m py_compile"} {
			if !strings.Contains(out, fragment) {
				t.Errorf("%s bootstrap lacks %s", egress, fragment)
			}
		}
		for _, fragment := range []string{"DNS=1.1.1.1 9.9.9.9", "systemctl restart systemd-resolved"} {
			if !strings.Contains(out, fragment) {
				t.Errorf("%s bootstrap lacks %s", egress, fragment)
			}
		}
		for _, absent := range []string{"xvfb", "openbox", "libreoffice", "google-chrome", "canter-desktop.service"} {
			if strings.Contains(out, absent) {
				t.Errorf("%s bootstrap unexpectedly contains %s", egress, absent)
			}
		}
	}
	// Desktop+Sandbox: GUI environment PLUS sandbox tooling — both layers.
	desktop, err := Bootstrap(BootstrapConfig{SSHPublicKey: bootstrapKey(t), SSHSourceCIDR: "203.0.113.0/24", Desktop: true, Sandbox: true, Egress: "standard"})
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"User=wren", "nodejs", "build-essential", "/opt/canter/sandbox",
		"canter-apt", "ufw deny out to 169.254.169.254", "ufw deny out to any port 25 proto tcp",
		"DNS=1.1.1.1 9.9.9.9", "xvfb", "openbox", "google-chrome",
	} {
		if !strings.Contains(desktop, fragment) {
			t.Errorf("desktop sandbox bootstrap lacks %s", fragment)
		}
	}
	for _, absent := range []string{"PermitRootLogin yes", "AllowTcpForwarding yes", "--no-sandbox", "docker", "x11vnc", "tigervnc"} {
		if strings.Contains(desktop, absent) {
			t.Errorf("desktop sandbox bootstrap unexpectedly contains %s", absent)
		}
	}
	restricted, _ := Bootstrap(BootstrapConfig{SSHPublicKey: bootstrapKey(t), SSHSourceCIDR: "203.0.113.0/24", Egress: "restricted"})
	for _, fragment := range []string{"ufw default deny outgoing", "ufw allow out to any port 80 proto tcp", "ufw allow out to any port 443 proto tcp", "ufw allow out to any port 53", "ufw allow out to any port 123 proto udp"} {
		if !strings.Contains(restricted, fragment) {
			t.Errorf("restricted egress lacks %s", fragment)
		}
	}
	standard, _ := Bootstrap(BootstrapConfig{SSHPublicKey: bootstrapKey(t), SSHSourceCIDR: "203.0.113.0/24", Egress: "standard"})
	if !strings.Contains(standard, "ufw default allow outgoing") || strings.Contains(standard, "ufw default deny outgoing") {
		t.Error("standard egress must keep default allow outgoing")
	}
	legacy, _ := Bootstrap(BootstrapConfig{SSHPublicKey: bootstrapKey(t), SSHSourceCIDR: "203.0.113.0/24"})
	if strings.Contains(legacy, "ufw deny out to 10.0.0.0/8") {
		t.Error("empty egress must preserve the existing Wren policy")
	}
}
