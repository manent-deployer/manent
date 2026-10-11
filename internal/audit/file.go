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

// FileOptions configura il file di audit.
type FileOptions struct {
	// MaxSize è la dimensione in byte oltre cui il file viene ruotato (0 = nessuna rotazione).
	MaxSize int64
	// Key, se presente, rende la catena un HMAC: senza la chiave non può essere ricalcolata.
	Key []byte
}

// FileRecorder aggiunge ogni voce come riga JSON a un file aperto in sola aggiunta. Le voci
// formano una catena: ognuna riporta l'hash della precedente (prev_hash) e il proprio (hash),
// calcolato sulla riga senza il campo hash. Una riga modificata, inserita o cancellata rompe la
// catena e viene rilevata da Verify. La catena prosegue tra i file ruotati.
type FileRecorder struct {
	mu   sync.Mutex
	path string
	opts FileOptions
	file *os.File
	size int64
	prev string
}

// NewFileRecorder apre (o crea con permessi 0600) il file di audit e riprende la catena
// dall'ultima voce presente.
func NewFileRecorder(path string, opts FileOptions) (*FileRecorder, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("file di audit: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("file di audit: %w", err)
	}
	prev, err := lastHash(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("file di audit %s: %w", path, err)
	}
	return &FileRecorder{path: path, opts: opts, file: f, size: st.Size(), prev: prev}, nil
}

// lastHash legge l'hash dell'ultima voce del file ("" se il file è vuoto o senza catena).
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

// Record completa la voce con la catena, la scrive e la sincronizza su disco.
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

// rotateSuffix è il formato del suffisso dei file ruotati: l'ordine alfabetico è cronologico.
const rotateSuffix = "20060102T150405.000000000Z"

// rotate rinomina il file corrente con il suffisso temporale e ne apre uno nuovo.
func (r *FileRecorder) rotate() error {
	if err := r.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(r.path, r.path+"."+time.Now().UTC().Format(rotateSuffix)); err != nil {
		return fmt.Errorf("rotazione del file di audit: %w", err)
	}
	f, err := os.OpenFile(r.path, os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("rotazione del file di audit: %w", err)
	}
	r.file, r.size = f, 0
	return nil
}

// Close chiude il file.
func (r *FileRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}

// chainHash è lo sha256 (o l'HMAC-SHA256 con la chiave) della riga senza il campo hash.
func chainHash(key, body []byte) string {
	if len(key) > 0 {
		m := hmac.New(sha256.New, key)
		m.Write(body)
		return hex.EncodeToString(m.Sum(nil))
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// hashSuffix è il campo hash con cui termina ogni riga.
var hashSuffix = regexp.MustCompile(`,"hash":"([0-9a-f]{64})"\}$`)

// splitHash separa una riga nel corpo su cui è calcolato l'hash e nell'hash.
func splitHash(line []byte) (body []byte, hash string, ok bool) {
	m := hashSuffix.FindSubmatchIndex(line)
	if m == nil {
		return nil, "", false
	}
	body = append(slices.Clone(line[:m[0]]), '}')
	return body, string(line[m[2]:m[3]]), true
}

// Report è l'esito di Verify.
type Report struct {
	Files   []string `json:"files"`
	Entries int      `json:"entries"`
	// Anchor è il prev_hash della prima voce verificata: vuoto se la catena parte dall'inizio,
	// altrimenti va confrontato con l'ultima voce del file precedente (es. archiviato).
	Anchor string `json:"anchor,omitempty"`
	// Last è l'hash dell'ultima voce, da confrontare con la copia nel SIEM o in un archivio.
	Last string `json:"last,omitempty"`
}

// Files restituisce il file di audit preceduto dai suoi file ruotati, in ordine cronologico.
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

// Verify ricalcola la catena dei file indicati, in ordine cronologico, e restituisce il
// primo punto in cui è rotta.
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
				return rep, fmt.Errorf("%s:%d: voce senza hash", p, n)
			}
			var e Entry
			if err := json.Unmarshal(body, &e); err != nil {
				_ = f.Close()
				return rep, fmt.Errorf("%s:%d: voce non valida: %w", p, n, err)
			}
			if first {
				rep.Anchor, prev, first = e.PrevHash, e.PrevHash, false
			}
			if e.PrevHash != prev {
				_ = f.Close()
				return rep, fmt.Errorf("%s:%d: catena interrotta: prev_hash non corrisponde alla voce precedente (voce mancante o inserita)", p, n)
			}
			if chainHash(key, body) != hash {
				_ = f.Close()
				return rep, fmt.Errorf("%s:%d: hash non valido: voce modificata (o chiave diversa)", p, n)
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
