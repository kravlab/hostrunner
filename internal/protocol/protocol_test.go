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

func TestReadFrameAcceptsUpToTheLastFrameType(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader([]byte{byte(FrameStopping), 0, 0, 0, 0})); err != nil {
		t.Fatalf("FrameStopping: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{byte(FrameStopping) + 1, 0, 0, 0, 0})); !errors.Is(err, ErrUnknownFrameType) {
		t.Fatalf("the type after FrameStopping: got %v, want ErrUnknownFrameType", err)
	}
}

// Older daemons and sessions read the frame types by number, so new
// frames are only ever appended.
func TestFrameTypesKeepTheirWireValues(t *testing.T) {
	for typ, want := range map[FrameType]byte{
		FrameRequest: 1, FrameApproved: 15, FrameHello: 16, FrameTakenOver: 30,
		FrameRunning: 31, FrameRunningList: 32, FrameStop: 33, FrameStopping: 34,
	} {
		if byte(typ) != want {
			t.Errorf("frame %v has value %d, want %d", typ, byte(typ), want)
		}
	}
}

func TestDaemonInfoOfOlderDaemonDoesNotStop(t *testing.T) {
	payload := []byte(`{"version":1,"workspace":"/home/u/app","container_workspace":"/workspaces/app","rules_file":"/home/u/app/.devcontainer/hostrun.yaml"}`)
	var info DaemonInfo
	if err := DecodeJSON(Frame{Type: FrameDaemonInfo, Payload: payload}, &info); err != nil {
		t.Fatal(err)
	}
	if info.Stops || info.ContainerStopped {
		t.Fatalf("got Stops=%v ContainerStopped=%v, want both false", info.Stops, info.ContainerStopped)
	}
}

func TestDaemonInfoOfStoppingDaemon(t *testing.T) {
	payload := []byte(`{"version":1,"workspace":"/home/u/app","container_workspace":"/workspaces/app","rules_file":"/home/u/app/.devcontainer/hostrun.yaml","stops":true,"container_stopped":true}`)
	var info DaemonInfo
	if err := DecodeJSON(Frame{Type: FrameDaemonInfo, Payload: payload}, &info); err != nil {
		t.Fatal(err)
	}
	if !info.Stops || !info.ContainerStopped {
		t.Fatalf("got Stops=%v ContainerStopped=%v, want both true", info.Stops, info.ContainerStopped)
	}
}

// The JSON names are the contract between versions, so the payload is
// written out rather than produced by the same struct.
func TestRunningListWireFormat(t *testing.T) {
	payload := []byte(`{"commands":[{"argv":["git","push"],"cwd":"/workspaces/app"}],"standing":2,"session":1}`)
	want := RunningList{
		Commands: []RunningCommand{{Argv: []string{"git", "push"}, Cwd: "/workspaces/app"}},
		Standing: 2,
		Session:  1,
	}
	var got RunningList
	if err := DecodeJSON(Frame{Type: FrameRunningList, Payload: payload}, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestStopWireFormat(t *testing.T) {
	var got Stop
	if err := DecodeJSON(Frame{Type: FrameStop, Payload: []byte(`{"drain":true}`)}, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Drain {
		t.Fatal("got Drain=false, want true")
	}
}
