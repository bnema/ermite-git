// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// inflateAll reads a whole zlib stream with the wrapper, reusing z.
func inflateAll(z *zlibReader, src []byte) ([]byte, error) {
	br := bytes.NewReader(src)
	if err := z.Reset(br); err != nil {
		return nil, err
	}
	return io.ReadAll(z)
}

// stdInflate is the reference: compress/zlib on the same bytes.
func stdInflate(src []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

func zlibLevel(b []byte, level int) []byte {
	var out bytes.Buffer
	w, _ := zlib.NewWriterLevel(&out, level)
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}

// zlibSamples are streams of every block type: stored, fixed and dynamic
// Huffman, empty, and several blocks.
func zlibSamples() map[string][]byte {
	rng := rand.New(rand.NewPCG(1, 2))
	random := make([]byte, 1200)
	for i := range random {
		random[i] = byte(rng.Uint32())
	}
	text := textLines(60, 3)
	return map[string][]byte{
		"empty":   zlibLevel(nil, zlib.DefaultCompression),
		"tiny":    zlibLevel([]byte("a"), zlib.DefaultCompression),
		"text":    zlibLevel(text, zlib.DefaultCompression),
		"random":  zlibLevel(random, zlib.DefaultCompression),
		"stored":  zlibLevel(text, zlib.NoCompression),
		"huffman": zlibLevel(text, zlib.HuffmanOnly),
		"fast":    zlibLevel(text, zlib.BestSpeed),
		"big":     zlibLevel(bytes.Repeat(text, 3), zlib.BestCompression),
	}
}

func TestZlibReaderMatchesStdlib(t *testing.T) {
	var z zlibReader // one reader for every stream: Reset must clear all state
	for name, src := range zlibSamples() {
		want, werr := stdInflate(src)
		got, err := inflateAll(&z, src)
		if werr != nil || err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: got %d bytes (%v), stdlib %d bytes (%v)", name, len(got), err, len(want), werr)
		}
		// EOF is sticky and the checksum was verified.
		if n, err := z.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			t.Errorf("%s: Read after the end = %d, %v", name, n, err)
		}
	}
}

func TestZlibReaderHeader(t *testing.T) {
	good := zlibLevel([]byte("hello"), zlib.DefaultCompression)
	mk := func(cmf, flg byte) []byte { return append([]byte{cmf, flg}, good[2:]...) }
	// A valid FCHECK for the given CMF and FLG bits.
	fix := func(cmf, flg byte) []byte {
		flg &^= 0x1f
		flg |= byte(31 - (uint(cmf)<<8|uint(flg))%31)
		return mk(cmf, flg)
	}
	var z zlibReader
	for _, c := range []struct {
		name string
		src  []byte
		want error // nil: accepted
	}{
		{"valid", good, nil},
		{"window 4 KiB", fix(0x28, 0), nil},
		{"bad check", mk(good[0], good[1]+1), errZlibHeader},
		{"not deflate", fix(0x79, 0), errZlibHeader},
		{"window over 32 KiB", fix(0x88, 0), errZlibHeader},
		{"preset dictionary", fix(0x78, 0x20), errZlibDict},
		{"empty", nil, io.ErrUnexpectedEOF},
		{"one byte", good[:1], io.ErrUnexpectedEOF},
	} {
		err := z.Reset(bytes.NewReader(c.src))
		if !errors.Is(err, c.want) && (err != nil || c.want != nil) {
			t.Errorf("%s: Reset = %v, want %v", c.name, err, c.want)
		}
	}
	// A reader that failed its header recovers on the next Reset.
	if got, err := inflateAll(&z, good); err != nil || string(got) != "hello" {
		t.Fatalf("after errors: %q, %v", got, err)
	}
}

// Every byte of the stream matters: a bit flipped in the deflate data or in
// the trailer never yields wrong content without an error. The only flips
// that go unnoticed are those that leave the content as it was (the unused
// bits after the last block).
func TestZlibReaderDetectsCorruption(t *testing.T) {
	var z zlibReader
	for name, src := range zlibSamples() {
		want, err := stdInflate(src)
		if err != nil {
			t.Fatal(err)
		}
		detected := 0
		for i := range src {
			for bit := range 8 {
				bad := bytes.Clone(src)
				bad[i] ^= 1 << bit
				got, err := inflateAll(&z, bad)
				switch {
				case err != nil:
					detected++
				case !bytes.Equal(got, want):
					t.Fatalf("%s: bit %d of byte %d/%d changed the content without an error", name, bit, i, len(src))
				}
			}
		}
		// The 4 trailer bytes are always checked.
		for i := len(src) - 4; i < len(src); i++ {
			bad := bytes.Clone(src)
			bad[i] ^= 0x01
			if _, err := inflateAll(&z, bad); !errors.Is(err, errZlibChecksum) {
				t.Errorf("%s: flipped trailer byte %d: %v, want a checksum error", name, i, err)
			}
		}
		if total := len(src) * 8; detected < total*9/10 {
			t.Errorf("%s: only %d of %d flips detected", name, detected, total)
		}
		// A truncated trailer or stream is an error too.
		for _, n := range []int{len(src) - 1, len(src) - 4, len(src) - 5} {
			if n < 2 {
				continue
			}
			if _, err := inflateAll(&z, src[:n]); err == nil {
				t.Errorf("%s: stream cut at %d of %d accepted", name, n, len(src))
			}
		}
	}
}

