package contentscan

import (
	"errors"
	"testing"
)

func TestBuiltinRejectsExecutablesArchivesAndInvalidText(t *testing.T) {
	scanner := Builtin{}
	for _, data := range [][]byte{{0x7f, 'E', 'L', 'F'}, {'M', 'Z'}, {'P', 'K', 3, 4}, {'a', 0, 'b'}} {
		if err := scanner.ScanFile("input", "text/plain", data); !errors.Is(err, ErrRejected) {
			t.Fatalf("data=%x err=%v", data, err)
		}
	}
	if err := scanner.ScanFile("input.jsonl", "application/jsonl", []byte("{\"ok\":true}\n")); err != nil {
		t.Fatal(err)
	}
	if !Textual("application/octet-stream", []byte("{\"secret\":\"sk-example-secret-value\"}\n")) {
		t.Fatal("JSON disguised as application/octet-stream was not classified as text")
	}
}

func TestBuiltinAudioRequiresSupportedExtensionAndNonTextPayload(t *testing.T) {
	scanner := Builtin{}
	if err := scanner.ScanAudio("voice.exe", "audio/mpeg", []byte{1, 2, 3}); !errors.Is(err, ErrRejected) {
		t.Fatalf("unexpected extension err=%v", err)
	}
	if err := scanner.ScanAudio("voice.mp3", "audio/mpeg", []byte("plain text")); !errors.Is(err, ErrRejected) {
		t.Fatalf("text payload err=%v", err)
	}
	mp3 := make([]byte, 512)
	copy(mp3, []byte("ID3\x04\x00\x00\x00\x00\x00\x15"))
	if err := scanner.ScanAudio("voice.mp3", "audio/mpeg", mp3); err != nil {
		t.Fatal(err)
	}
}
