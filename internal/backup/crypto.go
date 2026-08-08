package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	archiveMagic   = "HMBK"
	archiveVersion = 1
	chunkSize      = 1 << 20
	headerSize     = 16
	recordHeader   = 9
)

type encryptWriter struct {
	output  io.Writer
	aead    cipher.AEAD
	header  [headerSize]byte
	buffer  []byte
	counter uint32
	closed  bool
}

func newEncryptWriter(output io.Writer, backupKey []byte) (*encryptWriter, error) {
	if output == nil {
		return nil, errors.New("backup output is required")
	}
	aead, err := backupAEAD(backupKey)
	if err != nil {
		return nil, err
	}
	writer := &encryptWriter{
		output: output, aead: aead, buffer: make([]byte, 0, chunkSize),
	}
	copy(writer.header[:4], archiveMagic)
	writer.header[4] = archiveVersion
	if _, err := rand.Read(writer.header[8:]); err != nil {
		return nil, fmt.Errorf("generate backup nonce prefix: %w", err)
	}
	if _, err := output.Write(writer.header[:]); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *encryptWriter) Write(payload []byte) (int, error) {
	if w.closed {
		return 0, errors.New("backup encrypt writer is closed")
	}
	written := 0
	for len(payload) > 0 {
		available := chunkSize - len(w.buffer)
		take := min(available, len(payload))
		w.buffer = append(w.buffer, payload[:take]...)
		payload = payload[take:]
		written += take
		if len(w.buffer) == chunkSize {
			if err := w.writeRecord(w.buffer, 0); err != nil {
				return written, err
			}
			w.buffer = w.buffer[:0]
		}
	}
	return written, nil
}

func (w *encryptWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if len(w.buffer) > 0 {
		if err := w.writeRecord(w.buffer, 0); err != nil {
			return err
		}
		clear(w.buffer)
		w.buffer = nil
	}
	return w.writeRecord(nil, 1)
}

func (w *encryptWriter) writeRecord(plaintext []byte, final byte) error {
	if w.counter == ^uint32(0) {
		return errors.New("backup exceeds encrypted chunk counter")
	}
	var metadata [recordHeader]byte
	binary.BigEndian.PutUint32(metadata[:4], uint32(len(plaintext)))
	binary.BigEndian.PutUint32(metadata[4:8], w.counter)
	metadata[8] = final
	nonce := make([]byte, w.aead.NonceSize())
	copy(nonce, w.header[8:])
	binary.BigEndian.PutUint32(nonce[8:], w.counter)
	aad := make([]byte, 0, len(w.header)+len(metadata))
	aad = append(aad, w.header[:]...)
	aad = append(aad, metadata[:]...)
	ciphertext := w.aead.Seal(nil, nonce, plaintext, aad)
	if _, err := w.output.Write(metadata[:]); err != nil {
		return err
	}
	if _, err := w.output.Write(ciphertext); err != nil {
		return err
	}
	w.counter++
	return nil
}

type decryptReader struct {
	input    io.Reader
	aead     cipher.AEAD
	header   [headerSize]byte
	buffer   []byte
	counter  uint32
	finished bool
}

func newDecryptReader(input io.Reader, backupKey []byte) (*decryptReader, error) {
	if input == nil {
		return nil, errors.New("backup input is required")
	}
	reader := &decryptReader{input: input}
	if _, err := io.ReadFull(input, reader.header[:]); err != nil {
		return nil, fmt.Errorf("read backup header: %w", err)
	}
	if string(reader.header[:4]) != archiveMagic || reader.header[4] != archiveVersion {
		return nil, errors.New("backup format is not supported")
	}
	if reader.header[5] != 0 || reader.header[6] != 0 || reader.header[7] != 0 {
		return nil, errors.New("backup header reserved bytes are non-zero")
	}
	aead, err := backupAEAD(backupKey)
	if err != nil {
		return nil, err
	}
	reader.aead = aead
	return reader, nil
}

func (r *decryptReader) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	for len(r.buffer) == 0 {
		if r.finished {
			return 0, io.EOF
		}
		if err := r.readRecord(); err != nil {
			return 0, err
		}
	}
	count := copy(destination, r.buffer)
	r.buffer = r.buffer[count:]
	return count, nil
}

func (r *decryptReader) readRecord() error {
	var metadata [recordHeader]byte
	if _, err := io.ReadFull(r.input, metadata[:]); err != nil {
		return fmt.Errorf("backup ended before authenticated final record: %w", err)
	}
	length := binary.BigEndian.Uint32(metadata[:4])
	counter := binary.BigEndian.Uint32(metadata[4:8])
	final := metadata[8]
	if counter != r.counter || length > chunkSize || final > 1 ||
		(final == 1 && length != 0) {
		return errors.New("backup encrypted record is invalid")
	}
	ciphertext := make([]byte, int(length)+r.aead.Overhead())
	if _, err := io.ReadFull(r.input, ciphertext); err != nil {
		return fmt.Errorf("read encrypted backup record: %w", err)
	}
	nonce := make([]byte, r.aead.NonceSize())
	copy(nonce, r.header[8:])
	binary.BigEndian.PutUint32(nonce[8:], counter)
	aad := make([]byte, 0, len(r.header)+len(metadata))
	aad = append(aad, r.header[:]...)
	aad = append(aad, metadata[:]...)
	plaintext, err := r.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return errors.New("backup authentication failed")
	}
	r.counter++
	if final == 1 {
		var trailing [1]byte
		count, err := r.input.Read(trailing[:])
		if count != 0 || !errors.Is(err, io.EOF) {
			return errors.New("backup has trailing data after final record")
		}
		r.finished = true
		return nil
	}
	r.buffer = plaintext
	return nil
}

func backupAEAD(backupKey []byte) (cipher.AEAD, error) {
	if len(backupKey) != 32 {
		return nil, errors.New("backup key must be exactly 32 bytes")
	}
	domain := []byte("halro:backup:v1\x00")
	material := make([]byte, 0, len(domain)+len(backupKey))
	material = append(material, domain...)
	material = append(material, backupKey...)
	derived := sha256.Sum256(material)
	clear(material)
	block, err := aes.NewCipher(derived[:])
	clear(derived[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
