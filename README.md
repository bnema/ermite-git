<h1 align="center">ermite-git</h1>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue?style=flat-square" alt="License: MIT"></a>
  <a href="https://github.com/bnema/ermite-git/commits/main"><img src="https://badgen.net/github/last-commit/bnema/ermite-git/main?icon=github" alt="Last commit"></a>
  <a href="https://github.com/bnema/ermite-git/stargazers"><img src="https://badgen.net/github/stars/bnema/ermite-git?icon=github" alt="GitHub stars"></a>
</p>

<p align="center">A fast, read-only Git repository reader for Go. Standard library only.</p>

> Read-only mirror. Development happens at git.bnema.dev; issues and pull requests are closed here.

---

## Features

- Refs, loose and packed objects, trees, blobs and commits
- History in `git log` order, with `git log -- path` simplification
- Commit-graph and changed-path Bloom filters for fast history
- Safe for concurrent use; no allocation per commit after warm-up

Status: early development, no stable API.

## Usage

```go
import git "bnema.dev/ermite-git"

repo, err := git.Open("/srv/repos/project.git") // bare, or a ".git" directory
if err != nil {
	return err // errors.Is(err, git.ErrUnsupported) → use the git binary
}
defer repo.Close()

id, _ := repo.Resolve("main")
commitID, _ := repo.PeelToCommit(id)

var c git.Commit // reuse it
_ = repo.ReadCommit(commitID, &c)
_ = repo.ReadTree(c.Tree, func(e git.Entry) bool {
	fmt.Printf("%s %s\n", e.Mode, e.Name) // e.Name is valid during the callback only
	return true
})

for c, err := range repo.Walk(ctx, commitID, git.WalkOptions{Path: "pkg/gui", Limit: 50}) {
	if err != nil {
		return err
	}
	fmt.Println(c.ID) // c is reused: valid until the next iteration
}

// last commit of each entry of a directory, in one walk
ids, err := repo.LastCommits(ctx, commitID, "pkg", []string{"gui", "commands"})
```

Call `Refresh` after the repository changes (push, repack) to see new refs and packs.

`Open` returns `ErrUnsupported` for SHA-256, reftable, other `extensions.*`, shallow, grafts, replace refs, alternates, partial clones, and incremental or stale multi-pack-index.

Known limits: [docs/limits.md](docs/limits.md).

## Test

Tests compare with the `git` binary, 2.45 or newer (skipped without it).

```sh
go test -race ./...
ERMITE_GIT_BENCH_REPO=/path/to/repo/.git go test -run TestRealRepository -v   # every object vs git cat-file
go test -run '^$' -bench . -benchmem
go test -run '^$' -fuzz FuzzParseCommit -fuzztime 30s
```

## License

MIT. See [LICENSE](LICENSE).
