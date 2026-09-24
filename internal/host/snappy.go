package host

import (
	"errors"
	"fmt"
)

// DecodeSnappy decodes a raw (block-format) snappy stream, the encoding
// GitHub uses for attestation bundle_url payloads (as GitHub's own CLI
// decodes them with snappy.Decode). The format is small enough to implement
// here without a dependency: a varint length followed by literal and copy
// elements (https://github.com/google/snappy/blob/main/format_description.txt).
func DecodeSnappy(src []byte) ([]byte, error) {
	n, s := uvarint(src)
	if s <= 0 || n > 64<<20 {
		return nil, errors.New("snappy: invalid length")
	}
	dst := make([]byte, 0, n)
	for s < len(src) {
		tag := src[s]
		s++
		switch tag & 3 {
		case 0: // literal
			length := int(tag>>2) + 1
			if length > 60 {
				extra := length - 60
				if s+extra > len(src) {
					return nil, errors.New("snappy: truncated literal length")
				}
				length = 0
				for i := 0; i < extra; i++ {
					length |= int(src[s+i]) << (8 * i)
				}
				length++
				s += extra
			}
			if s+length > len(src) {
				return nil, errors.New("snappy: truncated literal")
			}
			dst = append(dst, src[s:s+length]...)
			s += length
		case 1: // copy with 1-byte offset
			if s >= len(src) {
				return nil, errors.New("snappy: truncated copy")
			}
			length := 4 + int(tag>>2)&7
			offset := int(tag&0xe0)<<3 | int(src[s])
			s++
			if err := appendCopy(&dst, offset, length); err != nil {
				return nil, err
			}
		case 2: // copy with 2-byte offset
			if s+2 > len(src) {
				return nil, errors.New("snappy: truncated copy")
			}
			length := 1 + int(tag>>2)
			offset := int(src[s]) | int(src[s+1])<<8
			s += 2
			if err := appendCopy(&dst, offset, length); err != nil {
				return nil, err
			}
		case 3: // copy with 4-byte offset
			if s+4 > len(src) {
				return nil, errors.New("snappy: truncated copy")
			}
			length := 1 + int(tag>>2)
			offset := int(src[s]) | int(src[s+1])<<8 | int(src[s+2])<<16 | int(src[s+3])<<24
			s += 4
			if err := appendCopy(&dst, offset, length); err != nil {
				return nil, err
			}
		}
	}
	if uint64(len(dst)) != n {
		return nil, fmt.Errorf("snappy: decoded %d bytes, header says %d", len(dst), n)
	}
	return dst, nil
}

func appendCopy(dst *[]byte, offset, length int) error {
	d := *dst
	if offset <= 0 || offset > len(d) {
		return errors.New("snappy: copy offset out of range")
	}
	for i := 0; i < length; i++ {
		d = append(d, d[len(d)-offset])
	}
	*dst = d
	return nil
}

func uvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i == 10 {
			return 0, -1
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}