// The wrapper answers like compress/zlib on any input, except that it
// refuses a preset dictionary.
func FuzzZlibReader(f *testing.F) {
	for _, src := range zlibSamples() {
		f.Add(src)
		if len(src) > 8 {
			bad := bytes.Clone(src)
			bad[len(src)-2] ^= 0x40
			f.Add(bad)
			f.Add(src[:len(src)-3])
		}
	}
	f.Add([]byte{0x78, 0x20, 0, 0, 0, 1, 0x03, 0x00, 0, 0, 0, 1})
	f.Add([]byte{0x78})
	f.Add([]byte{})
	var z zlibReader
	warm := zlibLevel([]byte("warm up the reused state"), zlib.DefaultCompression)
	f.Fuzz(func(t *testing.T, src []byte) {
		if _, err := inflateAll(&z, warm); err != nil { // dirty state from a good stream
			t.Fatal(err)
		}
		got, err := inflateAll(&z, src)
		if len(src) >= 2 && src[1]&0x20 != 0 && errors.Is(err, errZlibDict) {
			return
		}
		want, werr := stdInflate(src)
		if (err == nil) != (werr == nil) {
			t.Fatalf("wrapper: %v, stdlib: %v", err, werr)
		}
		if err == nil && !bytes.Equal(got, want) {
			t.Fatalf("content differs: %d bytes against %d", len(got), len(want))
		}
	})
}

// flipByteAt flips a bit of the byte at off (from the end when negative).
func flipByteAt(t *testing.T, path string, off int64) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if off < 0 {
		off += int64(len(b))
	}
	b[off] ^= 0x01
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// corruptObject flips a bit in the deflate data or in the adler32 trailer of
// the loose or packed object id (packed: repack first).
func corruptObject(t *testing.T, f *fx, id ObjectID, packed bool, where string) {
	t.Helper()
	var file string
	var entryEnd int64 = -1
	if packed {
		f.git("repack", "-adf")
		f.git("prune-packed")
		matches, _ := filepath.Glob(filepath.Join(f.dir, "objects/pack/*.pack"))
		if len(matches) != 1 {
			t.Fatalf("packs: %v", matches)
		}
		file = matches[0]
		// The blob's entry ends where the next one starts, or at
		// the pack trailer.
		r := f.open()
		p := r.snap.Load().packs[0]
		pos, ok := p.idx.find(&id)
		if !ok {
			t.Fatal("blob not in the pack")
		}
		off, _ := p.idx.offset(pos)
		next := uint64(len(p.data) - packTrailerLen)
		for i := range p.idx.n {
			if o, _ := p.idx.offset(i); o > off && o < next {
				next = o
			}
		}
		entryEnd = int64(next)
		_ = r.Close()
	} else {
		f.git("prune-packed")
		file = filepath.Join(f.dir, "objects", id.String()[:2], id.String()[2:])
	}
	switch {
	case where == "trailer" && packed:
		flipByteAt(t, file, entryEnd-1)
	case where == "trailer":
		flipByteAt(t, file, -1)
	case packed:
		flipByteAt(t, file, entryEnd/2+6) // inside the deflate data
	default:
		st, _ := os.Stat(file)
		flipByteAt(t, file, st.Size()/2)
	}
}

