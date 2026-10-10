// SPDX-License-Identifier: MIT

package git

// deltaVarint reads a little-endian base-128 size of a delta header.
func deltaVarint(d []byte, p int) (v uint64, next int, ok bool) {
	var shift uint
	for p < len(d) {
		c := d[p]
		p++
		if shift > 56 {
			return 0, 0, false
		}
		v |= uint64(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			return v, p, true
		}
	}
	return 0, 0, false
}

// deltaHeader parses the base size and the result size of a delta.
func deltaHeader(d []byte) (baseSize, resultSize uint64, body int, err error) {
	var ok bool
	baseSize, p, ok := deltaVarint(d, 0)
	if !ok {
		return 0, 0, 0, corrupt("delta: bad base size")
	}
	resultSize, p, ok = deltaVarint(d, p)
	if !ok {
		return 0, 0, 0, corrupt("delta: bad result size")
	}
	return baseSize, resultSize, p, nil
}

// applyDelta applies delta to base and writes the result into dst, whose
// length is the result size announced by the delta header (checked by the
// caller with deltaHeader). Every copy and insert is bounds checked.
func applyDelta(dst, base, delta []byte, body int) error {
	out := 0
	p := body
	for p < len(delta) {
		op := delta[p]
		p++
		switch {
		case op&0x80 != 0:
			var off, size uint64
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					if p >= len(delta) {
						return corrupt("delta: truncated copy offset")
					}
					off |= uint64(delta[p]) << (8 * i)
					p++
				}
			}
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					if p >= len(delta) {
						return corrupt("delta: truncated copy size")
					}
					size |= uint64(delta[p]) << (8 * i)
					p++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off > uint64(len(base)) || size > uint64(len(base))-off {
				return corrupt("delta: copy %d+%d outside base of %d", off, size, len(base))
			}
			if size > uint64(len(dst)-out) {
				return corrupt("delta: result longer than announced")
			}
			copy(dst[out:], base[off:off+size])
			out += int(size)
		case op != 0:
			n := int(op)
			if n > len(delta)-p {
				return corrupt("delta: truncated insert")
			}
			if n > len(dst)-out {
				return corrupt("delta: result longer than announced")
			}
			copy(dst[out:], delta[p:p+n])
			p += n
			out += n
		default:
			return corrupt("delta: reserved opcode 0")
		}
	}
	if out != len(dst) {
		return corrupt("delta: result of %d bytes, %d announced", out, len(dst))
	}
	return nil
}

// patch applies a delta to base and returns the result, which is at most
// limit bytes. It is the allocating form of applyDelta, for tests and fuzzing.
func patch(base, delta []byte, limit int64) ([]byte, error) {
	bs, rs, body, err := deltaHeader(delta)
	if err != nil {
		return nil, err
	}
	if bs != uint64(len(base)) {
		return nil, corrupt("delta: base is %d bytes, delta expects %d", len(base), bs)
	}
	if rs > uint64(limit) {
		return nil, ErrTooLarge
	}
	dst := make([]byte, rs)
	if err := applyDelta(dst, base, delta, body); err != nil {
		return nil, err
	}
	return dst, nil
}
