# ermite-git

A fast, read-only Git repository reader for Go: refs, objects, packfiles, trees and history, using commit-graph and changed-path Bloom filters. Standard library only.

Status: early development, no stable API. Phase A (this tree) reads refs, loose and packed objects, trees, blobs and commits; history walks come next.

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

A `Repo` is safe for concurrent use. Call `Refresh` after the repository changed (a push, a repack) to see new refs and packs; reads in flight keep working while packs are replaced.

Repositories the package does not read are rejected by `Open` with `ErrUnsupported`: SHA-256, reftable, other `extensions.*`, shallow, grafts, replace refs, alternates, partial clones, incremental or stale multi-pack-index.

## Test

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
