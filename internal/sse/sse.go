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
	var line []byte
	for {
		fragment, prefix, err := d.reader.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(line)+len(fragment) > d.maxBytes {
			return nil, fmt.Errorf("SSE line exceeds %d bytes", d.maxBytes)
		}
		line = append(line, fragment...)
		if !prefix {
			return line, nil
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
	lines := bytes.Split(event.Data, []byte{'\n'})
	for _, line := range lines {
		if _, err := e.writer.Write(append(append([]byte("data: "), line...), '\n')); err != nil {
			return err
		}
	}
	_, err := io.WriteString(e.writer, "\n")
	return err
}
