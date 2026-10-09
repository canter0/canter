package fileinput

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestRegularInputBoundaries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input")
	for _, size := range []int{0, 8, 9} {
		want := bytes.Repeat([]byte("x"), size)
		if err := os.WriteFile(path, want, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadRegular(path, 8)
		if size > 8 {
			if err == nil || got != nil {
				t.Fatal("oversized input accepted")
			}
		} else if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("size %d: data=%q error=%v", size, got, err)
		}
	}
	if _, err := ReadRegular(root, 8); err == nil {
		t.Fatal("directory accepted")
	}
	for _, limit := range []int64{0, -1, math.MaxInt64} {
		if _, err := ReadRegular(path, limit); err == nil {
			t.Fatalf("invalid limit %d accepted", limit)
		}
	}
}
