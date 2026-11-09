package wrencomputer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"
)

const CheckpointMaxCompressed = 32 << 20
const CheckpointMaxUnpacked = 128 << 20

//go:embed checkpoint.py
var checkpointSource []byte
var checkpointID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,79}$`)

type CheckpointManifest struct {
	Format          string `json:"format"`
	IncludeBrowser  bool   `json:"includeBrowser"`
	RawBytes        int64  `json:"rawBytes"`
	CompressedBytes int64  `json:"compressedBytes"`
	ArchiveSHA256   string `json:"archiveSha256"`
	Entries         []struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"entries"`
	Skipped []string `json:"skippedRebuildablePaths"`
}

func ValidateCheckpoint(data []byte, includeBrowser bool) error {
	if len(data) == 0 || len(data) > CheckpointMaxCompressed {
		return fmt.Errorf("checkpoint compressed size exceeds its limit")
	}
	zip, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("checkpoint is not gzip")
	}
	defer zip.Close()
	source := tar.NewReader(io.LimitReader(zip, CheckpointMaxUnpacked+(8<<20)))
	var total int64
	count := 0
	for {
		entry, e := source.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return fmt.Errorf("invalid checkpoint archive")
		}
		canonical := entry.Name
		if entry.Typeflag == tar.TypeDir {
			canonical = strings.TrimSuffix(canonical, "/")
		}
		name := path.Clean(canonical)
		if (entry.Typeflag != tar.TypeReg && entry.Typeflag != tar.TypeDir) || name != canonical || name == "." || strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") || name == ".canter" || strings.HasPrefix(name, ".canter/") || (!includeBrowser && (name == ".browser" || strings.HasPrefix(name, ".browser/"))) {
			return fmt.Errorf("unsafe checkpoint member")
		}
		total += entry.Size
		count++
		if total > CheckpointMaxUnpacked || count > 20000 {
			return fmt.Errorf("checkpoint unpacked size exceeds its limit")
		}
		if _, e = io.Copy(io.Discard, source); e != nil {
			return fmt.Errorf("truncated checkpoint")
		}
	}
	return nil
}
func (c *Client) checkpointOperation(ctx context.Context, id, action string, includeBrowser bool, parts int, digest string, keepStopped bool) (string, error) {
	if !checkpointID.MatchString(id) {
		return "", fmt.Errorf("invalid checkpoint identity")
	}
	base := "/workspace/.canter/checkpoints/" + id
	request, _ := json.Marshal(map[string]any{"includeBrowser": includeBrowser, "parts": parts, "archiveSha256": digest, "keepBrowserStopped": keepStopped})
	previous, readErr := c.ReadFile(ctx, base+"/request.json", 0, 4096)
	if readErr == nil && !bytes.Equal(previous.Data, request) {
		return "", fmt.Errorf("checkpoint identity already belongs to another operation")
	}
	if readErr != nil {
		var missing *Error
		if !errors.As(readErr, &missing) || (missing.Code != "not_found" && missing.Code != "invalid_file") {
			return "", readErr
		}
	}
	if _, err := c.WriteFile(ctx, "/workspace/.canter/checkpoint-agent.py", checkpointSource); err != nil {
		return "", err
	}
	if _, err := c.WriteFile(ctx, base+"/request.json", request); err != nil {
		return "", err
	}
	job, err := c.StartJob(ctx, JobRequest{ID: "checkpoint_" + action + "_" + id, Command: "python3 /workspace/.canter/checkpoint-agent.py " + action + " " + base, Directory: "/workspace", TimeoutSeconds: 300})
	if err != nil {
		return "", err
	}
	for job.Status == "starting" || job.Status == "running" {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		job, err = c.Job(ctx, job.ID)
		if err != nil {
			return "", err
		}
	}
	if job.Status != "completed" || job.ExitCode == nil || *job.ExitCode != 0 {
		return "", fmt.Errorf("checkpoint %s did not finish; its computer remains allocated", action)
	}
	return base, nil
}
func (c *Client) CreateCheckpoint(ctx context.Context, id string, includeBrowser bool, keepStopped ...bool) ([]byte, CheckpointManifest, error) {
	var manifest CheckpointManifest
	keep := len(keepStopped) > 0 && keepStopped[0]
	base, err := c.checkpointOperation(ctx, id, "create", includeBrowser, 0, "", keep)
	if err != nil {
		return nil, manifest, err
	}
	file, err := c.ReadFile(ctx, base+"/manifest.json", 0, 8<<20)
	if err != nil {
		return nil, manifest, err
	}
	if json.Unmarshal(file.Data, &manifest) != nil {
		return nil, manifest, fmt.Errorf("invalid checkpoint manifest")
	}
	var data []byte
	for offset := int64(0); ; {
		chunk, e := c.ReadFile(ctx, base+"/workspace.tar.gz", offset, 8<<20)
		if e != nil {
			return nil, manifest, e
		}
		if chunk.Size > CheckpointMaxCompressed || len(data)+len(chunk.Data) > CheckpointMaxCompressed {
			return nil, manifest, fmt.Errorf("checkpoint exceeds compressed size limit")
		}
		data = append(data, chunk.Data...)
		if chunk.EOF {
			break
		}
		if chunk.NextOffset <= offset {
			return nil, manifest, fmt.Errorf("checkpoint download made no progress")
		}
		offset = chunk.NextOffset
	}
	sum := sha256.Sum256(data)
	if manifest.ArchiveSHA256 != hex.EncodeToString(sum[:]) || int64(len(data)) != manifest.CompressedBytes {
		return nil, manifest, fmt.Errorf("checkpoint digest verification failed")
	}
	if err = ValidateCheckpoint(data, includeBrowser); err != nil {
		return nil, manifest, err
	}
	return data, manifest, nil
}
func (c *Client) RestoreCheckpoint(ctx context.Context, id string, data []byte, includeBrowser bool) error {
	return c.restoreCheckpoint(ctx, id, data, includeBrowser, "restore")
}

// VerifyCheckpointRestore materializes and verifies the archive in a private
// scratch tree. It never overwrites the live workspace or changes its browser.
func (c *Client) VerifyCheckpointRestore(ctx context.Context, id string, data []byte, includeBrowser bool) error {
	return c.restoreCheckpoint(ctx, id, data, includeBrowser, "verify")
}
func (c *Client) restoreCheckpoint(ctx context.Context, id string, data []byte, includeBrowser bool, action string) error {
	if !checkpointID.MatchString(id) {
		return fmt.Errorf("invalid checkpoint identity")
	}
	if err := ValidateCheckpoint(data, includeBrowser); err != nil {
		return err
	}
	parts := (len(data) + (8 << 20) - 1) / (8 << 20)
	for i := 0; i < parts; i++ {
		end := min(len(data), (i+1)*(8<<20))
		if _, err := c.WriteFile(ctx, fmt.Sprintf("/workspace/.canter/checkpoints/%s/parts/%d", id, i), data[i*(8<<20):end]); err != nil {
			return err
		}
	}
	sum := sha256.Sum256(data)
	_, err := c.checkpointOperation(ctx, id, action, includeBrowser, parts, hex.EncodeToString(sum[:]), false)
	return err
}
