package monitor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// History appends complete JSON records with one retained rotation. Use a
// private directory and one writer. No per-sample fsync is performed.
type History struct {
	Path     string
	MaxBytes int64
}

func (h History) Append(value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if h.MaxBytes <= 0 || int64(len(b)) > h.MaxBytes {
		return errors.New("record exceeds the history file limit")
	}
	if err := os.MkdirAll(filepath.Dir(h.Path), 0700); err != nil {
		return err
	}
	// Refuse symlink outputs; the deployment owner controls the parent directory.
	info, err := os.Lstat(h.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && !info.Mode().IsRegular() {
		return errors.New("history output must be a regular file")
	}
	f, err := os.OpenFile(h.Path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	// Only a newline commits a record. Recover a crash or failed-write tail
	// before checking rotation, so an incomplete record is never retained.
	size, err := recoverTail(f)
	if err != nil {
		return errors.Join(err, f.Close())
	}
	if size+int64(len(b)) > h.MaxBytes {
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Rename(h.Path, h.Path+".1"); err != nil {
			return fmt.Errorf("rotate history: %w", err)
		}
		f, err = os.OpenFile(h.Path, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		size = 0
	}
	_, writeErr := io.Copy(f, bytes.NewReader(b))
	if writeErr != nil {
		writeErr = errors.Join(writeErr, f.Truncate(size))
	}
	return errors.Join(writeErr, f.Close())
}

func recoverTail(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	buf := make([]byte, 4096)
	for end := size; end > 0; {
		start := max(int64(0), end-int64(len(buf)))
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			end = start + int64(i) + 1
			if end != size {
				return end, f.Truncate(end)
			}
			return end, nil
		}
		end = start
	}
	return 0, f.Truncate(0)
}
