//go:build unix

package fileinput

import (
	"os"
	"syscall"
)

func openInput(path string, noFollow bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if noFollow {
		flags |= syscall.O_NOFOLLOW
	}
	return os.OpenFile(path, flags, 0)
}
