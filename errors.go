// SPDX-License-Identifier: MIT

package git

import (
	"errors"
	"fmt"
)

// Errors returned by the package. They are wrapped with context, so test them
// with errors.Is.
var (
	// ErrNotFound reports a missing object, ref or path, or a directory that
	// is not a Git repository.
	ErrNotFound = errors.New("git: not found")
	// ErrUnsupported reports a repository feature the package does not read:
	// the error is wrapped with the reason.
	ErrUnsupported = errors.New("git: unsupported repository")
	// ErrAmbiguous reports an abbreviated object id that matches several
	// objects.
	ErrAmbiguous = errors.New("git: ambiguous object id")
	// ErrCorrupt reports malformed repository data. Reading never panics on
	// such data.
	ErrCorrupt = errors.New("git: corrupt data")
	// ErrInvalidID reports a malformed object id or abbreviation.
	ErrInvalidID = errors.New("git: invalid object id")
	// ErrWrongType reports an object that does not have the expected type.
	ErrWrongType = errors.New("git: wrong object type")
	// ErrTooLarge reports an object bigger than the configured bound
	// (see WithMaxObjectSize).
	ErrTooLarge = errors.New("git: object too large")
	// ErrClosed reports the use of a closed Repo.
	ErrClosed = errors.New("git: repository closed")
)

func corrupt(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, a...))
}

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, a...))
}

func notFound(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrNotFound, fmt.Sprintf(format, a...))
}
