package wrencomputer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"time"

	"golang.org/x/crypto/ssh"
)

var ErrScreenUnsupported = errors.New("this computer uses the earlier screen transport")

const ScreenContentType = "application/x-wren-screen"
const screenMagic = "WREN-SCREEN/1\n"
const maxScreenFrame = 2 << 20

// ScreenStream keeps one pinned SSH channel for a bounded feed. Cancelling this
// viewer closes only its channel, leaving input and other viewers connected.
func (c *Client) captureScreenStream(ctx context.Context, frame func([]byte) error) error {
	ctx, cancel := context.WithTimeout(ctx, 28*time.Second)
	defer cancel()
	client, err := c.connectionFor(ctx)
	if err != nil {
		return err
	}
	broken := false
	defer func() { c.releaseConnection(client, broken) }()
	type opened struct {
		session *ssh.Session
		err     error
	}
	ready := make(chan opened, 1)
	go func() { s, e := client.NewSession(); ready <- opened{s, e} }()
	openCtx, stopOpen := context.WithTimeout(ctx, 8*time.Second)
	defer stopOpen()
	var session *ssh.Session
	select {
	case result := <-ready:
		if result.err != nil {
			broken = true
			return transportError(ctx, result.err)
		}
		session = result.session
	case <-openCtx.Done():
		broken = true
		client.Close()
		result := <-ready
		if result.session != nil {
			result.session.Close()
		}
		return transportError(ctx, openCtx.Err())
	}
	defer session.Close()
	stop := context.AfterFunc(ctx, func() { session.Close() })
	defer stop()
	session.Stdin = bytes.NewBufferString(`{"operation":"screen_stream"}`)
	session.Stderr = io.Discard
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	if err = session.Start("python3 /opt/canter/computer-agent.py"); err != nil {
		return transportError(ctx, err)
	}
	if err = readScreenFrames(stdout, frame); err != nil {
		return err
	}
	return session.Wait()
}

func readScreenFrames(source io.Reader, frame func([]byte) error) error {
	reader := bufio.NewReaderSize(source, 1024)
	header, err := reader.ReadSlice('\n')
	magic := string(header)
	// Bound the handshake too: a guest is untrusted input.
	if len(magic) > 1024 {
		return fmt.Errorf("invalid desktop feed header")
	}
	if magic != screenMagic {
		if bytes.Contains([]byte(magic), []byte(`"invalid_operation"`)) {
			return ErrScreenUnsupported
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("invalid desktop feed header")
	}
	total := 0
	for count := 0; count <= 160; count++ {
		var size uint32
		if err = binary.Read(reader, binary.BigEndian, &size); err != nil {
			return err
		}
		if size == 0 {
			return nil
		}
		if count == 160 || size > maxScreenFrame {
			return fmt.Errorf("desktop feed frame exceeds its limit")
		}
		total += int(size)
		if total > 64<<20 {
			return fmt.Errorf("desktop feed exceeds its limit")
		}
		raw := make([]byte, size)
		if _, err = io.ReadFull(reader, raw); err != nil {
			return err
		}
		config, e := jpeg.DecodeConfig(bytes.NewReader(raw))
		if e != nil || config.Width > 4096 || config.Height > 4096 {
			return fmt.Errorf("invalid desktop feed image")
		}
		if err = frame(raw); err != nil {
			return err
		}
	}
	return fmt.Errorf("desktop feed exceeded its frame limit")
}
