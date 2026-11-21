package store

import (
	"os"

	"github.com/ncruces/go-sqlite3/vfs"
)

// No Unwrap or SharedMemory escape. Only the no-atime reader reads DB bytes;
// upstream's descriptor retains its tested OFD locking semantics. SQLite must
// see a logically writable DB to include CHECK constraints in integrity_check;
// this wrapper, not SQLite's readonly flag, enforces the inspection write ban.
type inspectionFile struct {
	vfs.File
	reader *os.File
}

func (f *inspectionFile) ReadAt(p []byte, off int64) (int, error) { return f.reader.ReadAt(p, off) }
func (f *inspectionFile) WriteAt([]byte, int64) (int, error)      { return 0, ErrUnsafeFile }
func (f *inspectionFile) Truncate(int64) error                    { return ErrUnsafeFile }
func (f *inspectionFile) Close() error {
	readerErr := f.reader.Close()
	fileErr := f.File.Close()
	if readerErr != nil || fileErr != nil {
		return ErrIO
	}
	return nil
}
