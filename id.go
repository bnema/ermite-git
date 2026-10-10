// SPDX-License-Identifier: MIT

package git

import "fmt"

// ObjectID is a SHA-1 object id.
type ObjectID [20]byte

const hexDigits = "0123456789abcdef"

// oidHexVal maps a hex digit to its value, or 0xff.
var oidHexVal = func() (t [256]byte) {
	for i := range t {
		t[i] = 0xff
	}
	for i := 0; i < 10; i++ {
		t['0'+i] = byte(i)
	}
	for i := 0; i < 6; i++ {
		t['a'+i] = byte(10 + i)
		t['A'+i] = byte(10 + i)
	}
	return t
}()

// ParseID parses 40 hexadecimal digits.
func ParseID(s string) (ObjectID, error) {
	var id ObjectID
	if len(s) != 40 {
		return id, fmt.Errorf("%w: %q has %d characters, want 40", ErrInvalidID, truncate(s), len(s))
	}
	for i := 0; i < 20; i++ {
		hi, lo := oidHexVal[s[2*i]], oidHexVal[s[2*i+1]]
		if hi|lo > 0x0f {
			return ObjectID{}, fmt.Errorf("%w: %q is not hexadecimal", ErrInvalidID, truncate(s))
		}
		id[i] = hi<<4 | lo
	}
	return id, nil
}

// parseIDBytes parses 40 hexadecimal digits.
func parseIDBytes(b []byte) (id ObjectID, ok bool) {
	if len(b) != 40 {
		return id, false
	}
	for i := 0; i < 20; i++ {
		hi, lo := oidHexVal[b[2*i]], oidHexVal[b[2*i+1]]
		if hi|lo > 0x0f {
			return ObjectID{}, false
		}
		id[i] = hi<<4 | lo
	}
	return id, true
}

// String returns the 40 lowercase hexadecimal digits of the id.
func (id ObjectID) String() string {
	var b [40]byte
	return string(id.AppendHex(b[:0]))
}

// AppendHex appends the 40 lowercase hexadecimal digits of the id to dst.
// It does not allocate when dst has room.
func (id ObjectID) AppendHex(dst []byte) []byte {
	for _, c := range id {
		dst = append(dst, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return dst
}

// IsZero reports whether the id is all zeros (the null id).
func (id ObjectID) IsZero() bool { return id == ObjectID{} }

func truncate(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
