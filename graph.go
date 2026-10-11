// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
)

// This file reads Git commit-graph files (gitformat-commit-graph): a single
// objects/info/commit-graph or a split chain
// objects/info/commit-graphs/commit-graph-chain plus graph-<hash>.graph
// layers. Files are mmapped read-only and never written.
//
// Lifecycle. openCommitGraph returns a graph holding one reference (the
// owner's). A reader that may outlive a concurrent reopen calls retain
// (which fails once the graph is closed) and release when done. The owner
// calls close when it replaces the graph; close is idempotent, only the first
// call drops the owner's reference. The mappings are unmapped when the last
// reference is dropped; no accessor may be used after that. Layers are
// immutable files named after their checksum, so a concurrent
// "git commit-graph write" never changes a mapped file; it can only delete
// it, which does not affect an existing mapping. It can however delete a
// layer between the moment the chain is read and the moment the layer is
// opened: openCommitGraph then starts again from the chain, and gives up with
// errNoGraph (callers fall back to the objects) if that keeps happening.
//
// File mapping is done by mapFile (mmap_*.go), which falls back to reading
// the file where mmap is unavailable.
//
// Positions are global across the chain: the commits of the base layer come
// first, then those of the next layer, and so on. Within a layer commits are
// sorted by object id.
//
// Every offset read from a file is bounds-checked. Structural problems found
// when opening return an error wrapping errGraphCorrupt; per-commit problems
// (edge list, overflow generation) are reported by the accessor that meets
// them. No accessor panics on malformed input.

var (
	// errGraphCorrupt wraps every error caused by malformed graph content.
	// After merging with the core it becomes an alias of (or is wrapped
	// with) ErrCorrupt.
	errGraphCorrupt = errors.New("corrupt commit-graph")
	// errGraphUnsupported wraps valid graphs this reader does not handle
	// (SHA-256, unknown format version).
	errGraphUnsupported = errors.New("unsupported commit-graph")
	// errNoGraph means the repository has no commit-graph.
	errNoGraph = errors.New("no commit-graph")
	// errGraphPosition is returned for a position >= numCommits.
	errGraphPosition = errors.New("commit-graph position out of range")
)

func graphCorruptf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errGraphCorrupt, fmt.Sprintf(format, args...))
}

const (
	graphHashSize       = 20
	graphHeaderSize     = 8
	graphChunkEntrySize = 12
	graphCommitSize     = graphHashSize + 16 // tree, 2 parents, level+time
	graphFanoutSize     = 256 * 4

	graphParentNone  = 0x70000000 // no parent in this slot
	graphParentExtra = 0x80000000 // second slot: index into the EDGE chunk
	graphEdgeLast    = 0x80000000 // EDGE entry: last parent of the commit
	graphPosMask     = 0x7fffffff
	graphGenOverflow = 0x80000000 // GDA2 entry: index into GDO2

	graphMaxLayers  = 256 // the base count is a single byte
	graphMaxParents = 1 << 13
	graphMaxChain   = graphMaxLayers * (2*graphHashSize + 1)

	chunkOIDF = 'O'<<24 | 'I'<<16 | 'D'<<8 | 'F'
	chunkOIDL = 'O'<<24 | 'I'<<16 | 'D'<<8 | 'L'
	chunkCDAT = 'C'<<24 | 'D'<<16 | 'A'<<8 | 'T'
	chunkEDGE = 'E'<<24 | 'D'<<16 | 'G'<<8 | 'E'
	chunkGDA2 = 'G'<<24 | 'D'<<16 | 'A'<<8 | '2'
	chunkGDO2 = 'G'<<24 | 'D'<<16 | 'O'<<8 | '2'
	chunkBIDX = 'B'<<24 | 'I'<<16 | 'D'<<8 | 'X'
	chunkBDAT = 'B'<<24 | 'D'<<16 | 'A'<<8 | 'T'
	chunkBASE = 'B'<<24 | 'A'<<16 | 'S'<<8 | 'E'
)

