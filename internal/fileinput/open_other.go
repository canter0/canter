//go:build !unix

package fileinput

import "os"

func openInput(path string, _ bool) (*os.File, error) {
	return os.Open(path)
}
