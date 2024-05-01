// Package fsnotify provides a cross-platform interface for file system
// notifications.
//
// Currently supported systems:
//
//   - Linux      via inotify
//   - BSD, macOS via kqueue
//   - Windows    via ReadDirectoryChangesW
//   - illumos    via FEN
//
// # FSNOTIFY_DEBUG
//
// Set the FSNOTIFY_DEBUG environment variable to "1" to print debug messages to
// stderr. This can be useful to track down some problems, especially in cases
// where fsnotify is used as an indirect dependency.
//
// Every event will be printed as soon as there's something useful to print,
// with as little processing from fsnotify.
//
// Example output:
//
//	FSNOTIFY_DEBUG: 11:34:23.633087586   256:IN_CREATE            → "/tmp/file-1"
//	FSNOTIFY_DEBUG: 11:34:23.633202319     4:IN_ATTRIB            → "/tmp/file-1"
//	FSNOTIFY_DEBUG: 11:34:28.989728764   512:IN_DELETE            → "/tmp/file-1"
package fsnotify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Event represents a file system notification.
type Event struct {
	// Path to the file or directory.
	//
	// Paths are relative to the input; for example with Add("dir") the Name
	// will be set to "dir/file" if you create that file, but if you use
	// Add("/path/to/dir") it will be "/path/to/dir/file".
	Name string

	// File operation that triggered the event.
	//
	// This is a bitmask and some systems may send multiple operations at once.
	// Use the Event.Has() method instead of comparing with ==.
	Op Op

	// Create events will have this set to the old path if it's a rename. This
	// only works when both the source and destination are watched. It's not
	// reliable when watching individual files, only directories.
	//
	// For example "mv /tmp/file /tmp/rename" will emit:
	//
	//   Event{Op: Rename, Name: "/tmp/file"}
	//   Event{Op: Create, Name: "/tmp/rename", RenamedFrom: "/tmp/file"}
	renamedFrom string
}

// Op describes a set of file operations.
type Op uint32

// The operations fsnotify can trigger; see the documentation on [Watcher] for a
// full description, and check them with [Event.Has].
const (
	// A new pathname was created.
	Create Op = 1 << iota

	// The pathname was written to; this does *not* mean the write has finished,
	// and a write can be followed by more writes.
	Write

	// The path was removed; any watches on it will be removed. Some "remove"
	// operations may trigger a Rename if the file is actually moved (for
	// example "remove to trash" is often a rename).
	Remove

	// The path was renamed to something else; any watches on it will be
	// removed.
	Rename

	// File attributes were changed.
	//
	// It's generally not recommended to take action on this event, as it may
	// get triggered very frequently by some software. For example, Spotlight
	// indexing on macOS, anti-virus software, backup software, etc.
	Chmod

	// File descriptor was opened.
	//
	// Only works on Linux and FreeBSD.
	xUnportableOpen

	// File was read from.
	//
	// Only works on Linux and FreeBSD.
	xUnportableRead

	// File opened for writing was closed.
	//
	// Only works on Linux and FreeBSD.
	//
	// The advantage of using this over Write is that it's more reliable than
	// waiting for Write events to stop. It's also faster (if you're not
	// listening to Write events): copying a file of a few GB can easily
	// generate tens of thousands of Write events in a short span of time.
	xUnportableCloseWrite

	// File opened for reading was closed.
	//
	// Only works on Linux and FreeBSD.
	xUnportableCloseRead
)

var (
	// ErrNonExistentWatch is used when Remove() is called on a path that's not
	// added.
	ErrNonExistentWatch = errors.New("fsnotify: can't remove non-existent watch")

	// ErrClosed is used when trying to operate on a closed Watcher.
	ErrClosed = errors.New("fsnotify: watcher already closed")

	// ErrEventOverflow is reported from the Errors channel when there are too
	// many events:
	//
	//  - inotify:      inotify returns IN_Q_OVERFLOW – because there are too
	//                  many queued events (the fs.inotify.max_queued_events
	//                  sysctl can be used to increase this).
	//  - windows:      The buffer size is too small; WithBufferSize() can be used to increase it.
	//  - kqueue, fen:  Not used.
	ErrEventOverflow = errors.New("fsnotify: queue or buffer overflow")

	// ErrUnsupported is returned by AddWith() when WithOps() specified an
	// Unportable event that's not supported on this platform.
	xErrUnsupported = errors.New("fsnotify: not supported with this backend")
)

