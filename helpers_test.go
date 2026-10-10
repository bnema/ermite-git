// SPDX-License-Identifier: MIT

package git

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fx builds a fixture repository with the git binary, which is also the
// oracle of the tests.
type fx struct {
	t    *testing.T
	dir  string // the git directory (bare) or work/.git
	work string // work tree, "" for a bare repository
	n    int    // commit counter, drives the dates
}

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Ada Lovelace",
		"GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Grace Hopper",
		"GIT_COMMITTER_EMAIL=grace@example.com",
		"LC_ALL=C",
	)
}

func requireGit(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

// runGit runs git in dir and returns its stdout.
func runGit(t testing.TB, dir string, stdin []byte, extraEnv []string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "gc.auto=0", "-c", "init.defaultBranch=main", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(), extraEnv...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return out.Bytes()
}

// newFx creates an empty non-bare repository; Open is given work, so the
// ".git" directory handling is covered.
func newFx(t *testing.T) *fx {
	requireGit(t)
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fx{t: t, work: work, dir: filepath.Join(work, ".git")}
	f.git("init", "-q")
	return f
}

// newBareFx creates an empty bare repository.
func newBareFx(t *testing.T) *fx {
	requireGit(t)
	dir := filepath.Join(t.TempDir(), "bare.git")
	f := &fx{t: t, dir: dir}
	runGit(t, t.TempDir(), nil, nil, "init", "-q", "--bare", dir)
	return f
}

func (f *fx) root() string {
	if f.work != "" {
		return f.work
	}
	return f.dir
}

func (f *fx) git(args ...string) string {
	f.t.Helper()
	return strings.TrimRight(string(runGit(f.t, f.root(), nil, f.env(), args...)), "\n")
}

func (f *fx) gitIn(stdin []byte, args ...string) string {
	f.t.Helper()
	return strings.TrimRight(string(runGit(f.t, f.root(), stdin, f.env(), args...)), "\n")
}

func (f *fx) env() []string {
	if f.work != "" {
		return nil
	}
	return []string{"GIT_DIR=" + f.dir}
}

func (f *fx) tick() []string {
	f.n++
	d := fmt.Sprintf("%d +0%d00", 1700000000+f.n*3600, f.n%10)
	if f.n%3 == 0 {
		d = fmt.Sprintf("%d -0%d30", 1700000000+f.n*3600, f.n%9)
	}
	return []string{"GIT_AUTHOR_DATE=" + d, "GIT_COMMITTER_DATE=" + d}
}

func (f *fx) write(path string, data []byte, mode os.FileMode) {
	f.t.Helper()
	full := filepath.Join(f.work, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, data, mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		f.t.Fatal(err)
	}
}

// commit stages everything and commits with the next date.
func (f *fx) commit(msg string) string {
	f.t.Helper()
	f.git("add", "-A")
	runGit(f.t, f.work, nil, f.tick(), "commit", "-q", "--allow-empty", "--allow-empty-message", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

func (f *fx) open(opts ...Option) *Repo {
	f.t.Helper()
	path := f.dir
	if f.work != "" {
		path = f.work
	}
	r, err := Open(path, opts...)
	if err != nil {
		f.t.Fatalf("Open: %v", err)
	}
	f.t.Cleanup(func() { _ = r.Close() })
	return r
}

// lines returns the non-empty lines of s.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func mustID(t testing.TB, s string) ObjectID {
	t.Helper()
	id, err := ParseID(strings.TrimSpace(s))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// textLines builds a compressible, delta-friendly text of n lines.
func textLines(n, seed int) []byte {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "line %d of the file, seed %d, some filler text to make it compress\n", i, (i*7+seed)%97)
	}
	return b.Bytes()
}

// ambiguousPair finds the contents of two blobs whose ids share their first
// 4 hex digits.
func ambiguousPair(t *testing.T) (a, b []byte) {
	t.Helper()
	seen := map[string]int{}
	for i := 0; i < 200000; i++ {
		data := "ambiguous " + strconv.Itoa(i) + "\n"
		h := sha1.New()
		_, _ = fmt.Fprintf(h, "blob %d\x00%s", len(data), data)
		p := fmt.Sprintf("%x", h.Sum(nil)[:2])
		if j, ok := seen[p]; ok {
			return []byte("ambiguous " + strconv.Itoa(j) + "\n"), []byte(data)
		}
		seen[p] = i
	}
	t.Fatal("no ambiguous pair found")
	return nil, nil
}

// writeBlob stores data as a loose blob and returns its id.
func (f *fx) writeBlob(data []byte) ObjectID {
	f.t.Helper()
	return mustID(f.t, f.gitIn(data, "hash-object", "-w", "--stdin"))
}
