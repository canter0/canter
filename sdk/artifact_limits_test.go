package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type boundedArtifactStore struct {
	*fakeStore
	data  []byte
	limit int64
}

func (s *boundedArtifactStore) GetBytesLimited(_ context.Context, _ string, limit int64) ([]byte, error) {
	s.limit = limit
	if int64(len(s.data)) > limit {
		return nil, fmt.Errorf("object exceeds %d bytes", limit)
	}
	return s.data, nil
}

func TestStagedArtifactRejectsInvalidSizeBeforeStorageRead(t *testing.T) {
	client := &Client{}
	for _, size := range []int64{0, -1, maxStagedArtifactBytes + 1, 1<<63 - 1} {
		if err := client.VerifyStagedArtifact(context.Background(), StagedArtifact{Size: size}); err == nil {
			t.Fatalf("accepted artifact size %d", size)
		}
	}
}

func TestStagedArtifactUsesRecordedSizeAsReadBudget(t *testing.T) {
	data := []byte("fixture artifact")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	key, err := ControlPlaneArtifactKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	store := &boundedArtifactStore{fakeStore: newFakeStore(), data: data}
	client := &Client{m1: store}
	artifact := StagedArtifact{Key: key, SHA256: digest, Size: int64(len(data))}
	if err := client.VerifyStagedArtifact(context.Background(), artifact); err != nil {
		t.Fatal(err)
	}
	if store.limit != artifact.Size {
		t.Fatalf("read budget=%d, recorded size=%d", store.limit, artifact.Size)
	}
	store.data = append(store.data, 'x')
	if err := client.VerifyStagedArtifact(context.Background(), artifact); err == nil {
		t.Fatal("accepted an object larger than the recorded artifact")
	}
}

func TestControlPlaneArtifactRejectsOversizedUploadBeforeStorageWrite(t *testing.T) {
	client := &Client{}
	if _, err := client.StageControlPlaneArtifact(context.Background(), make([]byte, maxStagedArtifactBytes+1), "fixture", ""); err == nil {
		t.Fatal("accepted oversized artifact")
	}
}

func TestReleaseArtifactRejectsOversizedEmptyAndNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	for _, fixture := range []struct {
		name string
		size int64
	}{{"empty", 0}, {"oversized", maxReleaseArtifactBytes + 1}, {"valid", 3}} {
		path := filepath.Join(root, fixture.name)
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(fixture.size); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := readReleaseArtifact(path)
		if fixture.name == "valid" {
			if err != nil || len(data) != 3 {
				t.Fatalf("valid file: len=%d err=%v", len(data), err)
			}
		} else if err == nil || data != nil {
			t.Fatalf("invalid file %s: len=%d err=%v", fixture.name, len(data), err)
		}
	}
	if _, err := readReleaseArtifact(root); err == nil {
		t.Fatal("accepted a directory as a release artifact")
	}
}
