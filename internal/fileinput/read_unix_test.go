//go:build unix

package fileinput

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenDoesNotWaitForFIFOReplacedAfterStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("valid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		file, err := openInput(path, false)
		if err == nil {
			defer file.Close()
			info, statErr := file.Stat()
			if statErr != nil {
				err = statErr
			} else {
				err = validate(info, path, 8)
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("replacement FIFO accepted")
		}
	case <-time.After(time.Second):
		// Unblock a regressed blocking open before reporting the failure.
		if writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			writer.Close()
		}
		t.Fatal("opening replacement FIFO waited for a writer")
	}
}

func TestSelectedSymlinksAndCredentialNoFollow(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(path, []byte("valid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadRegular(link, 8); err != nil || string(data) != "valid" {
		t.Fatalf("selected regular symlink: %q %v", data, err)
	}
	if _, err := ReadRegularNoFollow(link, 8); err == nil {
		t.Fatal("credential symlink accepted")
	}
	if file, err := openInput(link, true); err == nil {
		file.Close()
		t.Fatal("open followed replacement symlink")
	}
}
