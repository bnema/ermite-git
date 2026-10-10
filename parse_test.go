// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestObjectID(t *testing.T) {
	const hex = "0123456789abcdef0123456789abcdef01234567"
	id, err := ParseID(hex)
	if err != nil || id.String() != hex || id.IsZero() {
		t.Fatalf("ParseID = %s %v", id, err)
	}
	if up, err := ParseID(strings.ToUpper(hex)); err != nil || up != id {
		t.Fatalf("upper case: %v", err)
	}
	for _, bad := range []string{"", "abc", hex[:39], hex + "0", strings.Repeat("g", 40), hex[:39] + "\x00"} {
		if _, err := ParseID(bad); !errors.Is(err, ErrInvalidID) {
			t.Errorf("ParseID(%q) = %v", bad, err)
		}
	}
	if !(ObjectID{}).IsZero() {
		t.Error("zero id not zero")
	}
	buf := make([]byte, 0, 64)
	out := id.AppendHex(append(buf, "x:"...))
	if string(out) != "x:"+hex {
		t.Errorf("AppendHex = %s", out)
	}
	if n := testing.AllocsPerRun(100, func() { _ = id.AppendHex(buf[:0]) }); n != 0 {
		t.Errorf("AppendHex allocates %v", n)
	}
}

// varint encodes a delta header size.
func varint(v uint64) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
		} else {
			return append(b, c)
		}
	}
}

func copyOp(off, size uint32) []byte {
	op := byte(0x80)
	var args []byte
	for i := 0; i < 4; i++ {
		if b := byte(off >> (8 * i)); b != 0 {
			op |= 1 << i
			args = append(args, b)
		}
	}
	for i := 0; i < 3; i++ {
		if b := byte(size >> (8 * i)); b != 0 {
			op |= 0x10 << i
			args = append(args, b)
		}
	}
	return append([]byte{op}, args...)
}

func TestApplyDelta(t *testing.T) {
	base := []byte("hello, delta world")
	mk := func(baseSize, resSize uint64, ops ...[]byte) []byte {
		d := append(varint(baseSize), varint(resSize)...)
		for _, o := range ops {
			d = append(d, o...)
		}
		return d
	}
	ins := func(s string) []byte { return append([]byte{byte(len(s))}, s...) }
	good := mk(uint64(len(base)), 12, copyOp(0, 5), ins(" Go"), copyOp(11, 4))
	got, err := patch(base, good, 1<<20)
	if err != nil || string(got) != "hello Go"+string(base[11:15]) {
		t.Fatalf("patch = %q, %v", got, err)
	}
	// copy size 0 means 0x10000
	bigBase := bytes.Repeat([]byte{'x'}, 0x10000)
	got, err = patch(bigBase, mk(0x10000, 0x10000, []byte{0x80}), 1<<20)
	if err != nil || !bytes.Equal(got, bigBase) {
		t.Fatalf("copy 0x10000: %v", err)
	}
	bad := map[string][]byte{
		"empty":            nil,
		"truncated header": {0x80},
		"base size":        mk(3, 1, ins("a")),
		"copy outside":     mk(uint64(len(base)), 4, copyOp(15, 4)),
		"copy huge":        mk(uint64(len(base)), 4, copyOp(0xffffffff, 0xffffff)),
		"insert past end":  mk(uint64(len(base)), 9, []byte{9, 'a'}),
		"result too long":  mk(uint64(len(base)), 1, ins("ab")),
		"result too short": mk(uint64(len(base)), 5, ins("ab")),
		"opcode zero":      mk(uint64(len(base)), 1, []byte{0}),
		"truncated copy":   mk(uint64(len(base)), 1, []byte{0x81}),
		"huge varint":      append(bytes.Repeat([]byte{0xff}, 12), 0x01),
	}
	for name, d := range bad {
		if _, err := patch(base, d, 1<<20); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v, want ErrCorrupt", name, err)
		}
	}
	if _, err := patch(base, mk(uint64(len(base)), 1<<40, ins("a")), 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Errorf("huge result: %v, want ErrTooLarge", err)
	}
}

