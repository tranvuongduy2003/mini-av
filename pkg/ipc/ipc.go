package ipc

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"miniav/pkg/protocol"
)

const MaxFrameSize = 64 * 1024

var (
	ErrFrameTooLarge     = errors.New("protocol frame exceeds size limit")
	ErrUnterminatedFrame = errors.New("protocol frame is not newline terminated")
)

type Decoder struct {
	reader *bufio.Reader
	failed error
}

type Encoder struct {
	writer io.Writer
}

func NewDecoder(reader io.Reader) *Decoder {
	return &Decoder{reader: bufio.NewReaderSize(reader, MaxFrameSize+1)}
}

func NewEncoder(writer io.Writer) *Encoder {
	return &Encoder{writer: writer}
}

func (decoder *Decoder) Decode() (protocol.Message, error) {
	if decoder.failed != nil {
		return protocol.Message{}, decoder.failed
	}

	frame, err := decoder.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		decoder.failed = ErrFrameTooLarge
		return protocol.Message{}, decoder.failed
	}
	if errors.Is(err, io.EOF) {
		if len(frame) == 0 {
			return protocol.Message{}, io.EOF
		}
		decoder.failed = ErrUnterminatedFrame
		return protocol.Message{}, decoder.failed
	}
	if err != nil {
		return protocol.Message{}, fmt.Errorf("read protocol frame: %w", err)
	}

	payload := frame[:len(frame)-1]
	if len(payload) > MaxFrameSize {
		decoder.failed = ErrFrameTooLarge
		return protocol.Message{}, decoder.failed
	}
	message, err := protocol.Unmarshal(payload)
	if err != nil {
		return protocol.Message{}, fmt.Errorf("decode protocol frame: %w", err)
	}
	return message, nil
}

func (encoder *Encoder) Encode(message protocol.Message) error {
	payload, err := protocol.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode protocol frame: %w", err)
	}
	if len(payload) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	frame := make([]byte, len(payload)+1)
	copy(frame, payload)
	frame[len(payload)] = '\n'
	if err := writeAll(encoder.writer, frame); err != nil {
		return fmt.Errorf("write protocol frame: %w", err)
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
