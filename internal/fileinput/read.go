// Package fileinput reads bounded regular-file inputs without waiting for a
// named-pipe writer if a local process replaces an input during the open.
package fileinput

import (
	"fmt"
	"io"
	"math"
	"os"
)

// ReadRegular follows symlinks to regular files, as ordinary selected input
// paths do. Both metadata and the actual bytes read must fit within limit.
func ReadRegular(path string, limit int64) ([]byte, error) {
	return readRegular(path, limit, false)
}

// ReadRegularNoFollow also rejects a symlink at the final path component.
// On Unix the open itself enforces this, including concurrent replacements.
func ReadRegularNoFollow(path string, limit int64) ([]byte, error) {
	return readRegular(path, limit, true)
}

func readRegular(path string, limit int64, noFollow bool) ([]byte, error) {
	if limit < 1 || limit == math.MaxInt64 {
		return nil, fmt.Errorf("invalid file input limit")
	}
	stat := os.Stat
	if noFollow {
		stat = os.Lstat
	}
	info, err := stat(path)
	if err != nil {
		return nil, err
	}
	if err := validate(info, path, limit); err != nil {
		return nil, err
	}
	file, err := openInput(path, noFollow)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if err := validate(info, path, limit); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds the %d-byte input limit", path, limit)
	}
	return data, nil
}

func validate(info os.FileInfo, path string, limit int64) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", path)
	}
	if info.Size() > limit {
		return fmt.Errorf("%s exceeds the %d-byte input limit", path, limit)
	}
	return nil
}
