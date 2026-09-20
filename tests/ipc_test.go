package tests

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"miniav/pkg/ipc"
	"miniav/pkg/protocol"
)

func TestIPCEncoderDecoderRoundTripMultipleFrames(t *testing.T) {
	messages := []protocol.Message{
		{Version: protocol.Version, Type: protocol.TypeHello},
		{Version: protocol.Version, Type: protocol.TypeScan, RequestID: 101, FilePath: `C:\path\file.txt`},
		{Version: protocol.Version, Type: protocol.TypeShutdown},
	}
	var stream bytes.Buffer
	encoder := ipc.NewEncoder(&stream)
	for _, message := range messages {
		if err := encoder.Encode(message); err != nil {
			t.Fatal(err)
		}
	}

	decoder := ipc.NewDecoder(&stream)
	for _, want := range messages {
		got, err := decoder.Decode()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decoded = %#v, want %#v", got, want)
		}
	}
	if _, err := decoder.Decode(); !errors.Is(err, io.EOF) {
		t.Fatalf("final error = %v, want EOF", err)
	}
}

func TestIPCDecoderRejectsInvalidFrames(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   string
	}{
		{name: "unterminated", stream: `{"version":1,"type":"HELLO"}`, want: ipc.ErrUnterminatedFrame.Error()},
		{name: "malformed", stream: "not-json\n", want: "decode protocol frame"},
		{name: "invalid message", stream: `{"version":2,"type":"HELLO"}` + "\n", want: "protocol version"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ipc.NewDecoder(strings.NewReader(test.stream)).Decode()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestIPCDecoderRejectsOversizedFrame(t *testing.T) {
	stream := strings.Repeat("x", ipc.MaxFrameSize+1) + "\n"
	decoder := ipc.NewDecoder(strings.NewReader(stream))
	if _, err := decoder.Decode(); !errors.Is(err, ipc.ErrFrameTooLarge) {
		t.Fatalf("error = %v, want ErrFrameTooLarge", err)
	}
	if _, err := decoder.Decode(); !errors.Is(err, ipc.ErrFrameTooLarge) {
		t.Fatalf("second error = %v, want terminal ErrFrameTooLarge", err)
	}
}

func TestIPCEncoderRejectsOversizedFrame(t *testing.T) {
	message := protocol.Message{
		Version:   protocol.Version,
		Type:      protocol.TypeScan,
		RequestID: 1,
		FilePath:  strings.Repeat("x", ipc.MaxFrameSize),
	}
	if err := ipc.NewEncoder(io.Discard).Encode(message); !errors.Is(err, ipc.ErrFrameTooLarge) {
		t.Fatalf("error = %v, want ErrFrameTooLarge", err)
	}
}

type ipcShortWriter struct {
	buffer bytes.Buffer
}

func (writer *ipcShortWriter) Write(data []byte) (int, error) {
	if len(data) > 3 {
		data = data[:3]
	}
	return writer.buffer.Write(data)
}

func TestIPCEncoderCompletesShortWrites(t *testing.T) {
	written := &ipcShortWriter{}
	message := protocol.Message{Version: protocol.Version, Type: protocol.TypeHello}
	if err := ipc.NewEncoder(written).Encode(message); err != nil {
		t.Fatal(err)
	}
	if written.buffer.String() != `{"version":1,"type":"HELLO"}`+"\n" {
		t.Fatalf("frame = %q", written.buffer.String())
	}
}
