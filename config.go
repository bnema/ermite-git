// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// maxConfigSize bounds the config file that is read.
const maxConfigSize = 1 << 20

// checkConfig reads the few settings that decide whether the repository is
// readable: core.repositoryformatversion and the extensions.* keys. Other
// settings, includes and conditional includes are ignored.
func checkConfig(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) > maxConfigSize {
		return unsupported("config file larger than %d bytes", maxConfigSize)
	}
	return checkConfigData(data)
}

func checkConfigData(data []byte) error {
	section := ""
	version := int64(0)
	for len(data) > 0 {
		var line []byte
		line, data, _ = bytes.Cut(data, []byte{'\n'})
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := bytes.IndexByte(line, ']')
			if end < 0 {
				return corrupt("config: bad section line %q", truncate(string(line)))
			}
			head := string(bytes.TrimSpace(line[1:end]))
			// "[core]", "[remote \"origin\"]", "[branch.main]"
			name, _, _ := strings.Cut(head, " ")
			name, _, _ = strings.Cut(name, ".")
			section = strings.ToLower(name)
			line = bytes.TrimSpace(line[end+1:])
			if len(line) == 0 || line[0] == '#' || line[0] == ';' {
				continue
			}
			// "[core] bare = true" on one line.
		}
		keyb, valb, _ := bytes.Cut(line, []byte{'='})
		key := strings.ToLower(string(bytes.TrimSpace(keyb)))
		val := configValue(valb)
		switch section {
		case "core":
			if key == "repositoryformatversion" {
				v, err := strconv.ParseInt(val, 10, 64)
				if err != nil {
					return corrupt("config: core.repositoryformatversion %q", truncate(val))
				}
				version = v
			}
		case "extensions":
			switch key {
			case "objectformat":
				if !strings.EqualFold(val, "sha1") {
					return unsupported("extensions.objectformat = %s", truncate(val))
				}
			case "refstorage":
				if !strings.EqualFold(val, "files") {
					return unsupported("extensions.refstorage = %s", truncate(val))
				}
			case "noop", "worktreeconfig", "preciousobjects", "relativeworktrees":
				// No effect on reading: preciousobjects only forbids
				// deleting objects, relativeworktrees changes how linked
				// worktrees point at the repository (they are rejected
				// anyway).
			default:
				return unsupported("extensions.%s", truncate(key))
			}
		}
	}
	if version != 0 && version != 1 {
		return unsupported("core.repositoryformatversion = %d", version)
	}
	return nil
}

// configValue returns the unquoted value of a config line, without a trailing
// comment.
func configValue(b []byte) string {
	b = bytes.TrimSpace(b)
	var sb strings.Builder
	inQuote := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == '\\' && i+1 < len(b):
			i++
			sb.WriteByte(b[i])
		case c == '"':
			inQuote = !inQuote
		case !inQuote && (c == '#' || c == ';'):
			return strings.TrimSpace(sb.String())
		default:
			sb.WriteByte(c)
		}
	}
	return strings.TrimSpace(sb.String())
}
