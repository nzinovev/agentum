package artifacts

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHash_StableAndLowercase(t *testing.T) {
	t.Parallel()
	got := Hash([]byte("hello world"))
	want := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if got != want {
		t.Errorf("Hash = %q, want %q", got, want)
	}
	if strings.ToUpper(got) == got && got != want {
		t.Errorf("Hash not lowercase: %q", got)
	}
}

func TestBlobStore_PutGetRoundtrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := NewBlobStore(root)
	hash := Hash([]byte("payload"))

	if err := store.Put(hash, []byte("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("payload")) {
		t.Errorf("Get = %q, want %q", got, "payload")
	}
}

func TestBlobStore_PutIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := NewBlobStore(root)
	hash := Hash([]byte("same"))
	if err := store.Put(hash, []byte("same")); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	if err := store.Put(hash, []byte("same")); err != nil {
		t.Fatalf("Put second: %v", err)
	}
	got, err := store.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("same")) {
		t.Errorf("Get = %q after idempotent Put", got)
	}
}

func TestBlobStore_PutConcurrentSameHash(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := NewBlobStore(root)
	hash := Hash([]byte("concurrent"))
	done := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() { done <- store.Put(hash, []byte("concurrent")) }()
	}
	for i := 0; i < 5; i++ {
		if err := <-done; err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	got, err := store.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("concurrent")) {
		t.Errorf("Get = %q", got)
	}
}

func TestBlobStore_GetMissing(t *testing.T) {
	t.Parallel()
	store := NewBlobStore(t.TempDir())
	_, err := store.Get(Hash([]byte("missing")))
	if err == nil {
		t.Fatal("Get of missing blob returned nil error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Logf("Get error chain: %v", err)
	}
}

func TestBlobStore_CopyToStreamsAllBytes(t *testing.T) {
	t.Parallel()
	store := NewBlobStore(t.TempDir())
	payload := []byte("stream-me")
	hash := Hash(payload)
	if err := store.Put(hash, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var buffer bytes.Buffer
	count, err := store.CopyTo(hash, &buffer)
	if err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if count != int64(len(payload)) {
		t.Errorf("CopyTo count = %d, want %d", count, len(payload))
	}
	if !bytes.Equal(buffer.Bytes(), payload) {
		t.Errorf("CopyTo bytes = %q, want %q", buffer.Bytes(), payload)
	}
}

func TestBlobStore_Reader(t *testing.T) {
	t.Parallel()
	store := NewBlobStore(t.TempDir())
	payload := []byte("read-me")
	hash := Hash(payload)
	if err := store.Put(hash, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	reader, err := store.Reader(hash)
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Reader bytes = %q", got)
	}
}

func TestBlobStore_InvalidHashRejected(t *testing.T) {
	t.Parallel()
	store := NewBlobStore(t.TempDir())
	if err := store.Put("ab", []byte("too short")); err == nil {
		t.Error("Put with too-short hash did not error")
	}
	if err := store.Put("", []byte("empty")); err == nil {
		t.Error("Put with empty hash did not error")
	}
}

// onPlatformSkip is a small helper to skip path-sensitive tests on platforms
// where the FS semantics differ in ways that aren't the test's focus.
func onPlatformSkip(t *testing.T, reason string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("path-sensitive test skipped on windows: " + reason)
	}
}

func TestBlobStore_PathLayoutShardedByHash(t *testing.T) {
	onPlatformSkip(t, "uses Posix-style path strings")
	t.Parallel()
	root := t.TempDir()
	store := NewBlobStore(root)
	hash := Hash([]byte("path-test"))
	if err := store.Put(hash, []byte("path-test")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	expectedPath := filepath.Join(root, hash[:2], hash)
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("blob not at expected path %s: %v", expectedPath, err)
	}
}
