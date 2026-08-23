package redaction

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// jsonUnescaper decodes JSON string escapes out of a stream of fragments,
// holding back the bytes of an escape sequence that has not arrived in full.
// The longest sequence it has to recognise is a surrogate pair, so the held
// back tail never exceeds eleven bytes.
type jsonUnescaper struct {
	carry string
}

// push decodes the next fragment. Escapes are decoded wherever they appear
// rather than only inside string literals: a backslash outside a string is not
// valid JSON anyway, and tracking string context would buy no coverage while
// adding a parser that has to stay correct across fragment boundaries.
func (u *jsonUnescaper) push(piece string) string {
	buffer := u.carry + piece
	u.carry = ""
	var decoded strings.Builder
	for index := 0; index < len(buffer); {
		next := strings.IndexByte(buffer[index:], '\\')
		if next < 0 {
			decoded.WriteString(buffer[index:])
			break
		}
		decoded.WriteString(buffer[index : index+next])
		index += next
		value, width, complete := decodeJSONEscape(buffer[index:])
		if !complete {
			u.carry = buffer[index:]
			break
		}
		decoded.WriteString(value)
		index += width
	}
	return decoded.String()
}

// flush releases a trailing partial escape once the channel ends. Its raw bytes
// are the only honest projection left: the client's decoder will reject them.
func (u *jsonUnescaper) flush() string {
	carry := u.carry
	u.carry = ""
	return carry
}

// decodeJSONEscape decodes the escape sequence at the start of value and
// reports the bytes it consumed, or complete=false when the sequence is cut off
// by the end of the fragment.
func decodeJSONEscape(value string) (string, int, bool) {
	if len(value) < 2 {
		return "", 0, false
	}
	switch value[1] {
	case '"', '\\', '/':
		return value[1:2], 2, true
	case 'b':
		return "\b", 2, true
	case 'f':
		return "\f", 2, true
	case 'n':
		return "\n", 2, true
	case 'r':
		return "\r", 2, true
	case 't':
		return "\t", 2, true
	case 'u':
	default:
		// Not an escape JSON defines. Keeping both bytes leaves the mirror a
		// projection of what arrived instead of an invention of ours.
		return value[:2], 2, true
	}
	if len(value) < 6 {
		return "", 0, false
	}
	first, ok := parseHex16(value[2:6])
	if !ok {
		return value[:2], 2, true
	}
	if !utf16.IsSurrogate(rune(first)) {
		return string(rune(first)), 6, true
	}
	if len(value) < 12 {
		return "", 0, false
	}
	if value[6] != '\\' || value[7] != 'u' {
		return string(utf8.RuneError), 6, true
	}
	second, ok := parseHex16(value[8:12])
	if !ok {
		return string(utf8.RuneError), 6, true
	}
	if paired := utf16.DecodeRune(rune(first), rune(second)); paired != utf8.RuneError {
		return string(paired), 12, true
	}
	return string(utf8.RuneError), 6, true
}

func parseHex16(value string) (uint16, bool) {
	parsed, err := strconv.ParseUint(value, 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(parsed), true
}
