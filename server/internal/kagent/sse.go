package kagent

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

const maxEventBytes = 16 << 20

var errEventTooLarge = errors.New("kagent: SSE event exceeds 16 MiB")

// readSSE splits a text/event-stream body into events and calls yield with each event's data
// lines joined by newlines, until yield returns false. It stops with nil at a clean end of
// body, and a final event with no closing blank line is still delivered. Other fields and
// comments are ignored.
func readSSE(r io.Reader, yield func(data []byte) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var data []byte
	haveData := false
	flush := func() bool {
		if !haveData {
			return true
		}
		ok := yield(data)
		data, haveData = nil, false
		return ok
	}
	for {
		line, err := readLine(br)
		if len(line) > 0 || err == nil {
			switch {
			case len(line) == 0:
				if !flush() {
					return nil
				}
			case bytes.HasPrefix(line, []byte("data:")):
				value := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
				if haveData {
					data = append(data, '\n')
				}
				if len(data)+len(value) > maxEventBytes {
					return errEventTooLarge
				}
				data = append(data, value...)
				haveData = true
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				flush()
				return nil
			}
			return err
		}
	}
}

// readLine reads one line without its CR LF or LF, however long it is up to the event limit.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		line = append(line, chunk...)
		if len(line) > maxEventBytes {
			return nil, errEventTooLarge
		}
		if err != nil || !isPrefix {
			return line, err
		}
	}
}
