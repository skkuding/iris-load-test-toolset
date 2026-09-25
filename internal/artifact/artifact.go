// Package artifact implements deterministic result-bundle inventories,
// path/hash validation, atomic file writes, and secret scanning.
//
// A bundle inventory is the only trusted description of the files a run
// produced. Every path is relative, validated, and hashed. The controller
// validates the inventory before downloading anything and re-verifies sizes
// and hashes before a bundle is marked complete.
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SchemaVersion is the inventory schema this build writes and accepts.
const SchemaVersion = 1

// MaxPathLen bounds every artifact-relative path.
const MaxPathLen = 1024

// Sensitivity classifications carried in an inventory entry.
const (
	SensitivityInternal = "internal"
	SensitivitySecret   = "secret"
)

// ErrPathTraversal reports a path that is not a safe relative artifact path.
var ErrPathTraversal = errors.New("artifact: unsafe path")

// Entry describes one file in a bundle inventory.
type Entry struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	MediaType   string `json:"mediaType,omitempty"`
	Sensitivity string `json:"sensitivity,omitempty"`
}

// Inventory is a deterministic, sorted description of a bundle's files.
type Inventory struct {
	SchemaVersion int     `json:"schemaVersion"`
	Entries       []Entry `json:"entries"`
}

// ValidateRelativePath accepts only clean, slash-separated, relative paths
// without traversal segments, drive prefixes, backslashes, or NUL bytes.
func ValidateRelativePath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty", ErrPathTraversal)
	}
	if len(p) > MaxPathLen {
		return fmt.Errorf("%w: length %d exceeds %d", ErrPathTraversal, len(p), MaxPathLen)
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("%w: contains NUL", ErrPathTraversal)
	}
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("%w: contains backslash", ErrPathTraversal)
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return fmt.Errorf("%w: absolute or home-relative %q", ErrPathTraversal, p)
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("%w: absolute %q", ErrPathTraversal, p)
	}
	if p != path.Clean(p) {
		return fmt.Errorf("%w: not clean %q", ErrPathTraversal, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: bad segment %q", ErrPathTraversal, p)
		}
	}
	return nil
}

// HashBytes returns the lowercase hex SHA-256 of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashReader returns the lowercase hex SHA-256 and byte count read from r.
func HashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// HashFile returns the lowercase hex SHA-256 and size of the file at p.
func HashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return HashReader(f)
}

// ValidSHA256 reports whether s is a lowercase hex SHA-256 digest.
func ValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// BuildInventory walks root and describes every regular file below it.
// Symlinks and other non-regular entries are skipped. The result is sorted by
// path so the same tree always produces the same bytes.
func BuildInventory(root string) (Inventory, error) {
	if root == "" {
		return Inventory{}, errors.New("artifact: empty root")
	}
	inv := Inventory{SchemaVersion: SchemaVersion}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if err := ValidateRelativePath(rel); err != nil {
			return err
		}
		sum, size, err := HashFile(p)
		if err != nil {
			return err
		}
		inv.Entries = append(inv.Entries, Entry{
			Path:        rel,
			Size:        size,
			SHA256:      sum,
			MediaType:   MediaTypeFor(rel),
			Sensitivity: SensitivityInternal,
		})
		return nil
	})
	if err != nil {
		return Inventory{}, err
	}
	sort.Slice(inv.Entries, func(i, j int) bool { return inv.Entries[i].Path < inv.Entries[j].Path })
	return inv, nil
}

// MediaTypeFor guesses a media type from the file extension and falls back to
// application/octet-stream.
func MediaTypeFor(p string) string {
	if mt := mime.TypeByExtension(path.Ext(p)); mt != "" {
		return mt
	}
	switch path.Ext(p) {
	case ".ndjson", ".jsonl":
		return "application/x-ndjson"
	case ".sha256":
		return "text/plain"
	}
	return "application/octet-stream"
}

// Marshal encodes an inventory as canonical JSON (sorted entries assumed).
func (inv Inventory) Marshal() ([]byte, error) {
	out := inv
	if out.SchemaVersion == 0 {
		out.SchemaVersion = SchemaVersion
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Path < out.Entries[j].Path })
	return json.Marshal(out)
}

