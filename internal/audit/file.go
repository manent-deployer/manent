package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// FileOptions configures the audit file.
type FileOptions struct {
	// MaxSize is the size in bytes beyond which the file is rotated (0 = no rotation).
	MaxSize int64
	// Key, if set, makes the chain an HMAC: without the key it cannot be recomputed.
	Key []byte
}

// FileRecorder appends every entry as a JSON line to a file opened append-only. Entries form
// a chain: each one carries the hash of the previous one (prev_hash) and its own (hash),
// computed on the line without the hash field. A changed, inserted or deleted line breaks the
// chain and is detected by Verify. The chain continues across rotated files.
type FileRecorder struct {
	mu   sync.Mutex
	path string
	opts FileOptions
	file *os.File
	size int64
	prev string
}

// NewFileRecorder opens (or creates with 0600 permissions) the audit file and resumes the
// chain from its last entry.
func NewFileRecorder(path string, opts FileOptions) (*FileRecorder, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("audit file: %w", err)
	}
	prev, err := lastHash(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("audit file %s: %w", path, err)
	}
	return &FileRecorder{path: path, opts: opts, file: f, size: st.Size(), prev: prev}, nil
}

// lastHash reads the hash of the last entry of the file ("" if the file is empty or has no chain).
func lastHash(f *os.File, size int64) (string, error) {
	const tail = 64 << 10
	off := max(size-tail, 0)
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte("\n"))
	last := lines[len(lines)-1]
	if len(last) == 0 {
		return "", nil
	}
	_, hash, ok := splitHash(last)
	if !ok {
		return "", nil
	}
	return hash, nil
}

// Record completes the entry with the chain, writes it and syncs it to disk.
func (r *FileRecorder) Record(_ context.Context, e Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.PrevHash = r.prev
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	hash := chainHash(r.opts.Key, body)
	line := append(body[:len(body)-1], fmt.Sprintf(`,"hash":%q}`+"\n", hash)...)
	if r.opts.MaxSize > 0 && r.size > 0 && r.size+int64(len(line)) > r.opts.MaxSize {
		if err := r.rotate(); err != nil {
			return err
		}
	}
	n, err := r.file.Write(line)
	r.size += int64(n)
	if err != nil {
		return err
	}
	if err := r.file.Sync(); err != nil {
		return err
	}
	r.prev = hash
	return nil
}

// rotateSuffix is the suffix format of rotated files: alphabetical order is chronological.
const rotateSuffix = "20060102T150405.000000000Z"

// rotate renames the current file with the time suffix and opens a new one.
func (r *FileRecorder) rotate() error {
	if err := r.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(r.path, r.path+"."+time.Now().UTC().Format(rotateSuffix)); err != nil {
		return fmt.Errorf("audit file rotation: %w", err)
	}
	f, err := os.OpenFile(r.path, os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("audit file rotation: %w", err)
	}
	r.file, r.size = f, 0
	return nil
}

// Close closes the file.
func (r *FileRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}

// chainHash is the sha256 (or HMAC-SHA256 with the key) of the line without the hash field.
func chainHash(key, body []byte) string {
	if len(key) > 0 {
		m := hmac.New(sha256.New, key)
		m.Write(body)
		return hex.EncodeToString(m.Sum(nil))
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// hashSuffix is the hash field every line ends with.
var hashSuffix = regexp.MustCompile(`,"hash":"([0-9a-f]{64})"\}$`)

// splitHash splits a line into the body the hash is computed on and the hash.
func splitHash(line []byte) (body []byte, hash string, ok bool) {
	m := hashSuffix.FindSubmatchIndex(line)
	if m == nil {
		return nil, "", false
	}
	body = append(slices.Clone(line[:m[0]]), '}')
	return body, string(line[m[2]:m[3]]), true
}

// Report is the result of Verify.
type Report struct {
	Files   []string `json:"files"`
	Entries int      `json:"entries"`
	// Anchor is the prev_hash of the first verified entry: empty if the chain starts from the
	// beginning, otherwise it must match the last entry of the previous (e.g. archived) file.
	Anchor string `json:"anchor,omitempty"`
	// Last is the hash of the last entry, to compare with the copy in the SIEM or an archive.
	Last string `json:"last,omitempty"`
}

// Files returns the audit file preceded by its rotated files, in chronological order.
func Files(path string) ([]string, error) {
	rotated, err := filepath.Glob(path + ".*")
	if err != nil {
		return nil, err
	}
	rotated = slices.DeleteFunc(rotated, func(p string) bool {
		_, err := time.Parse(rotateSuffix, strings.TrimPrefix(p, path+"."))
		return err != nil
	})
	slices.Sort(rotated)
	return append(rotated, path), nil
}

// Verify recomputes the chain of the given files, in chronological order, and returns the
// first point where it is broken.
func Verify(paths []string, key []byte) (Report, error) {
	rep := Report{Files: paths}
	prev, first := "", true
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return rep, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for n := 1; sc.Scan(); n++ {
			body, hash, ok := splitHash(sc.Bytes())
			if !ok {
				_ = f.Close()
				return rep, fmt.Errorf("%s:%d: entry without hash", p, n)
			}
			var e Entry
			if err := json.Unmarshal(body, &e); err != nil {
				_ = f.Close()
				return rep, fmt.Errorf("%s:%d: invalid entry: %w", p, n, err)
			}
			if first {
				rep.Anchor, prev, first = e.PrevHash, e.PrevHash, false
			}
			if e.PrevHash != prev {
				_ = f.Close()
				return rep, fmt.Errorf("%s:%d: broken chain: prev_hash does not match the previous entry (entry missing or inserted)", p, n)
			}
			if chainHash(key, body) != hash {
				_ = f.Close()
				return rep, fmt.Errorf("%s:%d: invalid hash: entry modified (or different key)", p, n)
			}
			prev = hash
			rep.Entries++
		}
		err = sc.Err()
		_ = f.Close()
		if err != nil {
			return rep, fmt.Errorf("%s: %w", p, err)
		}
	}
	rep.Last = prev
	return rep, nil
}
