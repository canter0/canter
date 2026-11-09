package wrencomputer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
)

const sandboxSnapshotMagic = "CNSNAP2\n"
const sandboxSnapshotFrameBytes = 128 << 10
const sandboxSnapshotReceiptBytes = 16 << 10

type snapshotFrameReader struct {
	source    io.Reader
	options   SandboxSnapshotOptions
	remaining uint32
	count     int64
	hash      hash.Hash
	done      bool
	receipt   SandboxSnapshotStats
}

func snapshotMagic(source io.Reader) error {
	var magic [len(sandboxSnapshotMagic)]byte
	if _, err := io.ReadFull(source, magic[:]); err != nil {
		return fmt.Errorf("incomplete snapshot header: %w", err)
	}
	if string(magic[:]) != sandboxSnapshotMagic {
		return fmt.Errorf("invalid snapshot protocol")
	}
	return nil
}

func newSnapshotFrameReader(source io.Reader, options SandboxSnapshotOptions) (*snapshotFrameReader, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	if err := snapshotMagic(source); err != nil {
		return nil, err
	}
	return &snapshotFrameReader{source: source, options: options, hash: sha256.New()}, nil
}

func snapshotFrameHeader(source io.Reader) (byte, uint32, error) {
	var header [5]byte
	if _, err := io.ReadFull(source, header[:]); err != nil {
		return 0, 0, fmt.Errorf("incomplete snapshot frame: %w", err)
	}
	return header[0], binary.BigEndian.Uint32(header[1:]), nil
}

func snapshotReceipt(source io.Reader, size uint32, options SandboxSnapshotOptions) (SandboxSnapshotStats, error) {
	var receipt SandboxSnapshotStats
	if size < 1 || size > sandboxSnapshotReceiptBytes {
		return receipt, fmt.Errorf("invalid snapshot receipt size")
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(source, data); err != nil {
		return receipt, fmt.Errorf("incomplete snapshot receipt: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("invalid snapshot receipt: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return receipt, fmt.Errorf("snapshot receipt has trailing data")
	}
	digest, err := hex.DecodeString(receipt.ArchiveSHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != receipt.ArchiveSHA256 ||
		receipt.Format != SandboxSnapshotFormat || receipt.IncludeBrowser != options.IncludeBrowser ||
		receipt.RawBytes < 0 || receipt.RawBytes > options.MaxUnpackedBytes ||
		receipt.Entries < 2 || receipt.Entries > options.MaxEntries ||
		receipt.RuntimeSkipped < 0 || receipt.RuntimeSkipped > options.MaxEntries-receipt.Entries ||
		receipt.CompressedBytes < 1 || receipt.CompressedBytes > options.compressedLimit() {
		return receipt, fmt.Errorf("snapshot receipt exceeds its contract")
	}
	return receipt, nil
}

// ValidateSandboxSnapshotStats checks the bounded receipt contract without
// reading an archive. Archive bytes still require ValidateSandboxArchive.
func ValidateSandboxSnapshotStats(stats SandboxSnapshotStats, options SandboxSnapshotOptions) error {
	if err := options.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	_, err = snapshotReceipt(bytes.NewReader(data), uint32(len(data)), options)
	return err
}

func snapshotEnd(source io.Reader) error {
	var extra [1]byte
	if n, err := io.ReadFull(source, extra[:]); n != 0 || err != io.EOF {
		return fmt.Errorf("snapshot response has trailing data or did not end cleanly")
	}
	return nil
}

func (r *snapshotFrameReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if r.done {
		return 0, io.EOF
	}
	if r.remaining == 0 {
		kind, size, err := snapshotFrameHeader(r.source)
		if err != nil {
			return 0, err
		}
		switch kind {
		case 1:
			if size < 1 || size > sandboxSnapshotFrameBytes || r.count+int64(size) > r.options.compressedLimit() {
				return 0, fmt.Errorf("invalid snapshot data frame size")
			}
			r.remaining = size
		case 2:
			receipt, err := snapshotReceipt(r.source, size, r.options)
			if err != nil {
				return 0, err
			}
			if receipt.CompressedBytes != r.count || receipt.ArchiveSHA256 != hex.EncodeToString(r.hash.Sum(nil)) {
				return 0, fmt.Errorf("snapshot stream does not match its receipt")
			}
			if err = snapshotEnd(r.source); err != nil {
				return 0, err
			}
			r.receipt, r.done = receipt, true
			return 0, io.EOF
		default:
			return 0, fmt.Errorf("invalid snapshot frame kind")
		}
	}
	n, err := r.source.Read(buffer[:min(len(buffer), int(r.remaining))])
	r.remaining -= uint32(n)
	r.count += int64(n)
	_, _ = r.hash.Write(buffer[:n])
	if err == io.EOF {
		err = io.ErrUnexpectedEOF // the mandatory receipt has not arrived
	}
	return n, err
}

func sameSnapshotStats(actual, expected SandboxSnapshotStats) bool {
	return actual == expected
}

func validateSnapshotStream(frames *snapshotFrameReader, destination io.Writer) (SandboxSnapshotStats, error) {
	stats, err := ValidateSandboxArchive(io.TeeReader(frames, destination), frames.options)
	if err != nil {
		return stats, err
	}
	if !frames.done {
		return stats, fmt.Errorf("snapshot receipt was not consumed")
	}
	// Omitted sockets/FIFOs are reported by the trusted exporter, not the tar.
	stats.RuntimeSkipped = frames.receipt.RuntimeSkipped
	if !sameSnapshotStats(stats, frames.receipt) {
		return stats, fmt.Errorf("snapshot archive contents do not match the receipt")
	}
	return stats, nil
}

func readSnapshotRestoreReceipt(source io.Reader, options SandboxSnapshotOptions, expected SandboxSnapshotStats) error {
	if err := snapshotMagic(source); err != nil {
		return err
	}
	kind, size, err := snapshotFrameHeader(source)
	if err != nil {
		return err
	}
	if kind != 2 {
		return fmt.Errorf("unexpected snapshot restore output")
	}
	actual, err := snapshotReceipt(source, size, options)
	if err != nil {
		return err
	}
	if !sameSnapshotStats(actual, expected) {
		return fmt.Errorf("snapshot restore receipt does not match")
	}
	return snapshotEnd(source)
}
