package wrencomputer

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the real SSH helper/channel lifetime with generated fixture pixels;
// this is transport proof, not a claim of native desktop capture on macOS.
func TestScreenSSHSharedCaptureSurvivesOneViewerLeaving(t *testing.T) {
	config, _, calls := helperSSH(t)
	modules := t.TempDir()
	pil := filepath.Join(modules, "PIL")
	if err := os.Mkdir(pil, 0700); err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf("import base64\nclass Picture:\n width=3\n height=2\n def convert(self, mode): return self\n def close(self): pass\n def save(self, stream, **kwargs): stream.write(base64.b64decode(%q))\nclass ImageGrab:\n @staticmethod\n def grab(**kwargs): return Picture()\nclass Image: pass\nclass features:\n @staticmethod\n def check_feature(name): return True\n", base64.StdEncoding.EncodeToString(screenPacket(t)))
	if err := os.WriteFile(filepath.Join(pil, "__init__.py"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYTHONPATH", modules)
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	second, cancelSecond := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSecond()
	firstSeen := make(chan struct{}, 1)
	secondSeen := make(chan struct{}, 8)
	done := make(chan error, 2)
	var count atomic.Int32
	go func() {
		done <- client.ScreenStream(first, func([]byte) error {
			select {
			case firstSeen <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	select {
	case <-firstSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("first viewer got no frame")
	}
	go func() {
		done <- client.ScreenStream(second, func([]byte) error {
			count.Add(1)
			select {
			case secondSeen <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	select {
	case <-secondSeen:
	case <-second.Done():
		t.Fatal("second viewer got no frame")
	}
	cancelFirst()
	<-done
	before := count.Load()
	for count.Load() < before+3 {
		select {
		case <-secondSeen:
		case <-second.Done():
			t.Fatal("first viewer cancellation interrupted sibling")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("viewers created %d capture processes", calls.Load())
	}
	cancelSecond()
	<-done
	client.screenMu.Lock()
	hasFeed := client.screen != nil
	client.screenMu.Unlock()
	if hasFeed {
		t.Fatal("capture retained after last viewer left")
	}
}
