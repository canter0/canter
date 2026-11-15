package wrencomputer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"testing"
)

func screenPacket(t *testing.T) []byte {
	t.Helper()
	var picture bytes.Buffer
	if err := jpeg.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 3, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return picture.Bytes()
}
func TestScreenFramesBoundariesAndTermination(t *testing.T) {
	frame := screenPacket(t)
	var source bytes.Buffer
	source.WriteString(screenMagic)
	for i := 0; i < 3; i++ {
		binary.Write(&source, binary.BigEndian, uint32(len(frame)))
		source.Write(frame)
	}
	binary.Write(&source, binary.BigEndian, uint32(0))
	seen := 0
	if err := readScreenFrames(&source, func(raw []byte) error {
		seen++
		if !bytes.Equal(raw, frame) {
			t.Fatal("changed image")
		}
		return nil
	}); err != nil || seen != 3 {
		t.Fatalf("frames=%d err=%v", seen, err)
	}
}
func TestScreenRejectsUnboundedMalformedAndTruncatedFrames(t *testing.T) {
	cases := [][]byte{bytes.Repeat([]byte("x"), 100000), []byte(screenMagic), append([]byte(screenMagic), 0xff, 0xff, 0xff, 0xff), append([]byte(screenMagic), 0, 0, 0, 4, 1, 2)}
	for _, data := range cases {
		if err := readScreenFrames(bytes.NewReader(data), func([]byte) error { t.Fatal("invalid frame delivered"); return nil }); err == nil {
			t.Fatal("invalid feed accepted")
		}
	}
}
func TestScreenOldGuestAndViewerCancellation(t *testing.T) {
	err := readScreenFrames(bytes.NewBufferString(`{"ok":false,"error":{"code":"invalid_operation"}}`+"\n"), func([]byte) error { return nil })
	if !errors.Is(err, ErrScreenUnsupported) {
		t.Fatal(err)
	}
	frame := screenPacket(t)
	var source bytes.Buffer
	source.WriteString(screenMagic)
	binary.Write(&source, binary.BigEndian, uint32(len(frame)))
	source.Write(frame)
	if err = readScreenFrames(&source, func([]byte) error { return io.ErrClosedPipe }); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}
