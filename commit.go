// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"time"
)

// Signature is the author or committer line of a commit or the tagger of a
// tag. Name and Email point into the buffer of the object they come from.
type Signature struct {
	Name   []byte
	Email  []byte
	Time   int64 // seconds since the Unix epoch
	Offset int   // time zone, in seconds east of UTC
}

// Date returns the time of the signature, in its own time zone. It allocates.
func (s Signature) Date() time.Time {
	t := time.Unix(s.Time, 0)
	if s.Offset == 0 {
		return t.UTC()
	}
	return t.In(time.FixedZone("", s.Offset))
}

// Commit is a parsed commit. A Commit is meant to be reused: ReadCommit and
// ParseCommit overwrite it and reuse its storage, so a hot loop allocates
// nothing after the first few commits. Every []byte field is valid until the
// next call that fills this Commit.
type Commit struct {
	ID        ObjectID // set by ReadCommit
	Tree      ObjectID
	Parents   []ObjectID
	Author    Signature
	Committer Signature
	// Encoding is the value of the encoding header, nil when absent. The
	// other headers (gpgsig, mergetag...) are skipped.
	Encoding []byte
	// Message is everything after the blank line that ends the headers,
	// as raw bytes: it may not be valid UTF-8.
	Message []byte

	buf []byte
}

// ReadCommit reads and parses the commit id into c.
func (r *Repo) ReadCommit(id ObjectID, c *Commit) error {
	typ, data, err := r.ReadObject(id, c.buf)
	if err != nil {
		return err
	}
	c.buf = data
	if typ != TypeCommit {
		return wrongType(id, typ, TypeCommit)
	}
	if err := ParseCommit(data, c); err != nil {
		return wrapObj(id, err)
	}
	c.ID = id
	return nil
}

// ParseCommit parses the content of a commit object into c. The fields of c
// point into data, which must stay unchanged while they are used.
//
// It reads headers the way Git does: the first line must be the tree, the
// parent lines are the ones that immediately follow it, and any tree or
// parent line further down is an ordinary, ignored header. It returns
// ErrCorrupt if the first line is not a valid tree or a parent is malformed.
func ParseCommit(data []byte, c *Commit) error {
	c.ID = ObjectID{}
	c.Parents = c.Parents[:0]
	c.Author, c.Committer = Signature{}, Signature{}
	c.Encoding, c.Message = nil, nil
	const (
		stTree = iota
		stParents
		stRest
	)
	state := stTree
	for len(data) > 0 {
		if data[0] == '\n' && state != stTree {
			c.Message = data[1:]
			break
		}
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		key, val, _ := bytes.Cut(line, []byte{' '})
		switch state {
		case stTree:
			id, ok := parseIDBytes(val)
			if string(key) != "tree" || !ok {
				return corrupt("commit: the first header is not a valid tree")
			}
			c.Tree = id
			state = stParents
			continue
		case stParents:
			if string(key) == "parent" {
				id, ok := parseIDBytes(val)
				if !ok {
					return corrupt("commit: bad parent header")
				}
				c.Parents = append(c.Parents, id)
				continue
			}
			state = stRest
		}
		if line[0] == ' ' {
			continue // continuation of a multi-line header such as gpgsig
		}
		switch string(key) {
		case "author":
			c.Author = parseSignature(val)
		case "committer":
			c.Committer = parseSignature(val)
		case "encoding":
			c.Encoding = val
		}
	}
	if state == stTree {
		return corrupt("commit: no tree")
	}
	return nil
}

// parseSignature parses "Name <email> <seconds> <+hhmm>". It is as lenient
// as Git: what cannot be parsed is left zero.
func parseSignature(b []byte) Signature {
	var s Signature
	lt := bytes.IndexByte(b, '<')
	gt := bytes.LastIndexByte(b, '>')
	if lt < 0 || gt < lt {
		s.Name = bytes.TrimSpace(b)
		return s
	}
	s.Name = bytes.TrimRight(b[:lt], " \t\r\n")
	s.Email = b[lt+1 : gt]
	rest := bytes.TrimLeft(b[gt+1:], " ")
	secs, rest, _ := bytes.Cut(rest, []byte{' '})
	if t, ok := parseInt(secs); ok {
		s.Time = t
		if tz := bytes.TrimSpace(rest); len(tz) == 5 && (tz[0] == '+' || tz[0] == '-') {
			hh, ok1 := parseInt(tz[1:3])
			mm, ok2 := parseInt(tz[3:5])
			if ok1 && ok2 {
				off := int(hh*3600 + mm*60)
				if tz[0] == '-' {
					off = -off
				}
				s.Offset = off
			}
		}
	}
	return s
}

// parseInt parses a decimal integer of up to 18 digits.
func parseInt(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 18 {
		return 0, false
	}
	var v int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int64(c-'0')
	}
	return v, true
}

// Tag is a parsed annotated tag. Name, Tagger and Message point into the
// data that was parsed.
type Tag struct {
	Object  ObjectID
	Type    Type // the type of Object
	Name    []byte
	Tagger  Signature
	Message []byte
}

// ReadTag reads and parses the annotated tag id into t. The byte fields of t
// are copies, owned by the caller.
func (r *Repo) ReadTag(id ObjectID, t *Tag) error {
	b, err := r.readTyped(id, TypeTag)
	if err != nil {
		return err
	}
	defer putBuf(b)
	if err := ParseTag(b.b, t); err != nil {
		return wrapObj(id, err)
	}
	// The pooled buffer goes back: detach the fields.
	t.Name = bytes.Clone(t.Name)
	t.Message = bytes.Clone(t.Message)
	t.Tagger.Name = bytes.Clone(t.Tagger.Name)
	t.Tagger.Email = bytes.Clone(t.Tagger.Email)
	return nil
}

// ParseTag parses the content of a tag object into t, whose byte fields point
// into data. It returns ErrCorrupt if the object or its type is missing.
func ParseTag(data []byte, t *Tag) error {
	*t = Tag{}
	haveObj, haveType := false, false
	for len(data) > 0 {
		if data[0] == '\n' {
			t.Message = data[1:]
			break
		}
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		key, val, _ := bytes.Cut(line, []byte{' '})
		switch string(key) {
		case "object":
			id, ok := parseIDBytes(val)
			if !ok {
				return corrupt("tag: bad object header")
			}
			t.Object, haveObj = id, true
		case "type":
			typ, ok := parseTypeName(val)
			if !ok {
				return corrupt("tag: bad type header")
			}
			t.Type, haveType = typ, true
		case "tag":
			t.Name = val
		case "tagger":
			t.Tagger = parseSignature(val)
		}
	}
	if !haveObj || !haveType {
		return corrupt("tag: missing object or type")
	}
	return nil
}