// A flipped byte in the compressed content or in the adler32 trailer of a
// loose or a packed object is ErrCorrupt on every way to read it, since the
// checksum is the only check of the inflated content.
func TestCorruptedObjectsAreErrCorrupt(t *testing.T) {
	content := textLines(400, 9)
	for _, packed := range []bool{false, true} {
		for _, where := range []string{"trailer", "deflate"} {
			t.Run(fmt.Sprintf("packed=%v/%s", packed, where), func(t *testing.T) {
				f := newFx(t)
				f.write("f.txt", content, 0o644)
				f.commit("one")
				id := mustID(t, f.git("rev-parse", "HEAD:f.txt"))
				corruptObject(t, f, id, packed, where)
				r := f.open()
				if _, _, err := r.ReadObject(id, nil); !errors.Is(err, ErrCorrupt) {
					t.Errorf("ReadObject = %v, want ErrCorrupt", err)
				}
				_, rc, err := r.OpenBlob(id)
				if err == nil {
					_, err = io.Copy(io.Discard, rc)
					_ = rc.Close()
				}
				if !errors.Is(err, ErrCorrupt) {
					t.Errorf("OpenBlob = %v, want ErrCorrupt", err)
				}
			})
		}
	}
}

// A corrupt adler32 trailer is reported by readers that stop at the declared
// size too (io.ReadFull, io.CopyN), not only by those that read to EOF, and
// the last chunk is not returned along with a nil error.
func TestOpenBlobTrailerCorruptionOnExactReads(t *testing.T) {
	for _, packed := range []bool{false, true} {
		for _, size := range []int{0, 1, 30, 20000} {
			t.Run(fmt.Sprintf("packed=%v/size=%d", packed, size), func(t *testing.T) {
				f := newFx(t)
				content := textLines(size/16+1, 4)[:size]
				f.write("f.txt", content, 0o644)
				f.commit("one")
				id := mustID(t, f.git("rev-parse", "HEAD:f.txt"))
				corruptObject(t, f, id, packed, "trailer")
				r := f.open()
				open := func() io.ReadCloser {
					n, rc, err := r.OpenBlob(id)
					if err != nil || n != int64(size) {
						t.Fatalf("OpenBlob = %d %v", n, err)
					}
					return rc
				}
				if size > 0 {
					rc := open()
					buf := make([]byte, size)
					if n, err := io.ReadFull(rc, buf); !errors.Is(err, ErrCorrupt) {
						t.Errorf("io.ReadFull = %d %v, want ErrCorrupt", n, err)
					}
					_ = rc.Close()

					rc = open()
					if n, err := io.CopyN(io.Discard, rc, int64(size)); !errors.Is(err, ErrCorrupt) {
						t.Errorf("io.CopyN = %d %v, want ErrCorrupt", n, err)
					}
					_ = rc.Close()

					rc = open()
					if _, err := io.Copy(io.Discard, rc); !errors.Is(err, ErrCorrupt) {
						t.Errorf("io.Copy = %v, want ErrCorrupt", err)
					}
					// The error is sticky.
					if _, err := rc.Read(make([]byte, 1)); !errors.Is(err, ErrCorrupt) {
						t.Errorf("Read after the error = %v, want ErrCorrupt", err)
					}
					_ = rc.Close()

					// One byte at a time: the last Read fails.
					rc = open()
					var err error
					one := make([]byte, 1)
					for i := 0; i < size && err == nil; i++ {
						_, err = rc.Read(one)
					}
					if !errors.Is(err, ErrCorrupt) {
						t.Errorf("byte-wise read = %v, want ErrCorrupt", err)
					}
					_ = rc.Close()
				}
				if size == 0 {
					// Nothing is read from an empty blob: Close reports it.
					rc := open()
					if err := rc.Close(); !errors.Is(err, ErrCorrupt) {
						t.Errorf("Close = %v, want ErrCorrupt", err)
					}
				}
			})
		}
	}
}

// An intact blob read to exactly its size is returned whole and Close is nil,
// and an early Close is not an error.
func TestOpenBlobExactReadsIntact(t *testing.T) {
	for _, packed := range []bool{false, true} {
		f := newFx(t)
		content := textLines(1500, 5)
		f.write("f.txt", content, 0o644)
		f.write("empty", nil, 0o644)
		f.commit("one")
		if packed {
			f.git("repack", "-adf")
		}
		r := f.open()
		id := mustID(t, f.git("rev-parse", "HEAD:f.txt"))
		n, rc, err := r.OpenBlob(id)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(rc, buf); err != nil || !bytes.Equal(buf, content) {
			t.Fatalf("packed=%v: ReadFull: %v", packed, err)
		}
		if err := rc.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
		_, rc, _ = r.OpenBlob(id)
		if _, err := io.CopyN(io.Discard, rc, 10); err != nil {
			t.Fatal(err)
		}
		if err := rc.Close(); err != nil {
			t.Errorf("early Close = %v", err)
		}
		_, rc, err = r.OpenBlob(mustID(t, f.git("rev-parse", "HEAD:empty")))
		if err != nil {
			t.Fatal(err)
		}
		if err := rc.Close(); err != nil {
			t.Errorf("Close of an intact empty blob = %v", err)
		}
	}
}
