// SPDX-License-Identifier: MIT

package git

import "strconv"

// Type is the type of a Git object. The values are those of pack entries.
type Type uint8

// Object types.
const (
	TypeCommit Type = 1
	TypeTree   Type = 2
	TypeBlob   Type = 3
	TypeTag    Type = 4
)

// String returns the name Git uses for the type ("commit", "tree", "blob",
// "tag").
func (t Type) String() string {
	switch t {
	case TypeCommit:
		return "commit"
	case TypeTree:
		return "tree"
	case TypeBlob:
		return "blob"
	case TypeTag:
		return "tag"
	}
	return "type(" + strconv.Itoa(int(t)) + ")"
}

func parseTypeName(b []byte) (Type, bool) {
	switch string(b) {
	case "commit":
		return TypeCommit, true
	case "tree":
		return TypeTree, true
	case "blob":
		return TypeBlob, true
	case "tag":
		return TypeTag, true
	}
	return 0, false
}

// Mode is the file mode of a tree entry.
type Mode uint32

// Tree entry modes.
const (
	ModeTree    Mode = 0o040000
	ModeFile    Mode = 0o100644
	ModeExec    Mode = 0o100755
	ModeSymlink Mode = 0o120000
	ModeGitlink Mode = 0o160000 // a submodule
)

const modeTypeMask Mode = 0o170000

// IsTree reports whether the entry is a directory.
func (m Mode) IsTree() bool { return m&modeTypeMask == ModeTree }

// IsGitlink reports whether the entry is a submodule commit.
func (m Mode) IsGitlink() bool { return m&modeTypeMask == ModeGitlink }

// IsSymlink reports whether the entry is a symbolic link.
func (m Mode) IsSymlink() bool { return m&modeTypeMask == ModeSymlink }

// IsRegular reports whether the entry is a regular file, executable or not.
func (m Mode) IsRegular() bool { return m&modeTypeMask == 0o100000 }

// Type returns the type of the object the entry designates: a tree for a
// directory, a commit for a submodule, a blob otherwise.
func (m Mode) Type() Type {
	switch m & modeTypeMask {
	case ModeTree:
		return TypeTree
	case ModeGitlink:
		return TypeCommit
	}
	return TypeBlob
}

// String returns the mode as Git prints it: six octal digits (five for a
// directory is printed as "040000").
func (m Mode) String() string {
	s := strconv.FormatUint(uint64(m), 8)
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}