// graphLayer is one commit-graph file.
type graphLayer struct {
	data     []byte
	numBase  int    // base graphs declared in the header
	base     uint32 // global position of the first commit
	n        uint32 // commits in this layer
	fanout   []byte
	oidl     []byte
	cdat     []byte
	edge     []byte
	gda2     []byte
	gdo2     []byte
	bidx     []byte
	bdat     []byte
	baseList []byte

	// Changed-path Bloom filter settings, valid when hasBloom.
	hasBloom     bool
	bloomVersion uint32
	bloomHashes  uint32
	bloomBits    uint32
}

// commitGraph is a (possibly split) commit-graph.
type commitGraph struct {
	layers []graphLayer // base first
	total  uint32
	genV2  bool // every layer has GDA2: generation() is a corrected date

	refs   atomic.Int64
	closed atomic.Bool
	unmaps []func() error
}

// graphOpenRetries is how many times openCommitGraph starts over when a
// layer disappears under it, after the first attempt.
const graphOpenRetries = 3

// graphBeforeLayerOpen, when set by a test, is called with the path of each
// chain layer right before it is opened.
var graphBeforeLayerOpen func(path string)

// graphMmap maps a file read-only. The returned function releases the
// mapping.
func graphMmap(path string) ([]byte, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil, graphCorruptf("%s: not a regular file", filepath.Base(path))
	}
	size := st.Size()
	if size < graphHeaderSize+graphChunkEntrySize+graphHashSize {
		return nil, nil, graphCorruptf("%s: file too short (%d bytes)", filepath.Base(path), size)
	}
	if size > math.MaxInt32 {
		return nil, nil, graphCorruptf("%s: file too large (%d bytes)", filepath.Base(path), size)
	}
	data, unmap, err := mapFile(f, size)
	if err != nil {
		return nil, nil, fmt.Errorf("map %s: %w", filepath.Base(path), err)
	}
	return data, unmap, nil
}

// openCommitGraph opens the commit-graph of the objects directory objectsDir:
// objects/info/commit-graph if present, else the split chain. It returns
// errNoGraph when there is none. The result holds one reference.
//
// A concurrent "git commit-graph write --split" can merge layers and delete
// the old ones between the read of the chain and the open of a layer. The
// chain is then read again, up to graphOpenRetries more times; if layers keep
// disappearing the error wraps errNoGraph and the cause, so that callers fall
// back to reading objects.
func openCommitGraph(objectsDir string) (*commitGraph, error) {
	var gone error
	for attempt := 0; attempt <= graphOpenRetries; attempt++ {
		g, err := openCommitGraphOnce(objectsDir)
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			return g, err
		}
		gone = err
	}
	return nil, fmt.Errorf("%w: layers kept disappearing: %w", errNoGraph, gone)
}

func openCommitGraphOnce(objectsDir string) (*commitGraph, error) {
	infoDir := filepath.Join(objectsDir, "info")
	data, unmap, err := graphMmap(filepath.Join(infoDir, "commit-graph"))
	switch {
	case err == nil:
		l, perr := parseGraphLayer(data)
		if perr != nil {
			_ = unmap()
			return nil, fmt.Errorf("commit-graph: %w", perr)
		}
		g, aerr := assembleGraph([]*graphLayer{l}, nil)
		if aerr != nil {
			_ = unmap()
			return nil, fmt.Errorf("commit-graph: %w", aerr)
		}
		g.unmaps = []func() error{unmap}
		return g, nil
	case errors.Is(err, fs.ErrNotExist):
		return openCommitGraphChain(infoDir)
	default:
		return nil, fmt.Errorf("commit-graph: %w", err)
	}
}

