package sdk

import (
	"context"
	"strings"
	"testing"
)

type erasingArtifactStore struct {
	*fakeStore
	deleted []string
}

func (s *erasingArtifactStore) Delete(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	return nil
}

func TestArtifactErasureRejectsNoncanonicalKeys(t *testing.T) {
	store := &erasingArtifactStore{fakeStore: newFakeStore()}
	client := &Client{m1: store}
	digest := strings.Repeat("a", 64)
	key, err := ControlPlaneArtifactKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "systems/production/release.json", "../" + key, key + "/extra", strings.ToUpper(key), "control-plane/artifacts/sha256/not-a-digest.tar.gz"} {
		if err := client.DeleteControlPlaneArtifact(context.Background(), invalid); err == nil {
			t.Fatalf("accepted destructive key %q", invalid)
		}
	}
	if len(store.deleted) != 0 {
		t.Fatal("an invalid key reached object storage")
	}
	if err := client.DeleteControlPlaneArtifact(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != key {
		t.Fatal("canonical artifact was not erased")
	}
}
