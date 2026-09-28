package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	frames := []Frame{
		{Type: FrameStdout, Payload: []byte("hello")},
		{Type: FrameStdinClose},
		{Type: FrameStderr, Payload: bytes.Repeat([]byte{0xff}, 70000)},
	}
	var buf bytes.Buffer
	for _, f := range frames {
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatalf("WriteFrame(%v): %v", f.Type, err)
		}
	}
	for _, want := range frames {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("got frame %v (%d bytes), want %v (%d bytes)", got.Type, len(got.Payload), want.Type, len(want.Payload))
		}
	}
	if _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame on empty stream: got %v, want io.EOF", err)
	}
}

func TestReadFrameRejectsTruncatedFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{Type: FrameStdout, Payload: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	truncated := bytes.NewReader(buf.Bytes()[:buf.Len()-2])
	if _, err := ReadFrame(truncated); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadFrameRejectsOversizedPayload(t *testing.T) {
	header := make([]byte, 5)
	header[0] = byte(FrameStdout)
	binary.BigEndian.PutUint32(header[1:], MaxPayload+1)
	if _, err := ReadFrame(bytes.NewReader(header)); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("got %v, want ErrPayloadTooLarge", err)
	}
}

func TestWriteFrameRejectsOversizedPayload(t *testing.T) {
	err := WriteFrame(io.Discard, Frame{Type: FrameStdout, Payload: make([]byte, MaxPayload+1)})
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("got %v, want ErrPayloadTooLarge", err)
	}
}

func TestReadFrameRejectsUnknownType(t *testing.T) {
	header := []byte{0xee, 0, 0, 0, 0}
	if _, err := ReadFrame(bytes.NewReader(header)); !errors.Is(err, ErrUnknownFrameType) {
		t.Fatalf("got %v, want ErrUnknownFrameType", err)
	}
}

func TestJSONFrameRoundTrip(t *testing.T) {
	want := Request{Version: Version, Argv: []string{"git", "push"}, Cwd: "/workspaces/app"}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, FrameRequest, want); err != nil {
		t.Fatal(err)
	}
	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != FrameRequest {
		t.Fatalf("got type %v, want FrameRequest", f.Type)
	}
	var got Request
	if err := DecodeJSON(f, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDecodeJSONRejectsMalformedPayload(t *testing.T) {
	var exit Exit
	if err := DecodeJSON(Frame{Type: FrameExit, Payload: []byte("{not json")}, &exit); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}
