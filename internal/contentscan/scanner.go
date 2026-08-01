package contentscan

import (
	"bytes"
	"errors"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

var ErrRejected = errors.New("uploaded content rejected by media policy")

type Scanner interface {
	ScanFile(filename, declaredType string, data []byte) error
	ScanAudio(filename, declaredType string, data []byte) error
}

// Builtin is a bounded format admission gate, not an antivirus or malware
// classifier. Deployments that require malicious-content detection must inject
// a dedicated Scanner implementation and fail closed when it is unavailable.
type Builtin struct{}

// Textual reports whether content must pass the text redaction pipeline. It
// intentionally considers both the untrusted declared media type and a local
// sniff so that text disguised as application/octet-stream cannot bypass
// mandatory secret handling.
func Textual(declaredType string, data []byte) bool {
	declared := mediaType(declaredType)
	if strings.HasPrefix(declared, "text/") || strings.Contains(declared, "json") {
		return true
	}
	detected := mediaType(http.DetectContentType(data))
	if strings.HasPrefix(detected, "text/") || strings.Contains(detected, "json") {
		return true
	}
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && utf8.Valid(trimmed) &&
		(trimmed[0] == '{' || trimmed[0] == '[')
}

func (Builtin) ScanFile(_ string, declaredType string, data []byte) error {
	if len(data) == 0 || dangerousMagic(data) {
		return ErrRejected
	}
	if Textual(declaredType, data) {
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return ErrRejected
		}
	}
	return nil
}

func (Builtin) ScanAudio(filename, declaredType string, data []byte) error {
	if len(data) == 0 || dangerousMagic(data) {
		return ErrRejected
	}
	declared := mediaType(declaredType)
	if declared != "" && declared != "application/octet-stream" &&
		!strings.HasPrefix(declared, "audio/") && declared != "video/mp4" && declared != "video/webm" {
		return ErrRejected
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".flac", ".m4a", ".mp3", ".mp4", ".mpeg", ".mpga", ".oga", ".ogg", ".wav", ".webm":
	default:
		return ErrRejected
	}
	detected := mediaType(http.DetectContentType(data))
	if strings.HasPrefix(detected, "text/") || strings.Contains(detected, "json") {
		return ErrRejected
	}
	return nil
}

func mediaType(value string) string {
	parsed, _, err := mime.ParseMediaType(value)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	}
	return strings.ToLower(parsed)
}

func dangerousMagic(data []byte) bool {
	patterns := [][]byte{
		{0x7f, 'E', 'L', 'F'},
		{'M', 'Z'},
		{'P', 'K', 0x03, 0x04},
		{'R', 'a', 'r', '!'},
		{0x37, 0x7a, 0xbc, 0xaf, 0x27, 0x1c},
		{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf},
		{0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe},
	}
	for _, pattern := range patterns {
		if bytes.HasPrefix(data, pattern) {
			return true
		}
	}
	return false
}
