package pushgate

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1" //nolint:gosec // Git's SHA-1 checksum is part of the wire format.
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"math"
)

// walkPack bounds decompression before Git reconstructs deltas. Git performs the actual object
// validation. A ByteReader prevents zlib from consuming the next object's bytes.
func walkPack(source io.Reader, l Limits) error {
	r := bufio.NewReader(source)
	tracked := &packReader{r: r, h: sha1.New()} //nolint:gosec // required by the Git pack format
	var header [12]byte
	if _, err := io.ReadFull(tracked, header[:]); err != nil || string(header[:4]) != "PACK" {
		return denial("pack_header")
	}
	version := binary.BigEndian.Uint32(header[4:8])
	count := binary.BigEndian.Uint32(header[8:])
	if (version != 2 && version != 3) || int64(count) > l.Objects {
		return denial("pack_limit")
	}
	var expanded int64
	for range count {
		b, err := tracked.ReadByte()
		if err != nil {
			return denial("pack_truncated")
		}
		kind := b >> 4 & 7
		size := uint64(b & 15)
		shift := uint(4)
		for b&128 != 0 {
			if shift > 60 {
				return denial("pack_object_size")
			}
			b, err = tracked.ReadByte()
			if err != nil {
				return denial("pack_truncated")
			}
			size |= uint64(b&127) << shift
			shift += 7
		}
		decodedSize, err := objectSize(size, l.ExpandedBytes)
		if err != nil || (kind < 1 || kind > 7 || kind == 5) {
			return denial("pack_object_size")
		}
		if kind == 6 {
			for {
				b, err = tracked.ReadByte()
				if err != nil {
					return denial("pack_truncated")
				}
				if b&128 == 0 {
					break
				}
			}
		}
		if kind == 7 {
			var base [20]byte
			if _, err = io.ReadFull(tracked, base[:]); err != nil {
				return denial("pack_truncated")
			}
		}
		zr, err := zlib.NewReader(tracked)
		if err != nil {
			return denial("pack_compression")
		}
		var prefix [32]byte
		n, readErr := io.ReadFull(io.LimitReader(zr, decodedSize+1), prefix[:min(int64(len(prefix)), decodedSize+1)])
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			_ = zr.Close()
			return denial("pack_compression")
		}
		rest, err := io.Copy(io.Discard, io.LimitReader(zr, decodedSize+1-int64(n)))
		_ = zr.Close()
		if err != nil || int64(n)+rest != decodedSize {
			return denial("pack_object_size")
		}
		objSize := decodedSize
		if kind == 6 || kind == 7 {
			a, k, err := deltaSize(prefix[:n])
			if err != nil {
				return err
			}
			b, _, err := deltaSize(prefix[k:n])
			if err != nil {
				return err
			}
			_, baseErr := objectSize(a, l.ExpandedBytes)
			resultSize, resultErr := objectSize(b, l.ExpandedBytes)
			if baseErr != nil || resultErr != nil {
				return denial("pack_expansion_limit")
			}
			objSize = max(objSize, resultSize)
		}
		expanded += objSize
		if expanded > l.ExpandedBytes {
			return denial("pack_expansion_limit")
		}
	}
	var checksum [20]byte
	if _, err := io.ReadFull(r, checksum[:]); err != nil {
		return denial("pack_truncated")
	}
	if !bytes.Equal(checksum[:], tracked.h.Sum(nil)) {
		return denial("pack_checksum")
	}
	if _, err := r.ReadByte(); err != io.EOF {
		return denial("pack_trailing_or_truncated")
	}
	// SHA-1 is never authority or the HTTP body identity; those use the grant and SHA-256.
	return nil
}

func objectSize(v uint64, limit int64) (int64, error) {
	// Leave room for the sentinel byte read by the bounded inflater.
	if v >= math.MaxInt64 {
		return 0, denial("pack_object_size")
	}
	n := int64(v)
	if n > limit {
		return 0, denial("pack_object_size")
	}
	return n, nil
}

type packReader struct {
	r *bufio.Reader
	h hash.Hash
}

func (r *packReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	_, _ = r.h.Write(b[:n])
	return n, err
}
func (r *packReader) ReadByte() (byte, error) {
	b, err := r.r.ReadByte()
	if err == nil {
		_, _ = r.h.Write([]byte{b})
	}
	return b, err
}
func deltaSize(b []byte) (uint64, int, error) {
	var n uint64
	for i, v := range b {
		if i >= 9 {
			return 0, 0, denial("delta_header")
		}
		n |= uint64(v&127) << uint(i*7)
		if v&128 == 0 {
			return n, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("%w", denial("delta_header"))
}
