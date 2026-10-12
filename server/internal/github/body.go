package github

import (
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// pullBody rejects text encoding/json would silently replace with U+FFFD. A digest must
// describe the original UTF-8 text, including valid literal replacement characters.
type pullBody string

func (b *pullBody) UnmarshalJSON(raw []byte) error {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return ErrUnavailable
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		code, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil || (code >= 0xdc00 && code <= 0xdfff) {
			return ErrUnavailable
		}
		i += 4
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return ErrUnavailable
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return ErrUnavailable
			}
			i += 6
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return ErrUnavailable
	}
	*b = pullBody(text)
	return nil
}
