package driver

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// readJSONLines returns the lines of a JSON-Lines file, without their newlines
// and without the blank ones. A file that is not there has none. A last line with
// no newline after it is a write that was cut short: it is left out, and torn says
// it was there.
func readJSONLines(path string) (lines [][]byte, torn bool, err error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is one of the run's own files in its output directory
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			return lines, true, nil
		}
		if line := bytes.TrimSpace(data[:end]); len(line) > 0 {
			lines = append(lines, line)
		}
		data = data[end+1:]
	}
	return lines, false, nil
}

// rewriteJSONLines replaces the file at path with lines, one to a line. The
// replacement is all there or not at all.
func rewriteJSONLines(path string, lines [][]byte) error {
	var buf bytes.Buffer
	for _, line := range lines {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return writeFileAtomic(path, buf.Bytes())
}

// writeFileAtomic replaces the file at path with data. The data is written beside
// the file, forced to disk and renamed over it, so a reader sees all of the old
// content or all of the new, whatever stops the writer.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- beside one of the run's own files
	if err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Base(tmp), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", filepath.Base(tmp), err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing %s: %w", filepath.Base(tmp), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", filepath.Base(tmp), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing %s: %w", filepath.Base(path), err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

// removeFile removes the file at path. One that is not there is as good as removed.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", filepath.Base(path), err)
	}
	return nil
}

// syncDir forces the entry of a file made or renamed in dir onto disk. It is
// best effort: not every platform can open a directory to sync it.
func syncDir(dir string) {
	d, err := os.Open(dir) // #nosec G304 -- the run's own output directory
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// syncFile forces the content of the file at path onto disk. A file that is not
// there has nothing to force.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0) // #nosec G304 -- one of the run's own files in its output directory
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("opening %s to sync it: %w", filepath.Base(path), err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", filepath.Base(path), err)
	}
	return nil
}
