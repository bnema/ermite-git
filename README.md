# ermite-git

A fast, read-only Git repository reader for Go: refs, objects, packfiles, trees and history, using commit-graph and changed-path Bloom filters. Standard library only.

Status: early development, no stable API.

```go
import git "bnema.dev/ermite-git"
```

## Test

```sh
go test ./...
```

## License

MIT. See [LICENSE](LICENSE).
