// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func okOrCorrupt(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error is not a typed one: %v", err)
	}
}

func FuzzPackEntryHeader(f *testing.F) {
	pack := func(body ...byte) []byte {
		d := append([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01"), body...)
		return append(d, make([]byte, 20)...)
	}
	f.Add(pack(0x35, 0x78, 0x9c, 0x01), uint64(12))
	f.Add(pack(0xb4, 0x23, 0x78), uint64(12))
	f.Add(pack(0x65, 0x05, 0x78), uint64(12))
	f.Add(pack(0x65, 0xff, 0xff, 0xff, 0x7f, 0x78), uint64(12))
	f.Add(append(pack(0x75), bytes.Repeat([]byte{7}, 25)...), uint64(12))
	f.Add(pack(bytes.Repeat([]byte{0xff}, 30)...), uint64(12))
	f.Add([]byte{}, uint64(0))
	f.Fuzz(func(t *testing.T, data []byte, off uint64) {
		h, err := parseEntryHeader(data, off)
		okOrCorrupt(t, err)
		if err == nil {
			if h.data > uint64(len(data)) || (h.typ == objOfsDelta && h.baseOff >= off) {
				t.Fatalf("inconsistent header %+v for %d bytes at %d", h, len(data), off)
			}
		}
	})
}

func FuzzDeltaApply(f *testing.F) {
	base := []byte("hello, delta world")
	good := append(varint(uint64(len(base))), varint(12)...)
	good = append(good, copyOp(0, 5)...)
	good = append(good, 3, ' ', 'G', 'o')
	good = append(good, copyOp(11, 4)...)
	f.Add(base, good)
	f.Add(base, []byte{})
	f.Add(base, []byte{0x12, 0x05, 0x80})
	f.Add([]byte{}, []byte{0x00, 0x00})
	f.Add(base, append(varint(uint64(len(base))), 0xff, 0xff, 0xff, 0xff, 0x0f, 0x90, 0x01))
	f.Add(base, append(append(varint(uint64(len(base))), varint(1<<20)...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff))
	f.Fuzz(func(t *testing.T, base, delta []byte) {
		out, err := patch(base, delta, 1<<20)
		okOrCorrupt(t, err)
		if err == nil {
			_, rs, _, herr := deltaHeader(delta)
			if herr != nil || uint64(len(out)) != rs {
				t.Fatalf("result of %d bytes, header says %d (%v)", len(out), rs, herr)
			}
		}
	})
}

func FuzzParseTree(f *testing.F) {
	f.Add(append([]byte("100644 a\x00"), bytes.Repeat([]byte{1}, 20)...))
	f.Add(append(append([]byte("40000 dir\x00"), bytes.Repeat([]byte{2}, 20)...), append([]byte("160000 sub\x00"), bytes.Repeat([]byte{3}, 20)...)...))
	f.Add([]byte("100644 \x00"))
	f.Add([]byte("100644 name"))
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte("1 \x00"), 30))
	f.Fuzz(func(t *testing.T, data []byte) {
		n := 0
		err := ParseTree(data, func(e Entry) bool {
			n++
			if len(e.Name) == 0 {
				t.Fatal("empty name accepted")
			}
			return true
		})
		okOrCorrupt(t, err)
		if err == nil && n == 0 && len(data) != 0 {
			t.Fatal("non-empty tree without entries")
		}
	})
}

func FuzzParseCommit(f *testing.F) {
	tree, par := strings.Repeat("a", 40), strings.Repeat("b", 40)
	f.Add([]byte("tree " + tree + "\nparent " + par + "\nauthor A <a@x> 1700000000 +0100\ncommitter B <b@x> 1700000001 -0830\n\nmessage\n"))
	f.Add([]byte("tree " + tree + "\ngpgsig -----BEGIN-----\n \n abc\n -----END-----\nencoding latin1\n\n\xe9\xff"))
	f.Add([]byte("tree " + tree))
	f.Add([]byte("tree " + tree + "\nauthor <> 99999999999999999999999 +9999\n\n"))
	f.Add([]byte("tree\n\n"))
	f.Add([]byte("tree " + tree + "\ntree " + par + "\nparent " + par + "\n\n"))
	f.Add([]byte("tree " + tree + "\nparent " + par + "\nauthor A <a@x> 1 +0000\nparent " + tree + "\n\nm"))
	f.Add([]byte("parent " + par + "\ntree " + tree + "\n\n"))
	f.Add([]byte{})
	f.Add([]byte("\n\n\n"))
	var c Commit
	f.Fuzz(func(t *testing.T, data []byte) {
		err := ParseCommit(data, &c)
		okOrCorrupt(t, err)
		if err == nil && len(c.Message) > len(data) {
			t.Fatal("message longer than input")
		}
		if err == nil && !bytes.HasPrefix(data, []byte("tree ")) {
			t.Fatal("accepted a commit whose first header is not the tree")
		}
		if err == nil && len(c.Parents) > 0 {
			// The parents are the lines that follow the tree directly.
			rest := data[bytes.IndexByte(data, '\n')+1:]
			for _, p := range c.Parents {
				want := append([]byte("parent "), []byte(p.String())...)
				if len(rest) < len(want) || !bytes.EqualFold(rest[:len(want)], want) {
					t.Fatalf("parent %s is not on the line after the tree", p)
				}
				rest = rest[bytes.IndexByte(rest, '\n')+1:]
			}
		}
		var tg Tag
		okOrCorrupt(t, ParseTag(data, &tg))
		_ = parseSignature(data)
	})
}

func FuzzIdxLookup(f *testing.F) {
	var offs []uint64
	b1, b2 := []byte("one"), []byte("two")
	_, idx := buildPack([][]byte{entryBytes(3, 3, nil, b1), entryBytes(3, 3, nil, b2)},
		[]ObjectID{objectIDOf("blob", b1), objectIDOf("blob", b2)}, &offs)
	id := objectIDOf("blob", b1)
	f.Add(idx, id[:])
	f.Add(idx[:len(idx)-1], id[:])
	f.Add(idx[:1100], id[:])
	f.Add([]byte{}, id[:])
	big := bytes.Clone(idx)
	big[len(big)-41] |= 0x80
	f.Add(big, id[:])
	f.Fuzz(func(t *testing.T, data, key []byte) {
		x, err := parseIdx(data)
		if err != nil {
			okOrCorrupt(t, err)
			return
		}
		var want ObjectID
		copy(want[:], key)
		if pos, ok := x.find(&want); ok {
			if pos >= x.n || !bytes.Equal(x.id(pos), want[:]) {
				t.Fatalf("find returned position %d for %s", pos, want)
			}
			_, err := x.offset(pos)
			okOrCorrupt(t, err)
		}
		if len(key) > 0 {
			nib := min(len(key)*2, 40)
			x.prefixMatches(key, nib, func(id ObjectID) bool {
				if !hasNibblePrefix(id[:], key, nib) {
					t.Fatalf("prefixMatches yielded %s for %x/%d", id, key, nib)
				}
				return true
			})
		}
		for i := uint32(0); i < min(x.n, 8); i++ {
			_, err := x.offset(i)
			okOrCorrupt(t, err)
		}
	})
}

func FuzzPackedRefsAndConfig(f *testing.F) {
	a := strings.Repeat("a", 40)
	f.Add("# pack-refs with: peeled fully-peeled sorted \n" + a + " refs/heads/main\n" + a + " refs/tags/v1\n^" + a + "\n")
	f.Add("^" + a)
	f.Add("[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectformat = sha1\n")
	f.Add("[")
	f.Add("ref: refs/heads/main\n")
	f.Fuzz(func(t *testing.T, s string) {
		_, err := parsePackedRefs(s)
		okOrCorrupt(t, err)
		okOrCorrupt(t, checkConfigData([]byte(s)))
		_, err = parseRefContent([]byte(s))
		okOrCorrupt(t, err)
		_ = validRefName(s)
	})
}