func TestParseEntryHeader(t *testing.T) {
	mk := func(body ...byte) []byte {
		d := append([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01"), body...)
		return append(d, make([]byte, 20)...)
	}
	// blob, size 5
	h, err := parseEntryHeader(mk(0x35, 0x78, 0x9c), 12)
	if err != nil || h.typ != 3 || h.size != 5 || h.data != 13 {
		t.Fatalf("blob header = %+v %v", h, err)
	}
	// size 0x1234 : 4 low bits 4, then 7 bits ...
	h, err = parseEntryHeader(mk(0xb4, 0x23, 0x78), 12) // type 3 continued
	if err != nil || h.size != 4|0x23<<4 {
		t.Fatalf("multi byte size = %+v %v", h, err)
	}
	for name, d := range map[string][]byte{
		"unterminated":  mk(bytes.Repeat([]byte{0xbf}, 20)...),
		"bad type 0":    mk(0x05, 0x78),
		"bad type 5":    mk(0x55, 0x78),
		"ofs no data":   mk(0x65),
		"ofs outside":   mk(0x65, 0x7f, 0x78),
		"ref truncated": mk(0x75, 1, 2, 3),
	} {
		off := uint64(12)
		if _, err := parseEntryHeader(d, off); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := parseEntryHeader(mk(0x35, 0x78), 5); !errors.Is(err, ErrCorrupt) {
		t.Error("offset inside the pack header")
	}
	if _, err := parseEntryHeader(mk(0x35, 0x78), 1<<40); !errors.Is(err, ErrCorrupt) {
		t.Error("offset far outside")
	}
	if _, err := parseEntryHeader(nil, 12); !errors.Is(err, ErrCorrupt) {
		t.Error("nil pack")
	}
}

func TestParseTreeErrors(t *testing.T) {
	good := append([]byte("100644 a\x00"), bytes.Repeat([]byte{1}, 20)...)
	good = append(good, append([]byte("40000 d\x00"), bytes.Repeat([]byte{2}, 20)...)...)
	n := 0
	if err := ParseTree(good, func(e Entry) bool { n++; return true }); err != nil || n != 2 {
		t.Fatalf("good tree: %d %v", n, err)
	}
	n = 0
	if err := ParseTree(good, func(e Entry) bool { n++; return false }); err != nil || n != 1 {
		t.Fatalf("early stop: %d %v", n, err)
	}
	for name, d := range map[string][]byte{
		"no space":     []byte("100644"),
		"mode letters": []byte("10x644 a\x00" + strings.Repeat("a", 20)),
		"empty mode":   []byte(" a\x00" + strings.Repeat("a", 20)),
		"long mode":    []byte("10000000 a\x00" + strings.Repeat("a", 20)),
		"no nul":       []byte("100644 abc"),
		"short id":     []byte("100644 a\x00" + strings.Repeat("a", 19)),
		"empty name":   []byte("100644 \x00" + strings.Repeat("a", 20)),
	} {
		if err := ParseTree(d, func(Entry) bool { return true }); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := ParseTree(nil, func(Entry) bool { t.Fatal("called"); return true }); err != nil {
		t.Errorf("empty tree: %v", err)
	}
}

func TestParseCommitEdgeCases(t *testing.T) {
	tree := strings.Repeat("a", 40)
	par := strings.Repeat("b", 40)
	var c Commit
	// No message at all, no trailing newline.
	if err := ParseCommit([]byte("tree "+tree+"\nauthor A <a@x> 1 +0000\ncommitter B <b@x> 2 -0130"), &c); err != nil {
		t.Fatal(err)
	}
	if c.Message != nil || c.Committer.Offset != -(3600+30*60) || len(c.Parents) != 0 {
		t.Errorf("commit = %+v", c)
	}
	// Two parents then reuse with zero parents keeps no stale state.
	if err := ParseCommit([]byte("tree "+tree+"\nparent "+par+"\nparent "+tree+"\nauthor A <a@x> 1 +0000\n\nmsg\n"), &c); err != nil || len(c.Parents) != 2 || string(c.Message) != "msg\n" {
		t.Fatalf("two parents: %+v %v", c, err)
	}
	if err := ParseCommit([]byte("tree "+tree+"\n\n"), &c); err != nil || len(c.Parents) != 0 || c.Author.Name != nil || len(c.Message) != 0 {
		t.Fatalf("reuse: %+v %v", c, err)
	}
	// Multi-line header continuation containing something that looks like a header.
	raw := "tree " + tree + "\nmergetag object x\n tag y\n parent " + par + "\n\n  \nauthor A <a@x> 1 +0000\ncommitter C <c@x> 5 +0100\n\nbody"
	if err := ParseCommit([]byte(raw), &c); err != nil || len(c.Parents) != 0 {
		t.Fatalf("continuation: %+v %v", c, err)
	}
	// Odd signatures.
	for in, want := range map[string]Signature{
		"Name <e> 12 +0200":                {Name: []byte("Name"), Email: []byte("e"), Time: 12, Offset: 7200},
		"<> 0 +0000":                       {Name: []byte{}, Email: []byte{}},
		"No Email 1 +0000":                 {Name: []byte("No Email 1 +0000")},
		"A <b> notatime +0100":             {Name: []byte("A"), Email: []byte("b")},
		"A <b> 5 +01":                      {Name: []byte("A"), Email: []byte("b"), Time: 5},
		"A <b <c> 7 -0000":                 {Name: []byte("A"), Email: []byte("b <c"), Time: 7},
		"A <b> 99999999999999999999 +0000": {Name: []byte("A"), Email: []byte("b")},
	} {
		got := parseSignature([]byte(in))
		if !bytes.Equal(got.Name, want.Name) || !bytes.Equal(got.Email, want.Email) || got.Time != want.Time || got.Offset != want.Offset {
			t.Errorf("parseSignature(%q) = %+v %q %q, want %+v", in, got, got.Name, got.Email, want)
		}
	}
	for name, d := range map[string]string{
		"empty":            "",
		"no tree":          "author A <a@x> 1 +0000\n\nmsg",
		"bad tree":         "tree xyz\n\n",
		"short tree":       "tree " + tree[:39] + "\n\n",
		"bad parent":       "tree " + tree + "\nparent zz\n\n",
		"tree second":      "author A <a@x> 1 +0000\ntree " + tree + "\n\n",
		"parent first":     "parent " + tree + "\ntree " + tree + "\n\n",
		"blank first":      "\ntree " + tree + "\n",
		"bad parent after": "tree " + tree + "\nparent " + tree + "\nparent x\n\n",
	} {
		if err := ParseCommit([]byte(d), &c); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseTagErrors(t *testing.T) {
	obj := strings.Repeat("c", 40)
	var tg Tag
	if err := ParseTag([]byte("object "+obj+"\ntype commit\ntag v1\ntagger T <t@x> 3 +0000\n\nmsg"), &tg); err != nil || tg.Type != TypeCommit || string(tg.Name) != "v1" || string(tg.Message) != "msg" {
		t.Fatalf("tag = %+v %v", tg, err)
	}
	for name, d := range map[string]string{
		"empty":    "",
		"no type":  "object " + obj + "\n",
		"no obj":   "type commit\n",
		"bad type": "object " + obj + "\ntype thing\n",
		"bad obj":  "object zz\ntype commit\n",
	} {
		if err := ParseTag([]byte(d), &tg); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParsePackedRefsFormats(t *testing.T) {
	a, b, c := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	p, err := parsePackedRefs("# pack-refs with: peeled fully-peeled sorted \n" + a + " refs/heads/main\n" + b + " refs/tags/v1\n^" + c + "\n" + c + " refs/tags/v2\n")
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := p.find("refs/tags/v1"); !e.peeledKnown || e.peeled.String() != c {
		t.Errorf("v1 = %+v", e)
	}
	if e, _ := p.find("refs/heads/main"); !e.peeledKnown || e.peeled.String() != a {
		t.Errorf("fully peeled: main = %+v", e)
	}
	// Without the header nothing is known.
	p, err = parsePackedRefs(a + " refs/tags/v1\n")
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := p.find("refs/tags/v1"); e.peeledKnown {
		t.Errorf("unknown peel claimed known")
	}
	// Old "peeled" trait: tags known, branches not.
	p, _ = parsePackedRefs("# pack-refs with: peeled\n" + a + " refs/heads/x\n" + a + " refs/tags/t\n")
	if e, _ := p.find("refs/heads/x"); e.peeledKnown {
		t.Error("branch peel known with the peeled trait")
	}
	if e, _ := p.find("refs/tags/t"); !e.peeledKnown {
		t.Error("tag peel unknown with the peeled trait")
	}
	for _, bad := range []string{"^" + a + "\n", a + "\n", "zz refs/x\n", a + " \n", a + " refs/x\n^zz\n"} {
		if _, err := parsePackedRefs(bad); !errors.Is(err, ErrCorrupt) {
			t.Errorf("parsePackedRefs(%q) = %v", bad, err)
		}
	}
}

func TestValidRefName(t *testing.T) {
	for _, ok := range []string{"refs/heads/main", "refs/heads/a/b", "refs/tags/v1.0", "refs/heads/é"} {
		if !validRefName(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "HEAD", "refs", "refs/", "/refs/heads/x", "refs/heads/x/", "refs/heads/../x", "refs/heads/a..b", "refs/heads/.x", "refs/heads/x.lock",
		"refs/heads//x", "refs/heads/a b", "refs/heads/a~1", "refs/heads/a^", "refs/heads/a:b", "refs/heads/a?", "refs/heads/a*", "refs/heads/a[", "refs/heads/a\\b", "refs/heads/a@{1}", "refs/heads/\x01"} {
		if validRefName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// zlibBytes compresses b.
func zlibBytes(b []byte) []byte {
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}

// entryBytes encodes a pack entry header followed by data.
func entryBytes(typ int, size int, extra []byte, data []byte) []byte {
	c := byte(typ<<4) | byte(size&0x0f)
	size >>= 4
	var out []byte
	for size > 0 {
		out = append(out, c|0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, c)
	out = append(out, extra...)
	return append(out, zlibBytes(data)...)
}

// buildPack assembles a pack file and its v2 index from entries (whose ids
// are given) and returns both.
func buildPack(entries [][]byte, ids []ObjectID, offs *[]uint64) (pack, idx []byte) {
	pack = append([]byte("PACK"), 0, 0, 0, 2, 0, 0, 0, byte(len(entries)))
	for _, e := range entries {
		*offs = append(*offs, uint64(len(pack)))
		pack = append(pack, e...)
	}
	sum := sha1.Sum(pack)
	pack = append(pack, sum[:]...)

	order := make([]int, len(ids))
	for i := range order {
		order[i] = i
	}
	for i := range order { // insertion sort by id
		for j := i; j > 0 && bytes.Compare(ids[order[j]][:], ids[order[j-1]][:]) < 0; j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}
	idx = append([]byte{0xff, 't', 'O', 'c', 0, 0, 0, 2}, make([]byte, 1024)...)
	for i, k := range order {
		for b := int(ids[k][0]); b < 256; b++ {
			cur := binary.BigEndian.Uint32(idx[8+4*b:])
			if cur < uint32(i+1) {
				binary.BigEndian.PutUint32(idx[8+4*b:], uint32(i+1))
			}
		}
	}
	for _, k := range order {
		idx = append(idx, ids[k][:]...)
	}
	idx = append(idx, make([]byte, 4*len(order))...) // crc
	for _, k := range order {
		idx = binary.BigEndian.AppendUint32(idx, uint32((*offs)[k]))
	}
	idx = append(idx, pack[len(pack)-20:]...)
	isum := sha1.Sum(idx)
	return pack, append(idx, isum[:]...)
}

func objectIDOf(typ string, data []byte) ObjectID {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	_, _ = h.Write(data)
	var id ObjectID
	copy(id[:], h.Sum(nil))
	return id
}

func TestParseIdxErrors(t *testing.T) {
	var offs []uint64
	blob := []byte("hello")
	_, idx := buildPack([][]byte{entryBytes(3, len(blob), nil, blob)}, []ObjectID{objectIDOf("blob", blob)}, &offs)
	x, err := parseIdx(idx)
	if err != nil || x.n != 1 {
		t.Fatalf("good idx: %v", err)
	}
	if _, ok := x.find(&ObjectID{}); ok {
		t.Error("found a missing id")
	}
	id := objectIDOf("blob", blob)
	if pos, ok := x.find(&id); !ok || pos != 0 {
		t.Error("id not found")
	}
	trunc := func(n int) []byte { return idx[:n] }
	mut := func(f func(b []byte)) []byte { b := bytes.Clone(idx); f(b); return b }
	for name, d := range map[string][]byte{
		"empty":          nil,
		"short":          trunc(100),
		"no trailer":     trunc(len(idx) - 41),
		"magic":          mut(func(b []byte) { b[0] = 0 }),
		"version 3":      mut(func(b []byte) { b[7] = 3 }),
		"fanout down":    mut(func(b []byte) { binary.BigEndian.PutUint32(b[8+4*10:], 5) }),
		"n too big":      mut(func(b []byte) { binary.BigEndian.PutUint32(b[8+4*255:], 1<<31) }),
		"odd large part": append(bytes.Clone(idx), 1, 2, 3),
	} {
		if _, err := parseIdx(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A large offset entry pointing outside of the table.
	d := mut(func(b []byte) {
		off := len(b) - 40 - 4
		binary.BigEndian.PutUint32(b[off:], 0x80000005)
	})
	x, err = parseIdx(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.offset(0); !errors.Is(err, ErrCorrupt) {
		t.Errorf("large offset outside table: %v", err)
	}
}

// TestCorruptPacks mutates a real pack on disk and checks that every read
// either succeeds with the right content or fails with ErrCorrupt, never
// panics.
func TestCorruptPacks(t *testing.T) {
	// A small hand built pack: a blob, a delta against it, a ref-delta.
	base := bytes.Repeat([]byte("0123456789abcdef"), 50)
	res := append(bytes.Clone(base[:400]), []byte("tail")...)
	delta := append(varint(uint64(len(base))), varint(uint64(len(res)))...)
	delta = append(delta, copyOp(0, 400)...)
	delta = append(delta, 4, 't', 'a', 'i', 'l')
	baseID, resID := objectIDOf("blob", base), objectIDOf("blob", res)

	var offs []uint64
	e0 := entryBytes(3, len(base), nil, base)
	// ofs-delta distance to entry 0 is len(e0)
	ofs := []byte{byte(len(e0))}
	if len(e0) >= 128 {
		t.Fatal("fixture: e0 too long for a one byte offset")
	}
	e1 := entryBytes(objOfsDelta, len(delta), ofs, delta)
	res2 := append(bytes.Clone(base[:400]), []byte("TAIL")...)
	delta2 := append(varint(uint64(len(base))), varint(uint64(len(res2)))...)
	delta2 = append(delta2, copyOp(0, 400)...)
	delta2 = append(delta2, 4, 'T', 'A', 'I', 'L')
	e2 := entryBytes(objRefDelta, len(delta2), baseID[:], delta2)
	res2ID := objectIDOf("blob", res2)
	pk, idx := buildPack([][]byte{e0, e1, e2}, []ObjectID{baseID, resID, res2ID}, &offs)

	dir := t.TempDir()
	write := func(pack, idxd []byte) *Repo {
		t.Helper()
		g := dir + "/g" + fmt.Sprint(len(pack)) + fmt.Sprint(len(idxd)) + fmt.Sprint(bytes.Count(pack, []byte{0}))
		writeFile(t, g+"/HEAD", "ref: refs/heads/main\n")
		writeFile(t, g+"/refs/.keep", "")
		name := fmt.Sprintf("pack-%x", pk[len(pk)-20:])
		writeFile(t, g+"/objects/pack/"+name+".pack", string(pack))
		writeFile(t, g+"/objects/pack/"+name+".idx", string(idxd))
		r, err := Open(g)
		if err != nil {
			return nil
		}
		t.Cleanup(func() { _ = r.Close() })
		return r
	}
	// Intact.
	r := write(pk, idx)
	if r == nil {
		t.Fatal("intact pack rejected")
	}
	for id, want := range map[ObjectID][]byte{baseID: base, resID: res, res2ID: res2} {
		_, got, err := r.ReadObject(id, nil)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("intact %s: %v", id, err)
		}
		if _, n, err := r.ObjectHeader(id); err != nil || n != int64(len(want)) {
			t.Fatalf("intact header %s: %d %v", id, n, err)
		}
	}
	// Every single byte flipped in the pack body and truncations.
	cases := 0
	check := func(pack []byte) {
		cases++
		r := write(pack, idx)
		if r == nil {
			return // rejected at Open: fine
		}
		for _, id := range []ObjectID{baseID, resID, res2ID} {
			_, _, err := r.ReadObject(id, nil)
			if err != nil && !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrTooLarge) {
				t.Fatalf("ReadObject: %v is not ErrCorrupt", err)
			}
			_, _, err = r.ObjectHeader(id)
			if err != nil && !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ObjectHeader: %v is not ErrCorrupt", err)
			}
			if _, rc, err := r.OpenBlob(id); err == nil {
				_, rerr := readAll(rc)
				_ = rc.Close()
				if rerr != nil && !errors.Is(rerr, ErrCorrupt) {
					t.Fatalf("blob read: %v is not ErrCorrupt", rerr)
				}
			} else if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrWrongType) {
				t.Fatalf("OpenBlob: %v", err)
			}
		}
	}
	for i := 12; i < len(pk)-20; i++ {
		for _, x := range []byte{0x01, 0x80, 0xff} {
			m := bytes.Clone(pk)
			m[i] ^= x
			check(m)
		}
	}
	for n := 12; n < len(pk); n += 7 {
		check(pk[:n])
	}
	t.Logf("%d corrupted packs", cases)
}

func readAll(rc interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}