func (o Op) String() string {
	var b strings.Builder
	if o.Has(Create) {
		b.WriteString("|CREATE")
	}
	if o.Has(Remove) {
		b.WriteString("|REMOVE")
	}
	if o.Has(Write) {
		b.WriteString("|WRITE")
	}
	if o.Has(xUnportableOpen) {
		b.WriteString("|OPEN")
	}
	if o.Has(xUnportableRead) {
		b.WriteString("|READ")
	}
	if o.Has(xUnportableCloseWrite) {
		b.WriteString("|CLOSE_WRITE")
	}
	if o.Has(xUnportableCloseRead) {
		b.WriteString("|CLOSE_READ")
	}
	if o.Has(Rename) {
		b.WriteString("|RENAME")
	}
	if o.Has(Chmod) {
		b.WriteString("|CHMOD")
	}
	if b.Len() == 0 {
		return "[no events]"
	}
	return b.String()[1:]
}

// Has reports if this operation has the given operation.
func (o Op) Has(h Op) bool { return o&h != 0 }

// Has reports if this event has the given operation.
func (e Event) Has(op Op) bool { return e.Op.Has(op) }

// String returns a string representation of the event with their path.
func (e Event) String() string {
	if e.renamedFrom != "" {
		return fmt.Sprintf("%-13s %q ← %q", e.Op.String(), e.Name, e.renamedFrom)
	}
	return fmt.Sprintf("%-13s %q", e.Op.String(), e.Name)
}

type (
	addOpt   func(opt *withOpts)
	withOpts struct {
		bufsize  int
		op       Op
		noFollow bool
	}
)

var debug = func() bool {
	// Check for exactly "1" (rather than mere existence) so we can add
	// options/flags in the future. I don't know if we ever want that, but it's
	// nice to leave the option open.
	return os.Getenv("FSNOTIFY_DEBUG") == "1"
}()

var defaultOpts = withOpts{
	bufsize: 65536, // 64K
	op:      Create | Write | Remove | Rename | Chmod,
}

func getOptions(opts ...addOpt) withOpts {
	with := defaultOpts
	for _, o := range opts {
		if o != nil {
			o(&with)
		}
	}
	return with
}

// WithBufferSize sets the [ReadDirectoryChangesW] buffer size.
//
// This only has effect on Windows systems, and is a no-op for other backends.
//
// The default value is 64K (65536 bytes) which is the highest value that works
// on all filesystems and should be enough for most applications, but if you
// have a large burst of events it may not be enough. You can increase it if
// you're hitting "queue or buffer overflow" errors ([ErrEventOverflow]).
//
// [ReadDirectoryChangesW]: https://learn.microsoft.com/en-gb/windows/win32/api/winbase/nf-winbase-readdirectorychangesw
func WithBufferSize(bytes int) addOpt {
	return func(opt *withOpts) { opt.bufsize = bytes }
}

// WithOps sets which operations to listen for. The default is [Create],
// [Write], [Remove], [Rename], and [Chmod].
//
// Excluding operations you're not interested in can save quite a bit of CPU
// time; in some use cases there may be hundreds of thousands of useless Write
// or Chmod operations per second.
//
// This can also be used to add unportable operations not supported by all
// platforms; unportable operations all start with "Unportable":
// [UnportableOpen], [UnportableRead], [UnportableCloseWrite], and
// [UnportableCloseRead].
//
// AddWith returns an error when using an unportable operation that's not
// supported. Use [Watcher.Support] to check for support.
func withOps(op Op) addOpt {
	return func(opt *withOpts) { opt.op = op }
}

// WithNoFollow disables following symlinks, so the symlinks themselves are
// watched.
func withNoFollow() addOpt {
	return func(opt *withOpts) { opt.noFollow = true }
}

var enableRecurse = false

// Check if this path is recursive (ends with "/..." or "\..."), and return the
// path with the /... stripped.
func recursivePath(path string) (string, bool) {
	if !enableRecurse { // Only enabled in tests for now.
		return path, false
	}
	if filepath.Base(path) == "..." {
		return filepath.Dir(path), true
	}
	return path, false
}
