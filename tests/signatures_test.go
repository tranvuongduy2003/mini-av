package tests

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"miniav/pkg/signatures"
)

func TestSignatureStoreLoadsFiltersAndMatches(t *testing.T) {
	path := writeSignatureDatabase(t, "# comment\r\n\r\nalpha\r\nalpha\n beta \n  #literal\nβeta\n")
	store, err := signatures.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.Count() != 4 {
		t.Fatalf("signature count = %d, want 4", store.Count())
	}

	tests := []struct {
		name          string
		content       string
		wantSignature string
		wantFound     bool
	}{
		{name: "first unique pattern", content: "prefix alpha suffix", wantSignature: "alpha", wantFound: true},
		{name: "unicode", content: "prefix βeta suffix", wantSignature: "βeta", wantFound: true},
		{name: "literal whitespace", content: "prefix-beta-suffix", wantFound: false},
		{name: "indented hash is a pattern", content: "prefix  #literal suffix", wantSignature: "  #literal", wantFound: true},
		{name: "case sensitive", content: "ALPHA", wantFound: false},
		{name: "clean", content: "nothing here", wantFound: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			signature, found, err := store.Match(strings.NewReader(test.content))
			if err != nil {
				t.Fatal(err)
			}
			if signature != test.wantSignature || found != test.wantFound {
				t.Fatalf("Match = %q, %t; want %q, %t", signature, found, test.wantSignature, test.wantFound)
			}
		})
	}
}

func TestSignatureStoreAcceptsEmptyDatabase(t *testing.T) {
	store, err := signatures.NewStore(writeSignatureDatabase(t, "\n# comment\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if store.Count() != 0 {
		t.Fatalf("signature count = %d, want 0", store.Count())
	}
	if signature, found, err := store.Match(strings.NewReader("content")); err != nil || found || signature != "" {
		t.Fatalf("Match = %q, %t, %v; want no match", signature, found, err)
	}
}

func TestSignatureStoreMatchesAcrossReadBoundaries(t *testing.T) {
	store, err := signatures.NewStore(writeSignatureDatabase(t, "needle\n"))
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", 32*1024-3) + "needle"
	signature, found, err := store.Match(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if !found || signature != "needle" {
		t.Fatalf("Match = %q, %t; want needle, true", signature, found)
	}
}

func TestSignatureStoreRejectsInvalidDatabases(t *testing.T) {
	temporaryDirectory := t.TempDir()
	invalidUTF8 := filepath.Join(temporaryDirectory, "invalid.txt")
	if err := os.WriteFile(invalidUTF8, []byte{0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "missing", path: filepath.Join(temporaryDirectory, "missing.txt"), want: "stat signature database"},
		{name: "directory", path: temporaryDirectory, want: "not a regular file"},
		{name: "invalid UTF-8", path: invalidUTF8, want: "not valid UTF-8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := signatures.NewStore(test.path)
			if err == nil || store != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewStore = %#v, %v; want error containing %q", store, err, test.want)
			}
		})
	}
}

func TestSignatureReloadPublishesOnlyValidDatabase(t *testing.T) {
	oldPath := writeSignatureDatabase(t, "old-signature\n")
	newPath := writeSignatureDatabase(t, "new-signature\nnew-signature\n")
	store, err := signatures.NewStore(oldPath)
	if err != nil {
		t.Fatal(err)
	}

	count, err := store.Reload(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || store.Count() != 1 {
		t.Fatalf("reload count = %d, store count = %d; want 1, 1", count, store.Count())
	}
	assertSignatureMatch(t, store, "new-signature", "new-signature", true)
	assertSignatureMatch(t, store, "old-signature", "", false)

	if _, err := store.Reload(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Fatal("Reload accepted a missing database")
	}
	if store.Count() != 1 {
		t.Fatalf("signature count after failed reload = %d, want 1", store.Count())
	}
	assertSignatureMatch(t, store, "new-signature", "new-signature", true)
	assertSignatureMatch(t, store, "old-signature", "", false)
}

func TestSignatureReloadKeepsInFlightSnapshot(t *testing.T) {
	oldPath := writeSignatureDatabase(t, "old-signature\n")
	newPath := writeSignatureDatabase(t, "new-signature\n")
	store, err := signatures.NewStore(oldPath)
	if err != nil {
		t.Fatal(err)
	}

	reader := &gatedReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		data:    []byte("prefix old-signature suffix"),
	}
	type matchResult struct {
		signature string
		found     bool
		err       error
	}
	result := make(chan matchResult, 1)
	go func() {
		signature, found, err := store.Match(reader)
		result <- matchResult{signature: signature, found: found, err: err}
	}()
	<-reader.started

	if _, err := store.Reload(newPath); err != nil {
		t.Fatal(err)
	}
	close(reader.release)
	matched := <-result
	if matched.err != nil || !matched.found || matched.signature != "old-signature" {
		t.Fatalf("in-flight Match = %#v, want old signature", matched)
	}
	assertSignatureMatch(t, store, "old-signature", "", false)
	assertSignatureMatch(t, store, "new-signature", "new-signature", true)
}

func TestSignatureStoreConcurrentMatchAndReload(t *testing.T) {
	firstPath := writeSignatureDatabase(t, "first\n")
	secondPath := writeSignatureDatabase(t, "second\n")
	store, err := signatures.NewStore(firstPath)
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errorsFound := make(chan error, 9)
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 200; iteration++ {
				signature, found, err := store.Match(strings.NewReader("first second"))
				if err != nil {
					errorsFound <- err
					return
				}
				if !found || signature != "first" && signature != "second" {
					errorsFound <- errors.New("match observed an incomplete database")
					return
				}
				if count := store.Count(); count != 1 {
					errorsFound <- errors.New("count observed an incomplete database")
					return
				}
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		for iteration := 0; iteration < 200; iteration++ {
			path := firstPath
			if iteration%2 == 0 {
				path = secondPath
			}
			if _, err := store.Reload(path); err != nil {
				errorsFound <- err
				return
			}
		}
	}()
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
}

func TestSignatureStoreReportsMatchErrors(t *testing.T) {
	store, err := signatures.NewStore(writeSignatureDatabase(t, "signature\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Match(nil); err == nil {
		t.Fatal("Match accepted a nil reader")
	}
	readError := errors.New("read failed")
	if _, _, err := store.Match(errorReader{err: readError}); !errors.Is(err, readError) {
		t.Fatalf("Match error = %v, want %v", err, readError)
	}
}

type gatedReader struct {
	started chan struct{}
	release chan struct{}
	data    []byte
	once    sync.Once
}

func (reader *gatedReader) Read(destination []byte) (int, error) {
	reader.once.Do(func() {
		close(reader.started)
		<-reader.release
	})
	if len(reader.data) == 0 {
		return 0, io.EOF
	}
	read := copy(destination, reader.data)
	reader.data = reader.data[read:]
	return read, nil
}

type errorReader struct {
	err error
}

func (reader errorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

func writeSignatureDatabase(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "signatures.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertSignatureMatch(t *testing.T, store *signatures.Store, content string, wantSignature string, wantFound bool) {
	t.Helper()
	signature, found, err := store.Match(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if signature != wantSignature || found != wantFound {
		t.Fatalf("Match = %q, %t; want %q, %t", signature, found, wantSignature, wantFound)
	}
}