// ValidateInventory checks schema, unique paths, digests, and sizes.
func (inv Inventory) Validate() error {
	if inv.SchemaVersion != SchemaVersion {
		return fmt.Errorf("artifact: unsupported inventory schema %d", inv.SchemaVersion)
	}
	seen := make(map[string]struct{}, len(inv.Entries))
	for i, e := range inv.Entries {
		if err := ValidateRelativePath(e.Path); err != nil {
			return fmt.Errorf("artifact: entry %d: %w", i, err)
		}
		if _, ok := seen[e.Path]; ok {
			return fmt.Errorf("artifact: duplicate entry %q", e.Path)
		}
		seen[e.Path] = struct{}{}
		if e.Size < 0 {
			return fmt.Errorf("artifact: entry %q: negative size", e.Path)
		}
		if !ValidSHA256(e.SHA256) {
			return fmt.Errorf("artifact: entry %q: invalid sha256", e.Path)
		}
	}
	return nil
}

// VerifyInventory validates inv, then confirms every entry exists at the
// expected size and hash below root. It does not reject extra files; use
// VerifyComplete for that.
func VerifyInventory(root string, inv Inventory) error {
	if err := inv.Validate(); err != nil {
		return err
	}
	for _, e := range inv.Entries {
		full := filepath.Join(root, filepath.FromSlash(e.Path))
		info, err := os.Stat(full)
		if err != nil {
			return fmt.Errorf("artifact: entry %q: %w", e.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact: entry %q: not a regular file", e.Path)
		}
		if info.Size() != e.Size {
			return fmt.Errorf("artifact: entry %q: size %d != %d", e.Path, info.Size(), e.Size)
		}
		sum, _, err := HashFile(full)
		if err != nil {
			return err
		}
		if sum != e.SHA256 {
			return fmt.Errorf("artifact: entry %q: sha256 mismatch", e.Path)
		}
	}
	return nil
}

// VerifyComplete verifies inv and rejects any file below root that is not
// listed. Directories are ignored.
func VerifyComplete(root string, inv Inventory) error {
	if err := VerifyInventory(root, inv); err != nil {
		return err
	}
	listed := make(map[string]struct{}, len(inv.Entries))
	for _, e := range inv.Entries {
		listed[e.Path] = struct{}{}
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := listed[rel]; !ok {
			return fmt.Errorf("artifact: unlisted file %q", rel)
		}
		return nil
	})
	return err
}

// WriteFileAtomic writes data to path with perm using write-to-temp, fsync,
// and atomic rename, then fsyncs the parent directory.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// WriteJSONAtomic writes v as indented JSON atomically.
func WriteJSONAtomic(path string, v any, perm fs.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(path, data, perm)
}

var (
	credentialURL = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^/@\s:]+:[^/@\s]+@`)
	privateKey    = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
)

// ScanForSecrets reads text-like files in the inventory below root and fails
// if a known secret value or a credential-shaped pattern is found. The error
// never includes the matched secret value.
func ScanForSecrets(root string, inv Inventory, secrets []string) error {
	if err := inv.Validate(); err != nil {
		return err
	}
	known := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if len(s) >= 4 {
			known = append(known, s)
		}
	}
	for _, e := range inv.Entries {
		if !isText(e) {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(e.Path))
		data, err := readBounded(full, 8<<20)
		if err != nil {
			return err
		}
		for _, s := range known {
			if strings.Contains(data, s) {
				return fmt.Errorf("artifact: potential secret in %q", e.Path)
			}
		}
		if credentialURL.MatchString(data) {
			return fmt.Errorf("artifact: credential-shaped URL in %q", e.Path)
		}
		if privateKey.MatchString(data) {
			return fmt.Errorf("artifact: private key material in %q", e.Path)
		}
	}
	return nil
}

func isText(e Entry) bool {
	if strings.HasPrefix(e.MediaType, "text/") {
		return true
	}
	switch e.MediaType {
	case "application/json", "application/x-ndjson":
		return true
	}
	switch path.Ext(e.Path) {
	case ".json", ".ndjson", ".txt", ".log", ".yaml", ".yml", ".env", ".sha256", ".csv":
		return true
	}
	return false
}

func readBounded(p string, max int64) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := io.LimitReader(f, max)
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
