// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"fmt"
)

// Entry is an entry of a tree.
type Entry struct {
	// Name is the file name. It points into the tree buffer and is only
	// valid during the callback that receives the entry; copy it to keep it.
	Name []byte
	Mode Mode
	ID   ObjectID
}

// ParseTree calls fn for each entry of a tree object in order, until fn
// returns false. The entries are not copied, so iterating allocates nothing.
// It returns ErrCorrupt on a malformed tree.
func ParseTree(data []byte, fn func(Entry) bool) error {
	var e Entry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp <= 0 || sp > 7 {
			return corrupt("tree: bad mode")
		}
		var mode uint32
		for _, c := range data[:sp] {
			if c < '0' || c > '7' {
				return corrupt("tree: bad mode")
			}
			mode = mode<<3 | uint32(c-'0')
		}
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul <= 0 || len(data)-nul-1 < 20 {
			return corrupt("tree: truncated entry")
		}
		e.Name = data[:nul]
		e.Mode = Mode(mode)
		copy(e.ID[:], data[nul+1:nul+21])
		data = data[nul+21:]
		if !fn(e) {
			return nil
		}
	}
	return nil
}

// ReadTree reads the tree id and calls fn for each entry, in tree order,
// until fn returns false. Entry.Name is only valid during the call. The tree
// is read into a pooled buffer, so listing allocates nothing per entry.
func (r *Repo) ReadTree(id ObjectID, fn func(Entry) bool) error {
	b, err := r.readTyped(id, TypeTree)
	if err != nil {
		return err
	}
	defer putBuf(b)
	if err := ParseTree(b.b, fn); err != nil {
		return wrapObj(id, err)
	}
	return nil
}

// LookupPath finds the entry at path (slash separated, no leading slash)
// below the tree root. It returns ErrNotFound when a component does not
// exist or is not a directory. An empty path designates root itself, as a
// directory.
func (r *Repo) LookupPath(root ObjectID, path string) (Entry, error) {
	cur := Entry{Mode: ModeTree, ID: root}
	for path != "" {
		var name string
		name, path, _ = cutByte(path, '/')
		if name == "" {
			return Entry{}, notFound("empty path component")
		}
		if !cur.Mode.IsTree() {
			return Entry{}, notFound("%q is not a directory", name)
		}
		found := false
		var next Entry
		err := r.ReadTree(cur.ID, func(e Entry) bool {
			if string(e.Name) == name {
				next = Entry{Mode: e.Mode, ID: e.ID}
				found = true
				return false
			}
			return true
		})
		if err != nil {
			return Entry{}, err
		}
		if !found {
			return Entry{}, notFound("path component %q", name)
		}
		cur = next
	}
	return cur, nil
}

func cutByte(s string, c byte) (before, after string, found bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func wrongType(id ObjectID, got, want Type) error {
	return fmt.Errorf("%w: %s is a %s, not a %s", ErrWrongType, id.String(), got, want)
}

func wrapObj(id ObjectID, err error) error {
	return fmt.Errorf("object %s: %w", id.String(), err)
}
