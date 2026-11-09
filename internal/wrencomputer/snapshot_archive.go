package wrencomputer

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const SandboxSnapshotFormat = "canter-sandbox-files-v2"
const SandboxSnapshotMaxEntries = 1_000_000
const sandboxSnapshotMaxDepth = 256
const sandboxSnapshotMaxPath = 4096

// Limits apply to file contents, including dependencies, rather than imposing
// the old 32 MiB archive limit. Callers must select the allocated disk's bound.
type SandboxSnapshotOptions struct {
	MaxUnpackedBytes int64 `json:"maxUnpackedBytes"`
	MaxEntries       int   `json:"maxEntries"`
	IncludeBrowser   bool  `json:"includeBrowser"`
}

type SandboxSnapshotStats struct {
	Format          string `json:"format"`
	IncludeBrowser  bool   `json:"includeBrowser"`
	RawBytes        int64  `json:"rawBytes"`
	Entries         int    `json:"entries"`
	CompressedBytes int64  `json:"compressedBytes"`
	ArchiveSHA256   string `json:"archiveSha256"`
	RuntimeSkipped  int    `json:"runtimeSkipped"`
}

func (o SandboxSnapshotOptions) validate() error {
	if o.MaxUnpackedBytes < 1 || o.MaxUnpackedBytes > 1<<40 || o.MaxEntries < 2 || o.MaxEntries > SandboxSnapshotMaxEntries {
		return fmt.Errorf("invalid sandbox snapshot limits")
	}
	return nil
}

func (o SandboxSnapshotOptions) compressedLimit() int64 {
	// Allow incompressible deflate overhead and bounded PAX/path metadata.
	return o.MaxUnpackedBytes + o.MaxUnpackedBytes/100 + int64(o.MaxEntries)*16384 + (1 << 20)
}

func snapshotProtected(name string, browser bool) bool {
	for _, prefix := range []string{"home/.ssh", "home/.canter", "workspace/.canter"} {
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			return true
		}
	}
	if !browser {
		for _, prefix := range []string{"home/.browser", "workspace/.browser"} {
			if name == prefix || strings.HasPrefix(name, prefix+"/") {
				return true
			}
		}
	}
	return false
}

func snapshotCanonical(name string, browser bool) bool {
	if name == "" || len(name) > sandboxSnapshotMaxPath || !utf8.ValidString(name) ||
		strings.ContainsRune(name, 0) || path.Clean(name) != name ||
		snapshotProtected(name, browser) {
		return false
	}
	parts := strings.Split(name, "/")
	return len(parts) <= sandboxSnapshotMaxDepth && (parts[0] == "home" || parts[0] == "workspace")
}

// Component ordering permits a depth-first walk while making duplicates
// detectable without retaining every full path in memory.
func snapshotPathLess(a, b string) bool {
	left, right := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < min(len(left), len(right)); i++ {
		if left[i] != right[i] {
			return left[i] < right[i]
		}
	}
	return len(left) < len(right)
}

type snapshotCountReader struct {
	io.Reader
	count int64
}

func (r *snapshotCountReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.count += int64(n)
	return n, err
}

