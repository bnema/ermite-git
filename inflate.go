// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash"
	"hash/adler32"
	"io"
	"math"
	"sync"
)

// buffer is a pooled byte slice. Pooling the pointer keeps Put free of
// allocations.
type buffer struct{ b []byte }

// maxPooledBuffer is the largest buffer that goes back to the pool, so a
// single big blob does not pin memory.
const maxPooledBuffer = 4 << 20

var bufPool = sync.Pool{New: func() any { return &buffer{b: make([]byte, 0, 4096)} }}

func getBuf() *buffer { return bufPool.Get().(*buffer) }

func putBuf(b *buffer) {
	if b == nil || cap(b.b) > maxPooledBuffer {
		return
	}
	b.b = b.b[:0]
	bufPool.Put(b)
}

// grow returns b with length n, reusing its capacity when possible. The
// content is not preserved.
func grow(b []byte, n int) []byte {
	if cap(b) >= n {
		return b[:n]
	}
	// Spare room for the next, slightly bigger object; n+n/4 may overflow
	// an int on 32-bit systems, where the size is capped at MaxInt.
	c := n
	if n <= math.MaxInt-n/4 {
		c = n + n/4
	}
	return make([]byte, n, c)
}

// Errors of the zlib wrapper. Callers wrap them in ErrCorrupt.
var (
	errZlibHeader   = errors.New("zlib: invalid header")
	errZlibDict     = errors.New("zlib: preset dictionary")
	errZlibChecksum = errors.New("zlib: invalid checksum")
)

// zlibReader inflates a zlib stream (RFC 1950). It is compress/zlib's reader
// without its allocation per Reset: Go's zlib.Reader.Reset calls
// adler32.New() each time, while here the checksum state and the flate
// decompressor are both created once and reset. The adler32 trailer is
// verified when the deflate stream ends; it is the only check of the
// inflated content, since neither the pack trailer nor the idx CRCs are
// verified.
//
// A zlibReader is not safe for concurrent use.
type zlibReader struct {
	src  flate.Reader
	fl   io.ReadCloser // *flate decompressor, created at the first Reset
	rs   flate.Resetter
	sum  hash.Hash32
	err  error // sticky; io.EOF once the stream and its checksum are verified
	tail [4]byte
}

// Reset starts reading the zlib stream at the current position of src. It
// reads the 2-byte header: deflate with a window of at most 32 KiB, a valid
// check value, no preset dictionary (Git never writes one).
func (z *zlibReader) Reset(src flate.Reader) error {
	z.src, z.err = src, nil
	cmf, err := src.ReadByte()
	if err == nil {
		var flg byte
		if flg, err = src.ReadByte(); err == nil {
			switch {
			case cmf&0x0f != 8 || cmf>>4 > 7 || (uint(cmf)<<8|uint(flg))%31 != 0:
				err = errZlibHeader
			case flg&0x20 != 0:
				err = errZlibDict
			}
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		z.err = err
		return err
	}
	if z.fl == nil {
		z.fl = flate.NewReader(src)
		z.rs = z.fl.(flate.Resetter)
		z.sum = adler32.New()
	} else {
		_ = z.rs.Reset(src, nil) // never fails without a dictionary
		z.sum.Reset()
	}
	return nil
}

func (z *zlibReader) Read(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	n, err := z.fl.Read(p)
	_, _ = z.sum.Write(p[:n]) // never fails
	if err != io.EOF {
		z.err = err
		return n, err
	}
	// End of the deflate stream: the big-endian adler32 of the content.
	if _, err := io.ReadFull(z.src, z.tail[:]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		z.err = err
		return n, err
	}
	if binary.BigEndian.Uint32(z.tail[:]) != z.sum.Sum32() {
		z.err = errZlibChecksum
		return n, z.err
	}
	z.err = io.EOF
	return n, io.EOF
}

// memInflater inflates a zlib stream held in memory.
type memInflater struct {
	zr      zlibReader
	br      bytes.Reader
	scratch [maxLooseHeader]byte // for reads through the io.Reader interface, which escape
}

var memPool = sync.Pool{New: func() any { return new(memInflater) }}

// getMem returns an inflater positioned at the start of src.
func getMem(src []byte) (*memInflater, error) {
	m := memPool.Get().(*memInflater)
	m.br.Reset(src)
	if err := m.zr.Reset(&m.br); err != nil {
		putMem(m)
		return nil, corrupt("zlib header: %v", err)
	}
	return m, nil
}

func putMem(m *memInflater) {
	m.br.Reset(nil)
	memPool.Put(m)
}

// fileInflater inflates a zlib stream read from a file.
type fileInflater struct {
	zr      zlibReader
	br      *bufio.Reader
	scratch [maxLooseHeader]byte
}

var filePool = sync.Pool{New: func() any { return &fileInflater{br: bufio.NewReaderSize(nil, 8192)} }}

func getFile(r io.Reader) (*fileInflater, error) {
	f := filePool.Get().(*fileInflater)
	f.br.Reset(r)
	if err := f.zr.Reset(f.br); err != nil {
		putFile(f)
		return nil, corrupt("zlib header: %v", err)
	}
	return f, nil
}

func putFile(f *fileInflater) {
	f.br.Reset(nil)
	filePool.Put(f)
}

// inflateExact fills dst from zr and checks that the stream ends right
// after, so a stream shorter or longer than declared is corrupt.
func inflateExact(zr io.Reader, dst []byte, probe []byte) error {
	if _, err := io.ReadFull(zr, dst); err != nil {
		return corrupt("inflate: %v", err)
	}
	n, err := zr.Read(probe[:1])
	if n != 0 {
		return corrupt("inflate: object longer than its declared size")
	}
	if !errors.Is(err, io.EOF) {
		return corrupt("inflate: %v", err)
	}
	return nil
}
