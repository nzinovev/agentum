package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// hostSuffix is a short host identifier baked into per-writer temp file names
// so two hosts sharing the same ArtifactRoot never collide on the tmp path.
// Computed once at package init.
var hostSuffix = computeHostSuffix()

// tempCounter is a process-wide counter that distinguishes concurrent writers
// on the same pid (goroutines). Atomically incremented.
var tempCounter atomic.Uint64

func computeHostSuffix() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		// Fall back to "host" so the temp name is still well-formed; two
		// hosts with no hostname collide only on pid+counter, which the
		// rename-onto-existing path handles correctly.
		return "host"
	}
	// Trim to 8 chars so the temp name stays readable; collisions across
	// hosts at the same pid+counter are tolerable (the rename handles the
	// last-writer-wins case correctly).
	if len(hostname) > 8 {
		hostname = hostname[:8]
	}
	return hostname
}

// BlobStore is the content-addressed FS backing the revisions index. Blobs
// are stored under Root at <hash[:2]>/<hash>; identical content lands on the
// same path so deduplication is structural. All operations are idempotent on
// the hash.
//
// The BlobStore is intentionally separate from the index (Store): a revision
// row references a blob by hash, but the blob has no metadata of its own. This
// keeps the durable layer composable — the same blob backs every revision that
// references it, regardless of task or name.
type BlobStore struct {
	Root string
}

// NewBlobStore returns a BlobStore rooted at root. The directory is not
// touched until the first Put; MkdirAll handles parent creation.
func NewBlobStore(root string) *BlobStore { return &BlobStore{Root: root} }

// Hash computes the sha256 hex of the bytes. Centralized so callers and the
// store agree on the canonical hashing scheme.
func Hash(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
}

// pathFor returns the on-disk blob path for a content hash.
//
// <root>/<hash[:2]>/<hash>
//
// The two-char shard keeps a large store walkable (one level of 256 buckets);
// it is not a security boundary. The hash must be at least 8 chars — short
// strings are rejected so a stray label or empty value cannot masquerade as a
// content address.
func (blobStore *BlobStore) pathFor(contentHash string) (string, error) {
	if len(contentHash) < 8 {
		return "", fmt.Errorf("artifacts: invalid content hash %q (length %d < 8)", contentHash, len(contentHash))
	}
	cleaned := strings.ToLower(contentHash)
	return filepath.Join(blobStore.Root, cleaned[:2], cleaned), nil
}

// Put writes the bytes under the hash-derived path if absent. Idempotent: a
// blob that already exists on disk is left in place. The hash is computed by
// the caller and passed in so the index write and the blob write can share one
// value without re-hashing.
//
// Concurrent Puts for the same hash are safe: each Put writes to its own temp
// file (named with a host+pid+counter suffix) and renames it into place. A
// rename onto an existing target is a no-op (target already there from a
// concurrent writer).
func (blobStore *BlobStore) Put(contentHash string, bytes []byte) error {
	blobPath, err := blobStore.pathFor(contentHash)
	if err != nil {
		return err
	}
	if blobStore.has(contentHash) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		return fmt.Errorf("artifacts: create blob parent dir: %w", err)
	}
	// Per-writer temp file so two concurrent Puts do not collide. The temp
	// lives in the same dir so the rename is atomic (same volume).
	tempPath := makeTempPath(blobPath)
	if err := os.WriteFile(tempPath, bytes, 0o644); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("artifacts: write blob tmp: %w", err)
	}
	// Rename into place. On Windows, os.Rename fails if the target exists;
	// fall through to "already there" when another writer landed first. On
	// POSIX, rename is atomic and replaces the target, which is also fine
	// here (the bytes are identical).
	if err := os.Rename(tempPath, blobPath); err != nil {
		_ = os.Remove(tempPath)
		if blobStore.has(contentHash) {
			return nil
		}
		return fmt.Errorf("artifacts: rename blob: %w", err)
	}
	return nil
}

// makeTempPath returns a per-writer temp path next to blobPath. The suffix
// embeds host + pid + a counter so two concurrent writers (even on the same
// pid, e.g. goroutines) never collide.
func makeTempPath(blobPath string) string {
	tempCounter.Add(1)
	return fmt.Sprintf("%s.%s.%d.%d.tmp", blobPath, hostSuffix, os.Getpid(), tempCounter.Load())
}

// has reports whether a blob for the hash already exists on disk.
func (blobStore *BlobStore) has(contentHash string) bool {
	blobPath, err := blobStore.pathFor(contentHash)
	if err != nil {
		return false
	}
	if _, statErr := os.Stat(blobPath); statErr == nil {
		return true
	}
	return false
}

// Get returns the blob bytes for the hash. Returns os.ErrNotExist (wrapped)
// when the blob is missing — the index references a hash the FS does not have.
func (blobStore *BlobStore) Get(contentHash string) ([]byte, error) {
	blobPath, err := blobStore.pathFor(contentHash)
	if err != nil {
		return nil, err
	}
	bytes, err := os.ReadFile(blobPath)
	if err != nil {
		return nil, fmt.Errorf("artifacts: read blob: %w", err)
	}
	return bytes, nil
}

// CopyTo streams the blob bytes for contentHash to writer. Avoids buffering the
// whole blob in memory — the HTTP GET handler uses this for large artifacts.
func (blobStore *BlobStore) CopyTo(contentHash string, writer io.Writer) (int64, error) {
	blobPath, err := blobStore.pathFor(contentHash)
	if err != nil {
		return 0, err
	}
	file, err := os.Open(blobPath)
	if err != nil {
		return 0, fmt.Errorf("artifacts: open blob: %w", err)
	}
	defer file.Close()
	count, copyErr := io.Copy(writer, file)
	if copyErr != nil {
		return count, fmt.Errorf("artifacts: copy blob: %w", copyErr)
	}
	return count, nil
}

// Reader returns an open file handle for the blob. Caller owns the Close. Use
// CopyTo when streaming is sufficient; Reader is for callers that need
// random access (none today, exposed for completeness).
func (blobStore *BlobStore) Reader(contentHash string) (io.ReadCloser, error) {
	blobPath, err := blobStore.pathFor(contentHash)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(blobPath)
	if err != nil {
		return nil, fmt.Errorf("artifacts: open blob: %w", err)
	}
	return file, nil
}
