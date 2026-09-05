package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

const DefaultMaxEventBytes = 1 << 20

type Event struct {
	Event string
	Data  []byte
}

type Decoder struct {
	reader   *bufio.Reader
	maxBytes int
	// skipLF remembers a CR line ending. If the next byte is LF it belongs to
	// the same CRLF terminator; any other byte starts the next line. Keeping
	// this state avoids peeking (and blocking) after a bare CR.
	skipLF bool
}

func NewDecoder(reader io.Reader, maxBytes int) *Decoder {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxEventBytes
	}
	return &Decoder{reader: bufio.NewReaderSize(reader, 32<<10), maxBytes: maxBytes}
}

func (d *Decoder) Next() (Event, error) {
	var event Event
	var data bytes.Buffer
	seen := false
	for {
		line, err := d.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && seen {
				event.Data = bytes.TrimSuffix(data.Bytes(), []byte{'\n'})
				return event, nil
			}
			return Event{}, err
		}
		if len(line) == 0 {
			if !seen {
				continue
			}
			event.Data = bytes.TrimSuffix(data.Bytes(), []byte{'\n'})
			return event, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := bytes.Cut(line, []byte{':'})
		if found && len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(field) {
		case "event":
			event.Event = string(value)
			seen = true
		case "data":
			if data.Len()+len(value)+1 > d.maxBytes {
				return Event{}, fmt.Errorf("SSE event exceeds %d bytes", d.maxBytes)
			}
			data.Write(value)
			data.WriteByte('\n')
			seen = true
		}
	}
}

func (d *Decoder) readLine() ([]byte, error) {
	line := make([]byte, 0, 128)
	for {
		value, err := d.reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
		if d.skipLF {
			d.skipLF = false
			if value == '\n' {
				continue
			}
		}
		switch value {
		case '\n':
			return line, nil
		case '\r':
			d.skipLF = true
			return line, nil
		default:
			if len(line)+1 > d.maxBytes {
				return nil, fmt.Errorf("SSE line exceeds %d bytes", d.maxBytes)
			}
			line = append(line, value)
		}
	}
}

type Encoder struct {
	writer io.Writer
}

func NewEncoder(writer io.Writer) *Encoder {
	return &Encoder{writer: writer}
}

func (e *Encoder) Write(event Event) error {
	if event.Event != "" {
		if _, err := fmt.Fprintf(e.writer, "event: %s\n", event.Event); err != nil {
			return err
		}
	}
	// A bare CR ends a line for any spec-compliant client, so a payload carrying
	// one would otherwise inject a field — or, with a second CR, an entire event
	// boundary — into the stream. Native pass-through is where this bites: JSON
	// escapes CR, raw upstream bytes do not. Every CRLF, CR and LF becomes its
	// own data: line, which is the only thing this wire format can express.
	normalized := bytes.ReplaceAll(event.Data, []byte("\r\n"), []byte{'\n'})
	normalized = bytes.ReplaceAll(normalized, []byte{'\r'}, []byte{'\n'})
	for _, line := range bytes.Split(normalized, []byte{'\n'}) {
		if _, err := e.writer.Write(append(append([]byte("data: "), line...), '\n')); err != nil {
			return err
		}
	}
	_, err := io.WriteString(e.writer, "\n")
	return err
}
