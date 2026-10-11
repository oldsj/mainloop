// Package pushgate validates and publishes one feature-branch update through smart HTTP.
// Client packets supply proposed data, never identity or publication authority.
package pushgate

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/oldsj/mainloop/server/internal/gitref"
)

const zeroOID = "0000000000000000000000000000000000000000"
const capabilities = "report-status side-band-64k ofs-delta object-format=sha1 agent=mainloop"

var oidPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var agentPattern = regexp.MustCompile(`^agent=[A-Za-z0-9./_+-]{1,128}$`)

type denial string

func (d denial) Error() string { return string(d) }

type Update struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}
type commands struct {
	Update
	Offset   int64
	Sideband bool
}

func pkt(b []byte) []byte { return append(fmt.Appendf(nil, "%04x", len(b)+4), b...) }
func packet(r io.Reader) ([]byte, bool, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, false, denial("pkt_header")
	}
	for _, b := range head {
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(b)) {
			return nil, false, denial("pkt_header")
		}
	}
	n, err := strconv.ParseUint(string(head[:]), 16, 16)
	if err != nil {
		return nil, false, denial("pkt_header")
	}
	if n == 0 {
		return nil, true, nil
	}
	if n < 4 || n > 65520 {
		return nil, false, denial("pkt_length")
	}
	b := make([]byte, n-4)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, false, denial("pkt_truncated")
	}
	return b, false, nil
}

func receiveCommands(r io.ReadSeeker, limit int64) (commands, error) {
	var c commands
	seen := map[string]bool{}
	count := 0
	for {
		b, end, err := packet(r)
		if err != nil {
			return c, err
		}
		offset, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return c, err
		}
		if offset > limit {
			return c, denial("command_limit")
		}
		if end {
			c.Offset = offset
			break
		}
		if bytes.HasPrefix(b, []byte("shallow ")) {
			oid := strings.TrimSuffix(string(b[8:]), "\n")
			if count > 0 || !oidPattern.MatchString(oid) || oid == zeroOID || seen[oid] {
				return c, denial("shallow_invalid")
			}
			seen[oid] = true
			continue
		}
		count++
		if count > 1 {
			return c, denial("single_ref_required")
		}
		if bytes.Count(b, []byte{0}) != 1 {
			return c, denial("capability_placement")
		}
		line, caps, _ := bytes.Cut(b, []byte{0})
		words := strings.Split(strings.TrimPrefix(strings.TrimSuffix(string(caps), "\n"), " "), " ")
		capSeen := map[string]bool{}
		agents := 0
		for _, w := range words {
			if capSeen[w] {
				return c, denial("capability_duplicate")
			}
			capSeen[w] = true
			switch w {
			case "report-status", "ofs-delta", "object-format=sha1":
			case "side-band-64k":
				c.Sideband = true
			default:
				if !agentPattern.MatchString(w) {
					return c, denial("unsupported_capability")
				}
				agents++
			}
		}
		if !capSeen["report-status"] || agents > 1 {
			return c, denial("unsupported_capability")
		}
		fields := strings.Split(strings.TrimSuffix(string(line), "\n"), " ")
		if len(fields) != 3 || !oidPattern.MatchString(fields[0]) || !oidPattern.MatchString(fields[1]) || !gitref.Valid(fields[2]) {
			return c, denial("command_invalid")
		}
		c.Update = Update{fields[0], fields[1], fields[2]}
	}
	if count != 1 {
		return c, denial("single_ref_required")
	}
	return c, nil
}

func advertisement(branch, oid string) []byte {
	ref := "refs/heads/" + branch
	if oid == zeroOID {
		ref = "capabilities^{}"
	}
	b := pkt([]byte("# service=git-receive-pack\n"))
	b = append(b, []byte("0000")...)
	b = append(b, pkt([]byte(oid+" "+ref+"\x00"+capabilities+"\n"))...)
	return append(b, []byte("0000")...)
}

func advertisedRefs(data []byte, service string) (map[string]string, error) {
	r := bytes.NewReader(data)
	b, end, err := packet(r)
	if err != nil || end || string(b) != "# service="+service+"\n" {
		return nil, denial("upstream_advertisement")
	}
	if _, end, err = packet(r); err != nil || !end {
		return nil, denial("upstream_advertisement")
	}
	refs := map[string]string{}
	first := true
	for {
		b, end, err = packet(r)
		if err != nil {
			return nil, denial("upstream_advertisement")
		}
		if end {
			break
		}
		if bytes.Contains(b, []byte{0}) {
			if !first || bytes.Count(b, []byte{0}) != 1 {
				return nil, denial("upstream_advertisement")
			}
			line, caps, _ := bytes.Cut(b, []byte{0})
			b = line
			for _, c := range strings.Fields(string(caps)) {
				if strings.HasPrefix(c, "object-format=") && c != "object-format=sha1" {
					return nil, denial("upstream_object_format")
				}
			}
		}
		first = false
		f := strings.Split(strings.TrimSuffix(string(b), "\n"), " ")
		if len(f) != 2 || !oidPattern.MatchString(f[0]) || refs[f[1]] != "" {
			return nil, denial("upstream_advertisement")
		}
		if f[1] != "HEAD" && f[1] != "capabilities^{}" && (!strings.HasPrefix(f[1], "refs/") || !gitref.Valid(strings.TrimSuffix(f[1], "^{}"))) {
			return nil, denial("upstream_advertisement")
		}
		refs[f[1]] = f[0]
	}
	if r.Len() != 0 {
		return nil, denial("upstream_advertisement")
	}
	return refs, nil
}

func report(c commands, accepted bool) []byte {
	status := "ng " + c.Ref + " upstream_rejected\n"
	if accepted {
		status = "ok " + c.Ref + "\n"
	}
	b := append(pkt([]byte("unpack ok\n")), pkt([]byte(status))...)
	b = append(b, []byte("0000")...)
	if c.Sideband {
		b = append(pkt(append([]byte{1}, b...)), []byte("0000")...)
	}
	return b
}

func receipt(data []byte, c commands) (bool, error) {
	if c.Sideband {
		r := bytes.NewReader(data)
		var inner []byte
		for {
			b, end, err := packet(r)
			if err != nil {
				return false, denial("upstream_receipt")
			}
			if end {
				break
			}
			if len(b) == 0 || (b[0] != 1 && b[0] != 2) {
				return false, denial("upstream_receipt")
			}
			if b[0] == 1 {
				inner = append(inner, b[1:]...)
			}
		}
		if r.Len() != 0 {
			return false, denial("upstream_receipt")
		}
		data = inner
	}
	r := bytes.NewReader(data)
	unpack, end, err := packet(r)
	if err != nil || end || !safeLine(unpack, "unpack ") {
		return false, denial("upstream_receipt")
	}
	status, end, err := packet(r)
	if err != nil || end {
		return false, denial("upstream_receipt")
	}
	if _, end, err = packet(r); err != nil || !end || r.Len() != 0 {
		return false, denial("upstream_receipt")
	}
	if string(status) == "ok "+c.Ref+"\n" && string(unpack) == "unpack ok\n" {
		return true, nil
	}
	if safeLine(status, "ng "+c.Ref+" ") {
		return false, nil
	}
	return false, denial("upstream_receipt")
}

func safeLine(b []byte, prefix string) bool {
	if !bytes.HasPrefix(b, []byte(prefix)) || !bytes.HasSuffix(b, []byte("\n")) || len(b) <= len(prefix)+1 {
		return false
	}
	for _, v := range b[:len(b)-1] {
		if v < 32 || v == 127 {
			return false
		}
	}
	return true
}
