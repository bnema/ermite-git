// SPDX-License-Identifier: MIT

// Package git reads Git repositories without the git binary: refs, loose
// and packed objects, trees, blobs and commit history. It uses the
// commit-graph and changed-path Bloom filters when present. It never
// writes to a repository.
package git
