# Limits

## A pack truncated in place can crash the process

Packs and indexes are memory-mapped. If another process shrinks a mapped file, reading the missing part raises SIGBUS, which Go cannot recover from.

Git never does this: it replaces packs by renaming a new file over them. It takes an external tool that edits packs in place, or a failing disk. Not handled.

## Without mmap, packs are read into memory

Linux, macOS and the BSDs map packs. On Windows and other systems, the whole `.pack` and `.idx` are read into memory when opened, so memory grows with the repository. Not handled.

## Revision names

`Resolve` follows `git rev-parse` for plain names:

- a tag wins over a branch of the same name;
- a full 40-digit id is never read as a ref.

Use `ResolveRef` for a full ref name, and `ResolveCommitPrefix` for an abbreviation that must be a commit.