func openCommitGraphChain(infoDir string) (*commitGraph, error) {
	dir := filepath.Join(infoDir, "commit-graphs")
	names, err := readGraphChain(filepath.Join(dir, "commit-graph-chain"))
	if err != nil {
		return nil, err
	}
	layers := make([]*graphLayer, 0, len(names))
	unmaps := make([]func() error, 0, len(names))
	cleanup := func() {
		for _, u := range unmaps {
			_ = u()
		}
	}
	for _, name := range names {
		file := "graph-" + hex.EncodeToString(name[:]) + ".graph"
		path := filepath.Join(dir, file)
		if graphBeforeLayerOpen != nil {
			graphBeforeLayerOpen(path)
		}
		data, unmap, err := graphMmap(path)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("commit-graph chain layer: %w", err)
		}
		unmaps = append(unmaps, unmap)
		l, err := parseGraphLayer(data)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("commit-graph layer %s: %w", file, err)
		}
		layers = append(layers, l)
	}
	g, err := assembleGraph(layers, names)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("commit-graph chain: %w", err)
	}
	g.unmaps = unmaps
	return g, nil
}

// readGraphChain reads commit-graph-chain.
func readGraphChain(path string) ([][graphHashSize]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errNoGraph
		}
		return nil, fmt.Errorf("commit-graph chain: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, graphMaxChain+1))
	if err != nil {
		return nil, fmt.Errorf("commit-graph chain: %w", err)
	}
	return parseGraphChain(raw)
}

// parseGraphChain parses the content of commit-graph-chain: one lowercase
// hex checksum per line, base layer first.
func parseGraphChain(raw []byte) ([][graphHashSize]byte, error) {
	if len(raw) == 0 {
		return nil, errNoGraph
	}
	const line = 2*graphHashSize + 1
	if len(raw) > graphMaxChain || len(raw)%line != 0 {
		return nil, graphCorruptf("commit-graph-chain: bad size %d", len(raw))
	}
	names := make([][graphHashSize]byte, len(raw)/line)
	for i := range names {
		row := raw[i*line : (i+1)*line]
		if row[line-1] != '\n' {
			return nil, graphCorruptf("commit-graph-chain: line %d not terminated", i+1)
		}
		if _, err := hex.Decode(names[i][:], row[:line-1]); err != nil {
			return nil, graphCorruptf("commit-graph-chain: line %d: %v", i+1, err)
		}
	}
	return names, nil
}

// parseGraphLayer validates the header, the chunk table and the sizes of the
// chunks it understands. It does not verify the checksum nor the content of
// individual records.
func parseGraphLayer(data []byte) (*graphLayer, error) {
	if len(data) < graphHeaderSize+graphChunkEntrySize+graphHashSize {
		return nil, graphCorruptf("file too short (%d bytes)", len(data))
	}
	if string(data[:4]) != "CGPH" {
		return nil, graphCorruptf("bad signature %q", data[:4])
	}
	if data[4] != 1 {
		return nil, fmt.Errorf("%w: version %d", errGraphUnsupported, data[4])
	}
	switch data[5] {
	case 1:
	case 2:
		return nil, fmt.Errorf("%w: SHA-256", errGraphUnsupported)
	default:
		return nil, graphCorruptf("unknown hash version %d", data[5])
	}
	nChunks := int(data[6])
	l := &graphLayer{data: data, numBase: int(data[7])}

	tableEnd := uint64(graphHeaderSize + (nChunks+1)*graphChunkEntrySize)
	bodyEnd := uint64(len(data) - graphHashSize)
	if tableEnd > bodyEnd {
		return nil, graphCorruptf("chunk table exceeds file")
	}

	var ids [256]uint32
	prevOff := tableEnd
	for i := 0; i <= nChunks; i++ {
		ent := data[graphHeaderSize+i*graphChunkEntrySize:]
		id := binary.BigEndian.Uint32(ent)
		off := binary.BigEndian.Uint64(ent[4:])
		if (id == 0) != (i == nChunks) {
			return nil, graphCorruptf("chunk table: bad terminator at entry %d", i)
		}
		if off < prevOff || off > bodyEnd {
			return nil, graphCorruptf("chunk table: bad offset %d for entry %d", off, i)
		}
		if i > 0 {
			// Close the previous chunk: it spans [its offset, this offset).
			if err := l.setChunk(ids[i-1], data[prevOff:off]); err != nil {
				return nil, err
			}
		}
		for j := 0; j < i; j++ {
			if ids[j] == id {
				return nil, graphCorruptf("chunk table: duplicate chunk %08x", id)
			}
		}
		ids[i] = id
		prevOff = off
	}

	if err := l.validate(); err != nil {
		return nil, err
	}
	return l, nil
}

