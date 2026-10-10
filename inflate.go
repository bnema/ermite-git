// SPDX-License-Identifier: MIT

package git

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"errors"
	"io"
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
	return make([]byte, n, n+n/4)
}

// memInflater inflates a zlib stream held in memory.
type memInflater struct {
	zr      io.ReadCloser
	br      bytes.Reader
	scratch [maxLooseHeader]byte // for reads through the io.Reader interface, which escape
}

var memPool = sync.Pool{New: func() any { return new(memInflater) }}

// getMem returns an inflater positioned at the start of src.
func getMem(src []byte) (*memInflater, error) {
	m := memPool.Get().(*memInflater)
	m.br.Reset(src)
	var err error
	if m.zr == nil {
		m.zr, err = zlib.NewReader(&m.br)
	} else {
		err = m.zr.(zlib.Resetter).Reset(&m.br, nil)
	}
	if err != nil {
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
	zr      io.ReadCloser
	br      *bufio.Reader
	scratch [maxLooseHeader]byte
}

var filePool = sync.Pool{New: func() any { return &fileInflater{br: bufio.NewReaderSize(nil, 8192)} }}

func getFile(r io.Reader) (*fileInflater, error) {
	f := filePool.Get().(*fileInflater)
	f.br.Reset(r)
	var err error
	if f.zr == nil {
		f.zr, err = zlib.NewReader(f.br)
	} else {
		err = f.zr.(zlib.Resetter).Reset(f.br, nil)
	}
	if err != nil {
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
