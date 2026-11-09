package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const MaxComputerCheckpointBytes = 32<<20 + 128

type ComputerCheckpointObject struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func computerCheckpointKey(scope, digest string) (string, error) {
	if scope == "" {
		return "", fmt.Errorf("checkpoint scope is required")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || digest != hex.EncodeToString(decoded) {
		return "", fmt.Errorf("invalid checkpoint digest")
	}
	bound := sha256.Sum256([]byte(scope))
	return fmt.Sprintf("wren-computers/checkpoints/%x/%s.bin", bound, digest), nil
}
func PlanComputerCheckpointObject(scope string, data []byte) (ComputerCheckpointObject, error) {
	if len(data) < 1 || len(data) > MaxComputerCheckpointBytes {
		return ComputerCheckpointObject{}, fmt.Errorf("checkpoint exceeds storage limit")
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	key, err := computerCheckpointKey(scope, digest)
	if err != nil {
		return ComputerCheckpointObject{}, err
	}
	return ComputerCheckpointObject{Key: key, SHA256: digest, Size: int64(len(data))}, nil
}

// ValidateComputerCheckpointObject checks an object's namespace and bounded
// descriptor without downloading it. Get additionally verifies its bytes.
func ValidateComputerCheckpointObject(scope string, object ComputerCheckpointObject) error {
	key, err := computerCheckpointKey(scope, object.SHA256)
	if err != nil || key != object.Key || object.Size < 1 || object.Size > MaxComputerCheckpointBytes {
		return fmt.Errorf("checkpoint does not match its scope or size")
	}
	return nil
}
func (c *Client) PutComputerCheckpoint(ctx context.Context, scope string, data []byte) (ComputerCheckpointObject, error) {
	object, err := PlanComputerCheckpointObject(scope, data)
	if err != nil {
		return object, err
	}
	if err = c.m1.Put(ctx, object.Key, data, "application/octet-stream"); err != nil {
		return ComputerCheckpointObject{}, fmt.Errorf("checkpoint upload failed")
	}
	return object, nil
}
func (c *Client) GetComputerCheckpoint(ctx context.Context, scope string, object ComputerCheckpointObject) ([]byte, error) {
	key, err := computerCheckpointKey(scope, object.SHA256)
	if err != nil || key != object.Key || object.Size < 1 || object.Size > MaxComputerCheckpointBytes {
		return nil, fmt.Errorf("checkpoint does not match its scope or size")
	}
	data, err := c.readArtifactBytes(ctx, key, object.Size)
	if err != nil {
		return nil, fmt.Errorf("checkpoint download failed")
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != object.Size || hex.EncodeToString(sum[:]) != object.SHA256 {
		return nil, fmt.Errorf("checkpoint storage verification failed")
	}
	return data, nil
}
func (c *Client) DeleteComputerCheckpoint(ctx context.Context, scope string, object ComputerCheckpointObject) error {
	key, err := computerCheckpointKey(scope, object.SHA256)
	if err != nil || key != object.Key {
		return fmt.Errorf("checkpoint does not match its scope")
	}
	store, ok := c.m1.(interface {
		Delete(context.Context, string) error
	})
	if !ok {
		return fmt.Errorf("checkpoint erasure unavailable")
	}
	return store.Delete(ctx, key)
}
