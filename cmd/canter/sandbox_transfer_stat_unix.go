//go:build unix

package main

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"syscall"
)

func transferHardLinks(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 0
}

func transferLocalFingerprint(info fs.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	raw := fmt.Sprintf("%d/%d/%d/%d/%d/%d", stat.Dev, stat.Ino, stat.Nlink, info.Size(), info.Mode(), info.ModTime().UnixNano())
	return fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
}