// setChunk records chunk id. Unknown chunks (and the obsolete GDAT/GDOV) are
// ignored.
func (l *graphLayer) setChunk(id uint32, b []byte) error {
	switch id {
	case chunkOIDF:
		l.fanout = b
	case chunkOIDL:
		l.oidl = b
	case chunkCDAT:
		l.cdat = b
	case chunkEDGE:
		l.edge = b
	case chunkGDA2:
		l.gda2 = b
	case chunkGDO2:
		l.gdo2 = b
	case chunkBIDX:
		l.bidx = b
	case chunkBDAT:
		l.bdat = b
	case chunkBASE:
		l.baseList = b
	}
	return nil
}

func (l *graphLayer) validate() error {
	if len(l.fanout) != graphFanoutSize {
		return graphCorruptf("OIDF chunk missing or has size %d", len(l.fanout))
	}
	var prev uint32
	for i := 0; i < 256; i++ {
		v := binary.BigEndian.Uint32(l.fanout[i*4:])
		if v < prev {
			return graphCorruptf("OIDF chunk not monotonic at %d", i)
		}
		prev = v
	}
	if prev >= graphParentNone {
		return graphCorruptf("too many commits (%d)", prev)
	}
	l.n = prev
	n := uint64(prev)
	if l.oidl == nil || uint64(len(l.oidl)) != n*graphHashSize {
		return graphCorruptf("OIDL chunk missing or has size %d for %d commits", len(l.oidl), n)
	}
	if l.cdat == nil || uint64(len(l.cdat)) != n*graphCommitSize {
		return graphCorruptf("CDAT chunk missing or has size %d for %d commits", len(l.cdat), n)
	}
	if len(l.edge)%4 != 0 {
		return graphCorruptf("EDGE chunk size %d not a multiple of 4", len(l.edge))
	}
	// Optional chunks of the wrong size disable their feature in this layer,
	// as in git, instead of failing the layer. Without GDA2 in every layer
	// generation() falls back to topological levels (see assembleGraph).
	if l.gda2 != nil && uint64(len(l.gda2)) != n*4 {
		l.gda2 = nil
	}
	if len(l.gdo2)%8 != 0 {
		return graphCorruptf("GDO2 chunk size %d not a multiple of 8", len(l.gdo2))
	}
	if l.numBase > 0 && uint64(len(l.baseList)) != uint64(l.numBase)*graphHashSize {
		return graphCorruptf("BASE chunk has size %d for %d base graphs", len(l.baseList), l.numBase)
	}
	if l.numBase == 0 && len(l.baseList) != 0 {
		return graphCorruptf("BASE chunk present without base graphs")
	}
	// Bloom filters need both chunks, correctly sized; otherwise git ignores
	// them (the layer has no changed-path data).
	if l.bidx != nil && l.bdat != nil && uint64(len(l.bidx)) == n*4 && len(l.bdat) >= bloomHeaderSize {
		l.bloomVersion = binary.BigEndian.Uint32(l.bdat)
		l.bloomHashes = binary.BigEndian.Uint32(l.bdat[4:])
		l.bloomBits = binary.BigEndian.Uint32(l.bdat[8:])
		// Unknown versions are ignored like git does; an absurd hash
		// count would only make queries slow.
		l.hasBloom = (l.bloomVersion == 1 || l.bloomVersion == 2) &&
			l.bloomHashes >= 1 && l.bloomHashes <= bloomMaxHashes
	}
	return nil
}

