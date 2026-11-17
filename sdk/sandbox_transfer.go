package sdk

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

type SandboxTransferEntry struct {
	Path       string `json:"path"`
	Directory  bool   `json:"directory,omitempty"`
	Executable bool   `json:"executable,omitempty"`
	Size       int64  `json:"size,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Version    string `json:"version,omitempty"`
}

type SandboxTransferExclusion struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type SandboxTransferManifest struct {
	Entries    []SandboxTransferEntry     `json:"entries"`
	Exclusions []SandboxTransferExclusion `json:"exclusions,omitempty"`
}

type SandboxTransferRequest struct {
	Direction        string `json:"direction"`
	Path             string `json:"path"`
	Overwrite        bool   `json:"overwrite"`
	IncludeSensitive bool   `json:"includeSensitive"`
	SandboxTransferManifest
}

type SandboxTransferLimits struct {
	Files         int   `json:"files"`
	Bytes         int64 `json:"bytes"`
	ChunkBytes    int   `json:"chunkBytes"`
	MetadataBytes int   `json:"metadataBytes"`
}

type SandboxTransferFileReceipt struct {
	State   string `json:"state"`
	Offset  int64  `json:"offset"`
	Message string `json:"message,omitempty"`
}

type SandboxTransfer struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	SandboxID   string `json:"sandboxId"`
	Generation  int    `json:"generation"`
	State       string `json:"state"`
	TotalBytes  int64  `json:"totalBytes"`
	SandboxTransferRequest
	Destinations       []string                           `json:"destinationVersions,omitempty"`
	DestinationBinding string                             `json:"destinationBinding,omitempty"`
	Collisions         []int                              `json:"collisions,omitempty"`
	Receipts           map[int]SandboxTransferFileReceipt `json:"receipts"`
}

func SandboxTransferRelativePath(name string, includeSensitive bool) error {
	if !utf8.ValidString(name) || len(name) == 0 || len(name) > 4096 || strings.ContainsAny(name, "\x00\\") || path.Clean(name) != name || strings.HasPrefix(name, "/") {
		return fmt.Errorf("invalid transfer path %q", name)
	}
	parts := strings.Split(name, "/")
	if len(parts) > 64 {
		return fmt.Errorf("transfer path is nested more than 64 directories")
	}
	for _, part := range parts {
		if len(part) > 255 || part == "." || part == ".." || SandboxTransferPrivateName(part) {
			return fmt.Errorf("unsupported transfer path %q", name)
		}
		if !includeSensitive && SandboxTransferSensitiveName(part) {
			return fmt.Errorf("sensitive transfer path %q requires explicit inclusion", name)
		}
	}
	return nil
}

func SandboxTransferPrivateName(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".canter", ".browser", ".ssh":
		return true
	}
	return strings.HasPrefix(name, ".canter-transfer-")
}

func SandboxTransferSensitiveName(name string) bool {
	name = strings.ToLower(name)
	switch name {
	case ".env", ".netrc", ".npmrc", ".pypirc", "credentials.json", "id_rsa", "id_ed25519":
		return true
	}
	return strings.HasPrefix(name, ".env.") || strings.HasSuffix(name, ".pem") ||
		strings.HasSuffix(name, ".key") || strings.HasSuffix(name, ".p12") || strings.HasSuffix(name, ".pfx")
}