// ValidateSandboxArchive consumes exactly one gzip/tar stream, including its
// checksum and padding. It never extracts paths or allocates a complete file.
// Only hashes of regular-file names are retained for hardlink validation.
func ValidateSandboxArchive(source io.Reader, options SandboxSnapshotOptions) (SandboxSnapshotStats, error) {
	stats := SandboxSnapshotStats{Format: SandboxSnapshotFormat, IncludeBrowser: options.IncludeBrowser}
	if err := options.validate(); err != nil {
		return stats, err
	}
	hash := sha256.New()
	compressed := &snapshotCountReader{Reader: io.TeeReader(io.LimitReader(source, options.compressedLimit()+1), hash)}
	buffered := bufio.NewReader(compressed)
	zipped, err := gzip.NewReader(buffered)
	if err != nil {
		return stats, fmt.Errorf("invalid snapshot gzip: %w", err)
	}
	defer zipped.Close()
	zipped.Multistream(false)
	plainLimit := options.MaxUnpackedBytes + int64(options.MaxEntries)*16384 + 10240
	plain := &snapshotCountReader{Reader: io.LimitReader(zipped, plainLimit+1)}
	archive := tar.NewReader(plain)
	type regular struct {
		mode, modified int64
	}
	regulars := map[[32]byte]regular{}
	roots := map[string]bool{}
	var directories []string
	previous := ""
	for {
		header, nextErr := archive.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return stats, fmt.Errorf("invalid snapshot tar: %w", nextErr)
		}
		stats.Entries++
		if stats.Entries > options.MaxEntries || plain.count > plainLimit {
			return stats, fmt.Errorf("snapshot entry or metadata limit exceeded")
		}
		for key := range header.PAXRecords {
			switch key {
			case "path", "linkpath", "size", "mtime", "atime", "ctime":
			default:
				return stats, fmt.Errorf("unsupported snapshot metadata")
			}
		}
		name := header.Name
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !snapshotCanonical(name, options.IncludeBrowser) || (previous != "" && !snapshotPathLess(previous, name)) {
			return stats, fmt.Errorf("invalid or unordered snapshot path")
		}
		previous = name
		for len(directories) > 0 && !strings.HasPrefix(name, directories[len(directories)-1]+"/") {
			directories = directories[:len(directories)-1]
		}
		if strings.Contains(name, "/") {
			if len(directories) == 0 || directories[len(directories)-1] != path.Dir(name) {
				return stats, fmt.Errorf("snapshot parent is not a directory")
			}
		} else {
			if header.Typeflag != tar.TypeDir {
				return stats, fmt.Errorf("snapshot root is not a directory")
			}
			roots[name] = true
		}
		modeMask := int64(0777)
		if header.Typeflag == tar.TypeDir {
			modeMask = 01777
		}
		if header.Mode < 0 || header.Mode & ^modeMask != 0 || header.Uid != 0 || header.Gid != 0 || header.Size < 0 {
			return stats, fmt.Errorf("unsafe snapshot ownership, mode or size")
		}
		modified := header.ModTime.UnixNano()
		if !time.Unix(0, modified).Equal(header.ModTime) {
			return stats, fmt.Errorf("snapshot timestamp is out of range")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return stats, fmt.Errorf("snapshot directory contains data")
			}
			directories = append(directories, name)
		case tar.TypeReg, tar.TypeRegA:
			if header.Size > options.MaxUnpackedBytes-stats.RawBytes {
				return stats, fmt.Errorf("snapshot contents exceed their limit")
			}
			stats.RawBytes += header.Size
			regulars[sha256.Sum256([]byte(name))] = regular{mode: header.Mode, modified: modified}
		case tar.TypeLink:
			target, found := regulars[sha256.Sum256([]byte(header.Linkname))]
			if header.Size != 0 || !snapshotCanonical(header.Linkname, options.IncludeBrowser) || !found || target.mode != header.Mode || target.modified != modified {
				return stats, fmt.Errorf("invalid snapshot hardlink")
			}
			regulars[sha256.Sum256([]byte(name))] = target
		case tar.TypeSymlink:
			if header.Size != 0 || header.Linkname == "" || len(header.Linkname) > sandboxSnapshotMaxPath ||
				!utf8.ValidString(header.Linkname) || strings.ContainsRune(header.Linkname, 0) {
				return stats, fmt.Errorf("invalid snapshot symlink")
			}
		default:
			return stats, fmt.Errorf("unsupported snapshot file type")
		}
		if _, err = io.Copy(io.Discard, archive); err != nil {
			return stats, fmt.Errorf("truncated snapshot file: %w", err)
		}
	}
	if !roots["home"] || !roots["workspace"] {
		return stats, fmt.Errorf("snapshot roots are incomplete")
	}
	// tarfile's record padding is permitted; hidden data after the tar end is
	// not. Reading to gzip EOF also verifies its CRC and uncompressed length.
	var padding [1024]byte
	paddingBytes := 0
	for {
		n, readErr := plain.Read(padding[:])
		paddingBytes += n
		if paddingBytes > 10240 || plain.count > plainLimit {
			return stats, fmt.Errorf("snapshot padding limit exceeded")
		}
		for _, value := range padding[:n] {
			if value != 0 {
				return stats, fmt.Errorf("snapshot has data after tar end")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return stats, fmt.Errorf("invalid snapshot checksum: %w", readErr)
		}
	}
	if _, err = buffered.ReadByte(); err != io.EOF || compressed.count > options.compressedLimit() {
		return stats, fmt.Errorf("snapshot has trailing or oversized compressed data")
	}
	stats.CompressedBytes = compressed.count
	stats.ArchiveSHA256 = hex.EncodeToString(hash.Sum(nil))
	return stats, nil
}