// assembleGraph builds a graph from validated layers, base first. names are
// the chain entries (checksums) of the layers, nil for a single file.
func assembleGraph(layers []*graphLayer, names [][graphHashSize]byte) (*commitGraph, error) {
	if len(layers) == 0 {
		return nil, errNoGraph
	}
	if len(layers) > graphMaxLayers {
		return nil, graphCorruptf("too many layers (%d)", len(layers))
	}
	if names != nil && len(names) != len(layers) {
		return nil, graphCorruptf("%d layers for %d chain entries", len(layers), len(names))
	}
	if names == nil && len(layers) > 1 {
		return nil, graphCorruptf("%d layers without chain entries", len(layers))
	}
	g := &commitGraph{layers: make([]graphLayer, len(layers)), genV2: true}
	var total uint64
	for k, l := range layers {
		if l.numBase != k {
			return nil, graphCorruptf("layer %d declares %d base graphs", k, l.numBase)
		}
		for i := 0; i < k; i++ {
			if !bytes.Equal(l.baseList[i*graphHashSize:(i+1)*graphHashSize], names[i][:]) {
				return nil, graphCorruptf("layer %d: base graph %d does not match the chain", k, i)
			}
		}
		if names != nil && !bytes.Equal(l.data[len(l.data)-graphHashSize:], names[k][:]) {
			return nil, graphCorruptf("layer %d: checksum does not match its name in the chain", k)
		}
		l.base = uint32(total)
		total += uint64(l.n)
		if total >= graphParentNone {
			return nil, graphCorruptf("too many commits in chain")
		}
		if l.gda2 == nil {
			g.genV2 = false
		}
		g.layers[k] = *l
	}
	g.total = uint32(total)
	g.refs.Store(1)
	return g, nil
}

