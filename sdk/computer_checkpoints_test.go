package sdk

import (
	"bytes"
	"context"
	"testing"
)

type checkpointObjectTestStore struct {
	*fakeStore
	deleted string
}

func (s *checkpointObjectTestStore) Delete(_ context.Context, key string) error {
	s.deleted = key
	return nil
}
func TestComputerCheckpointObjectsScopeDigestAndErasure(t *testing.T) {
	ctx := context.Background()
	store := &checkpointObjectTestStore{fakeStore: newFakeStore()}
	client := &Client{m1: store}
	data := []byte("opaque encrypted checkpoint fixture")
	object, err := client.PutComputerCheckpoint(ctx, "workspace-a\x00private-room", data)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := client.GetComputerCheckpoint(ctx, "workspace-a\x00private-room", object)
	if err != nil || !bytes.Equal(saved, data) {
		t.Fatalf("roundtrip: %v", err)
	}
	if _, err = client.GetComputerCheckpoint(ctx, "workspace-b\x00private-room", object); err == nil {
		t.Fatal("cross-scope checkpoint read")
	}
	if err = client.DeleteComputerCheckpoint(ctx, "workspace-b\x00private-room", object); err == nil || store.deleted != "" {
		t.Fatal("cross-scope checkpoint erasure")
	}
	altered := bytes.Clone(data)
	altered[0] ^= 1
	store.Put(ctx, object.Key, altered, "application/octet-stream")
	if _, err = client.GetComputerCheckpoint(ctx, "workspace-a\x00private-room", object); err == nil {
		t.Fatal("tampered checkpoint accepted")
	}
	if err = client.DeleteComputerCheckpoint(ctx, "workspace-a\x00private-room", object); err != nil || store.deleted != object.Key {
		t.Fatalf("owned erasure: %v", err)
	}
	if _, err = PlanComputerCheckpointObject("", data); err == nil {
		t.Fatal("unscoped object accepted")
	}
}
