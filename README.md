# ermite-git

> Read-only mirror. Development happens at git.bnema.dev; issues and pull requests are closed here.

A fast, read-only Git repository reader for Go: refs, objects, packfiles, trees and history, using commit-graph and changed-path Bloom filters. Standard library only.

Status: early development, no stable API. Phase A reads refs, loose and packed objects, trees, blobs and commits; phase B adds the commit-graph, changed-path Bloom filters and history walks.

```go
import git "bnema.dev/ermite-git"

repo, err := git.Open("/srv/repos/project.git") // bare, or a ".git" directory
if err != nil {
	return err // errors.Is(err, git.ErrUnsupported) → use the git binary
}
defer repo.Close()

id, _ := repo.Resolve("main")
commitID, _ := repo.PeelToCommit(id)

var c git.Commit // reuse it: no allocation per commit after warm-up
_ = repo.ReadCommit(commitID, &c)
_ = repo.ReadTree(c.Tree, func(e git.Entry) bool {
	fmt.Printf("%s %s\n", e.Mode, e.Name) // e.Name is valid during the callback only
	return true
})
```

History in the order of `git log`, with the same path simplification as `git log -- path`:

```go
for c, err := range repo.Walk(ctx, commitID, git.WalkOptions{Path: "pkg/gui", Skip: 100, Limit: 50}) {
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", c.ID) // c is reused: valid until the next iteration
}
// the last commit of every entry of a directory, in one walk
ids, err := repo.LastCommits(ctx, commitID, "pkg", []string{"gui", "commands"})
```

A `Repo` is safe for concurrent use. Call `Refresh` after the repository changed (a push, a repack) to see new refs and packs; reads in flight keep working while packs are replaced.

Repositories the package does not read are rejected by `Open` with `ErrUnsupported`: SHA-256, reftable, other `extensions.*`, shallow, grafts, replace refs, alternates, partial clones, incremental or stale multi-pack-index.

## Limits

- **A pack truncated in place can crash the process.** Packs and indexes are memory-mapped; if another process shrinks one of them while it is mapped, a read of the missing part ends with SIGBUS, which Go cannot recover from. Git always replaces a pack by renaming a new file over it (repack, gc, fetch), and a mapping keeps the old file alive, so this takes an external actor that edits packs in place, or a failing disk. Not handled.
- **Without mmap, packs are read into memory.** Linux, macOS, FreeBSD, OpenBSD, NetBSD and DragonFly map packs. On Windows and other systems the whole `.pack` and `.idx` are read into memory when a pack is opened, so memory grows with the size of the repository. Not handled.
- **Revision names.** `Resolve` follows `git rev-parse` for plain names: a tag wins over a branch of the same name, and a full 40 digit id is never read as a ref. Use `ResolveRef` for a full ref name and `ResolveCommitPrefix` for an abbreviation that must designate a commit.

## Test

The conformance tests compare with the `git` binary, 2.45 or newer (they are skipped without git).

```sh
go test -race ./...
# also compare every object of a real repository with `git cat-file`:
ERMITE_GIT_BENCH_REPO=/path/to/repo/.git go test -run TestRealRepository -v
# benchmarks (the same variable runs them on that repository):
go test -run '^$' -bench . -benchmem
# fuzz targets:
go test -run '^$' -fuzz FuzzParseCommit -fuzztime 30s
```

The tests build their fixtures with the `git` binary (2.45 or later) and compare with it.

## License

MIT. See [LICENSE](LICENSE).