// retain adds a reference. It reports false once the graph is closed, even
// if readers still hold references.
func (g *commitGraph) retain() bool {
	if g.closed.Load() {
		return false
	}
	for {
		n := g.refs.Load()
		if n <= 0 {
			return false
		}
		if g.refs.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// release drops a reference; the last one unmaps the files.
func (g *commitGraph) release() error {
	switch n := g.refs.Add(-1); {
	case n > 0:
		return nil
	case n == 0:
		var first error
		for _, unmap := range g.unmaps {
			if err := unmap(); err != nil && first == nil {
				first = err
			}
		}
		g.unmaps = nil
		return first
	default:
		g.refs.Add(1)
		return errors.New("commit-graph released too many times")
	}
}

// close drops the owner's reference. Only the first call has an effect, so
// calling it again never drops a reference held by a reader.
func (g *commitGraph) close() error {
	if !g.closed.CompareAndSwap(false, true) {
		return nil
	}
	return g.release()
}

// numCommits is the number of commits in the whole chain.
func (g *commitGraph) numCommits() uint32 { return g.total }

// numLayers is the number of files in the chain.
func (g *commitGraph) numLayers() int { return len(g.layers) }

// hasGenerationV2 reports whether generation returns corrected commit dates
// (GDA2 present in every layer, as git requires) rather than topological
// levels. The two must not be compared with each other.
func (g *commitGraph) hasGenerationV2() bool { return g.genV2 }

// layerOf finds the layer holding the global position pos and the index of
// the commit inside it.
func (g *commitGraph) layerOf(pos uint32) (*graphLayer, uint32) {
	if pos >= g.total {
		return nil, 0
	}
	for i := len(g.layers) - 1; i >= 0; i-- {
		if l := &g.layers[i]; pos >= l.base {
			return l, pos - l.base
		}
	}
	return nil, 0
}

// find looks up id in one layer using the fanout table.
func (l *graphLayer) find(id *[graphHashSize]byte) (uint32, bool) {
	b := int(id[0])
	var lo uint32
	if b > 0 {
		lo = binary.BigEndian.Uint32(l.fanout[(b-1)*4:])
	}
	hi := binary.BigEndian.Uint32(l.fanout[b*4:]) // <= l.n, checked when opening
	for lo < hi {
		mid := lo + (hi-lo)/2
		off := int(mid) * graphHashSize
		switch c := bytes.Compare(l.oidl[off:off+graphHashSize], id[:]); {
		case c == 0:
			return mid, true
		case c < 0:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return 0, false
}

// lookup returns the global position of commit id. It does not allocate.
func (g *commitGraph) lookup(id [graphHashSize]byte) (uint32, bool) {
	for i := len(g.layers) - 1; i >= 0; i-- {
		l := &g.layers[i]
		if idx, ok := l.find(&id); ok {
			return l.base + idx, true
		}
	}
	return 0, false
}

// commitID returns the object id at pos.
func (g *commitGraph) commitID(pos uint32) (id [graphHashSize]byte, err error) {
	l, idx := g.layerOf(pos)
	if l == nil {
		return id, errGraphPosition
	}
	copy(id[:], l.oidl[int(idx)*graphHashSize:])
	return id, nil
}

// record returns the CDAT record of pos.
func (g *commitGraph) record(pos uint32) (*graphLayer, []byte, error) {
	l, idx := g.layerOf(pos)
	if l == nil {
		return nil, nil, errGraphPosition
	}
	off := int(idx) * graphCommitSize
	return l, l.cdat[off : off+graphCommitSize], nil
}

// rootTree returns the root tree id of the commit at pos.
func (g *commitGraph) rootTree(pos uint32) (id [graphHashSize]byte, err error) {
	_, rec, err := g.record(pos)
	if err != nil {
		return id, err
	}
	copy(id[:], rec)
	return id, nil
}

// commitTime returns the commit time in seconds since the epoch (34 bits).
func (g *commitGraph) commitTime(pos uint32) (int64, error) {
	_, rec, err := g.record(pos)
	if err != nil {
		return 0, err
	}
	return graphRecordTime(rec), nil
}

func graphRecordTime(rec []byte) int64 {
	hi := uint64(binary.BigEndian.Uint32(rec[graphHashSize+8:]) & 3)
	lo := uint64(binary.BigEndian.Uint32(rec[graphHashSize+12:]))
	return int64(hi<<32 | lo)
}

// topoLevel returns the topological level (generation number v1) of pos;
// 0 means "not computed".
func (g *commitGraph) topoLevel(pos uint32) (uint32, error) {
	_, rec, err := g.record(pos)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(rec[graphHashSize+8:]) >> 2, nil
}

// generation returns the corrected commit date of pos when
// hasGenerationV2, else its topological level. In both cases a commit has a
// strictly greater generation than each of its parents.
func (g *commitGraph) generation(pos uint32) (uint64, error) {
	l, idx := g.layerOf(pos)
	if l == nil {
		return 0, errGraphPosition
	}
	off := int(idx) * graphCommitSize
	rec := l.cdat[off : off+graphCommitSize]
	if !g.genV2 {
		return uint64(binary.BigEndian.Uint32(rec[graphHashSize+8:]) >> 2), nil
	}
	v := binary.BigEndian.Uint32(l.gda2[int(idx)*4:])
	offset := uint64(v)
	if v&graphGenOverflow != 0 {
		oi := uint64(v & graphPosMask)
		if (oi+1)*8 > uint64(len(l.gdo2)) {
			return 0, graphCorruptf("GDA2 overflow index %d beyond GDO2 (%d bytes)", oi, len(l.gdo2))
		}
		offset = binary.BigEndian.Uint64(l.gdo2[oi*8:])
		if offset >= 1<<62 {
			return 0, graphCorruptf("GDO2 offset %d out of range", offset)
		}
	}
	return uint64(graphRecordTime(rec)) + offset, nil
}

// appendParents appends the parent positions of pos, in commit order, to dst
// and returns it. It allocates only if dst has to grow (octopus merges).
func (g *commitGraph) appendParents(dst []uint32, pos uint32) ([]uint32, error) {
	l, rec, err := g.record(pos)
	if err != nil {
		return dst, err
	}
	// Parents are in this layer or in a lower one.
	limit := l.base + l.n
	p1 := binary.BigEndian.Uint32(rec[graphHashSize:])
	p2 := binary.BigEndian.Uint32(rec[graphHashSize+4:])
	if p1 == graphParentNone {
		if p2 != graphParentNone {
			return dst, graphCorruptf("commit %d has a second parent but no first", pos)
		}
		return dst, nil
	}
	if p1 >= limit {
		return dst, graphCorruptf("commit %d: parent %d out of range", pos, p1)
	}
	dst = append(dst, p1)
	switch {
	case p2 == graphParentNone:
		return dst, nil
	case p2&graphParentExtra == 0:
		if p2 >= limit {
			return dst, graphCorruptf("commit %d: parent %d out of range", pos, p2)
		}
		return append(dst, p2), nil
	}
	count := 1
	for e := uint64(p2 & graphPosMask); ; e++ {
		if (e+1)*4 > uint64(len(l.edge)) {
			return dst, graphCorruptf("commit %d: EDGE list overruns the chunk", pos)
		}
		v := binary.BigEndian.Uint32(l.edge[e*4:])
		p := v & graphPosMask
		if p >= limit {
			return dst, graphCorruptf("commit %d: parent %d out of range", pos, p)
		}
		if count++; count > graphMaxParents {
			return dst, graphCorruptf("commit %d: too many parents", pos)
		}
		dst = append(dst, p)
		if v&graphEdgeLast != 0 {
			return dst, nil
		}
	}
}

// graphFileStamp identifies a version of a file for change detection.
type graphFileStamp struct {
	exists bool
	size   int64
	mtime  int64
	ino    uint64
}

// graphStamp identifies the on-disk commit-graph state: the single file and
// the chain file (layers are immutable and named by checksum).
type graphStamp struct{ single, chain graphFileStamp }

// trusted reports whether unchanged stat data proves that the files did not
// change, for a stamp taken at the given time (unix nanoseconds). This is
// Git's racy rule, the one of fileSig.same: a write that follows within the
// clock granularity of the previous modification can leave the stat data as
// it was, so the data only proves anything once the newest modification is
// older than racyWindow when the stamp was taken. Git writes graph files by
// rename, which already changes the inode; the rule covers the rest. The
// time is that of the stamp, not of the last reload: measuring from the
// reload would restart the window each time and reload a busy repository
// forever.
func (s graphStamp) trusted(at int64) bool {
	newest := int64(math.MinInt64)
	for _, f := range [...]graphFileStamp{s.single, s.chain} {
		if f.exists {
			newest = max(newest, f.mtime)
		}
	}
	return newest == math.MinInt64 || at-newest > int64(racyWindow)
}

// graphPaths are the paths statGraph looks at, built once.
type graphPaths struct{ single, chain statPath }

func newGraphPaths(objectsDir string) graphPaths {
	return graphPaths{
		single: newStatPath(objectsDir + "/info/commit-graph"),
		chain:  newStatPath(objectsDir + "/info/commit-graphs/commit-graph-chain"),
	}
}

func statGraphFile(p *statPath) graphFileStamp {
	st, err := p.stat(false)
	if err != nil || !st.exists {
		return graphFileStamp{}
	}
	return graphFileStamp{exists: true, size: st.size, mtime: st.mtime, ino: st.ino}
}

// stamp returns the stamp of the commit-graph files. Comparing two stamps
// tells whether openCommitGraph may give another result.
func (g *graphPaths) stamp() graphStamp {
	return graphStamp{single: statGraphFile(&g.single), chain: statGraphFile(&g.chain)}
}

// statGraph returns the stamp of the commit-graph files of objectsDir.
func statGraph(objectsDir string) graphStamp {
	p := newGraphPaths(objectsDir)
	return p.stamp()
}
