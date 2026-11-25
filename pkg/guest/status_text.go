package guest

import (
	"unicode"
	"unicode/utf8"
)

// MaxGuestStatusChars is the most text a guest-supplied status message
// or event may contain. The guest is root on its VM and is not trusted
// to size the strings the host writes into PodStatus.
const MaxGuestStatusChars = 1024

// ClampExitCode folds a guest-reported workload exit into the 0..255
// range a container status can hold. Negative becomes 0. A guest can
// already report 0, so this does not stop it from claiming success.
func ClampExitCode(code int32) int32 {
	if code < 0 {
		return 0
	}
	if code > 255 {
		return 255
	}
	return code
}

// SanitizeGuestText keeps at most MaxGuestStatusChars printable UTF-8
// runes. Invalid sequences and non-printable characters, including
// newlines and control bytes, are dropped. The result is safe to put
// in a PodStatus message or an event.
func SanitizeGuestText(s string) string {
	if s == "" {
		return ""
	}
	var b []rune
	for i := 0; i < len(s) && len(b) < MaxGuestStatusChars; {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		i += size
		if unicode.IsPrint(r) {
			b = append(b, r)
		}
	}
	return string(b)
}
