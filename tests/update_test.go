package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	updatemanager "miniav/pkg/update"
)

func TestPrepareValidatesAndStagesRelativeRelease(t *testing.T) {
	base := t.TempDir()
	artifact := filepath.Join(base, "releases", "scanner-a", "2.0.0", executableName("scanner-a"))
	content := []byte("candidate worker")
	writeTestFile(t, artifact, content, 0o751)
	digest := sha256.Sum256(content)
	manifestPath := filepath.Join(base, "releases", "scanner-a", "2.0.0", "manifest.json")
	manifest := updatemanager.Manifest{
		ScannerID:        "scanner-a",
		WorkerVersion:    "2.0.0",
		SignatureVersion: "2",
		ArtifactPath:     relativePath(t, base, artifact),
		SHA256:           strings.ToUpper(hex.EncodeToString(digest[:])),
	}
	writeManifest(t, manifestPath, manifest)

	prepared, err := updatemanager.Prepare(context.Background(), base, relativePath(t, base, manifestPath))
	if err != nil {
		t.Fatal(err)
	}
	wantStaged := filepath.Join(base, "staging", "scanner-a", "2.0.0", filepath.Base(artifact))
	if prepared.Manifest.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("normalized SHA-256 = %q", prepared.Manifest.SHA256)
	}
	if prepared.ManifestPath != manifestPath || prepared.SourcePath != artifact || prepared.StagedPath != wantStaged {
		t.Fatalf("prepared release = %#v", prepared)
	}
	staged, err := os.ReadFile(prepared.StagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != string(content) {
		t.Fatalf("staged content = %q, want %q", staged, content)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(prepared.StagedPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o751 {
			t.Fatalf("staged permissions = %o, want 751", info.Mode().Perm())
		}
	}
}

func TestPrepareAcceptsAbsoluteManifestAndArtifactPaths(t *testing.T) {
	base := t.TempDir()
	artifact := filepath.Join(t.TempDir(), executableName("scanner-a"))
	content := []byte("absolute candidate")
	writeTestFile(t, artifact, content, 0o700)
	digest := sha256.Sum256(content)
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	writeManifest(t, manifestPath, updatemanager.Manifest{
		ScannerID:        "scanner-a",
		WorkerVersion:    "2.0.0",
		SignatureVersion: "2",
		ArtifactPath:     artifact,
		SHA256:           hex.EncodeToString(digest[:]),
	})

	prepared, err := updatemanager.Prepare(context.Background(), base, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ManifestPath != manifestPath || prepared.SourcePath != artifact {
		t.Fatalf("absolute paths were changed: %#v", prepared)
	}
	if !strings.HasPrefix(prepared.StagedPath, filepath.Join(base, "staging")) {
		t.Fatalf("staged path %q is outside configuration staging", prepared.StagedPath)
	}
}

func TestPrepareRejectsInvalidManifests(t *testing.T) {
	base := t.TempDir()
	artifact := filepath.Join(base, executableName("worker"))
	content := []byte("worker")
	writeTestFile(t, artifact, content, 0o700)
	digest := sha256.Sum256(content)
	validHash := hex.EncodeToString(digest[:])

	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "top level array", data: `[]`, want: "top-level value must be an object"},
		{name: "unknown field", data: `{"scannerId":"scanner-a","workerVersion":"2","signatureVersion":"2","artifactPath":"worker","sha256":"` + validHash + `","extra":true}`, want: "unknown field"},
		{name: "duplicate field", data: `{"scannerId":"scanner-a","scannerId":"scanner-b","workerVersion":"2","signatureVersion":"2","artifactPath":"worker","sha256":"` + validHash + `"}`, want: "duplicate field"},
		{name: "trailing value", data: `{"scannerId":"scanner-a","workerVersion":"2","signatureVersion":"2","artifactPath":"worker","sha256":"` + validHash + `"}{}`, want: "multiple JSON values"},
		{name: "missing scanner", data: `{"workerVersion":"2","signatureVersion":"2","artifactPath":"worker","sha256":"` + validHash + `"}`, want: "scannerId must not be empty"},
		{name: "unsafe scanner", data: `{"scannerId":"../scanner-a","workerVersion":"2","signatureVersion":"2","artifactPath":"worker","sha256":"` + validHash + `"}`, want: "safe path component"},
		{name: "unsafe version", data: `{"scannerId":"scanner-a","workerVersion":"two/next","signatureVersion":"2","artifactPath":"worker","sha256":"` + validHash + `"}`, want: "safe path component"},
		{name: "short hash", data: `{"scannerId":"scanner-a","workerVersion":"2","signatureVersion":"2","artifactPath":"worker","sha256":"abcd"}`, want: "exactly 64"},
		{name: "non hexadecimal hash", data: `{"scannerId":"scanner-a","workerVersion":"2","signatureVersion":"2","artifactPath":"worker","sha256":"` + strings.Repeat("z", 64) + `"}`, want: "not hexadecimal"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifestPath := filepath.Join(base, strings.ReplaceAll(test.name, " ", "-")+".json")
			writeTestFile(t, manifestPath, []byte(test.data), 0o600)
			_, err := updatemanager.Prepare(context.Background(), base, manifestPath)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestPrepareRejectsInvalidArtifacts(t *testing.T) {
	base := t.TempDir()
	validHash := strings.Repeat("0", 64)
	tests := []struct {
		name         string
		artifactPath string
		want         string
	}{
		{name: "missing", artifactPath: "missing-worker", want: "stat artifact"},
		{name: "directory", artifactPath: "artifact-directory", want: "not a regular file"},
	}
	if err := os.Mkdir(filepath.Join(base, "artifact-directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifestPath := filepath.Join(base, test.name+".json")
			writeManifest(t, manifestPath, updatemanager.Manifest{
				ScannerID:        "scanner-a",
				WorkerVersion:    test.name,
				SignatureVersion: "2",
				ArtifactPath:     test.artifactPath,
				SHA256:           validHash,
			})
			_, err := updatemanager.Prepare(context.Background(), base, manifestPath)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}

	artifact := filepath.Join(base, executableName("worker"))
	writeTestFile(t, artifact, []byte("worker"), 0o700)
	manifestPath := filepath.Join(base, "mismatch.json")
	writeManifest(t, manifestPath, updatemanager.Manifest{
		ScannerID:        "scanner-a",
		WorkerVersion:    "mismatch",
		SignatureVersion: "2",
		ArtifactPath:     filepath.Base(artifact),
		SHA256:           validHash,
	})
	if _, err := updatemanager.Prepare(context.Background(), base, manifestPath); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("hash mismatch error = %v", err)
	}
}

func TestPrepareRejectsInvalidManifestAndConfigurationLocations(t *testing.T) {
	base := t.TempDir()
	missingManifest := filepath.Join(base, "missing.json")
	if _, err := updatemanager.Prepare(context.Background(), base, missingManifest); err == nil || !strings.Contains(err.Error(), "stat manifest") {
		t.Fatalf("missing manifest error = %v", err)
	}
	manifestDirectory := filepath.Join(base, "manifest-directory")
	if err := os.Mkdir(manifestDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := updatemanager.Prepare(context.Background(), base, manifestDirectory); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("manifest directory error = %v", err)
	}
	configFile := filepath.Join(base, "config.json")
	writeTestFile(t, configFile, []byte("config"), 0o600)
	if _, err := updatemanager.Prepare(context.Background(), configFile, missingManifest); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("configuration file error = %v", err)
	}
}

func TestPrepareReusesMatchingStageAndRejectsConflict(t *testing.T) {
	base := t.TempDir()
	artifact := filepath.Join(base, executableName("worker"))
	content := []byte("worker")
	writeTestFile(t, artifact, content, 0o700)
	digest := sha256.Sum256(content)
	manifestPath := filepath.Join(base, "manifest.json")
	writeManifest(t, manifestPath, updatemanager.Manifest{
		ScannerID:        "scanner-a",
		WorkerVersion:    "2.0.0",
		SignatureVersion: "2",
		ArtifactPath:     filepath.Base(artifact),
		SHA256:           hex.EncodeToString(digest[:]),
	})

	first, err := updatemanager.Prepare(context.Background(), base, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := updatemanager.Prepare(context.Background(), base, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if second.StagedPath != first.StagedPath {
		t.Fatalf("reused staged path = %q, want %q", second.StagedPath, first.StagedPath)
	}
	writeTestFile(t, first.StagedPath, []byte("conflict"), 0o700)
	if _, err := updatemanager.Prepare(context.Background(), base, manifestPath); err == nil || !strings.Contains(err.Error(), "conflicts with immutable release") {
		t.Fatalf("immutable conflict error = %v", err)
	}
	staged, err := os.ReadFile(first.StagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != "conflict" {
		t.Fatalf("conflicting staged artifact was overwritten: %q", staged)
	}
}

func TestPrepareHonorsCancellationWithoutPublishing(t *testing.T) {
	base := t.TempDir()
	artifact := filepath.Join(base, executableName("worker"))
	content := []byte("worker")
	writeTestFile(t, artifact, content, 0o700)
	digest := sha256.Sum256(content)
	manifestPath := filepath.Join(base, "manifest.json")
	writeManifest(t, manifestPath, updatemanager.Manifest{
		ScannerID:        "scanner-a",
		WorkerVersion:    "2.0.0",
		SignatureVersion: "2",
		ArtifactPath:     filepath.Base(artifact),
		SHA256:           hex.EncodeToString(digest[:]),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := updatemanager.Prepare(ctx, base, manifestPath); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prepare error = %v, want context.Canceled", err)
	}
	stagedPath := filepath.Join(base, "staging", "scanner-a", "2.0.0", filepath.Base(artifact))
	if _, err := os.Stat(stagedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled prepare published %q: %v", stagedPath, err)
	}
}

func TestPrepareDownloadsAndStagesLoopbackRelease(t *testing.T) {
	base := t.TempDir()
	artifactName := executableName("scanner-a")
	content := []byte("remote candidate worker")
	digest := sha256.Sum256(content)
	manifest := updatemanager.Manifest{
		ScannerID:        "scanner-a",
		WorkerVersion:    "2.0.0",
		SignatureVersion: "2",
		ArtifactPath:     artifactName,
		SHA256:           strings.ToUpper(hex.EncodeToString(digest[:])),
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/releases/scanner-a/2.0.0/manifest.json":
			writer.Header().Set("Content-Type", "application/json")
			writer.Write(manifestData)
		case "/releases/scanner-a/2.0.0/" + artifactName:
			writer.Write(content)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	manifestURL := server.URL + "/releases/scanner-a/2.0.0/manifest.json"
	prepared, err := updatemanager.Prepare(context.Background(), base, manifestURL)
	if err != nil {
		t.Fatal(err)
	}
	wantSource := server.URL + "/releases/scanner-a/2.0.0/" + artifactName
	wantStaged := filepath.Join(base, "staging", "scanner-a", "2.0.0", artifactName)
	if prepared.ManifestPath != manifestURL || prepared.SourcePath != wantSource || prepared.StagedPath != wantStaged {
		t.Fatalf("prepared remote release = %#v", prepared)
	}
	if prepared.Manifest.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("normalized SHA-256 = %q", prepared.Manifest.SHA256)
	}
	staged, err := os.ReadFile(prepared.StagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != string(content) {
		t.Fatalf("staged content = %q, want %q", staged, content)
	}
}

func TestPrepareRemoteRejectsUnsafeSourcesAndInvalidDownloads(t *testing.T) {
	base := t.TempDir()
	if _, err := updatemanager.Prepare(context.Background(), base, "http://192.0.2.1/manifest.json"); err == nil || !strings.Contains(err.Error(), "literal loopback IP") {
		t.Fatalf("non-loopback source error = %v", err)
	}

	content := []byte("candidate")
	digest := sha256.Sum256(content)
	tests := []struct {
		name       string
		manifest   func(string) updatemanager.Manifest
		handler    func(http.ResponseWriter, *http.Request)
		want       string
		manifestOK bool
	}{
		{
			name: "absolute artifact URL",
			manifest: func(origin string) updatemanager.Manifest {
				return remoteManifest(origin+"/worker", digest)
			},
			want:       "relative URL",
			manifestOK: true,
		},
		{
			name: "artifact hash mismatch",
			manifest: func(string) updatemanager.Manifest {
				return remoteManifest("worker", sha256.Sum256([]byte("different")))
			},
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.Write(content)
			},
			want:       "SHA-256 mismatch",
			manifestOK: true,
		},
		{
			name: "artifact too large",
			manifest: func(string) updatemanager.Manifest {
				return remoteManifest("worker", digest)
			},
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Length", fmt.Sprint(128*1024*1024+1))
				writer.WriteHeader(http.StatusOK)
			},
			want:       "size limit",
			manifestOK: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var origin string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/manifest.json" && test.manifestOK {
					data, err := json.Marshal(test.manifest(origin))
					if err != nil {
						t.Error(err)
						return
					}
					writer.Write(data)
					return
				}
				if test.handler != nil {
					test.handler(writer, request)
					return
				}
				http.NotFound(writer, request)
			}))
			origin = server.URL
			defer server.Close()

			_, err := updatemanager.Prepare(context.Background(), base, server.URL+"/manifest.json")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestPrepareRemoteRejectsRedirectAndHonorsTimeout(t *testing.T) {
	base := t.TempDir()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/elsewhere", http.StatusFound)
	}))
	defer redirect.Close()
	if _, err := updatemanager.Prepare(context.Background(), base, redirect.URL+"/manifest.json"); err == nil || !strings.Contains(err.Error(), "redirects are not allowed") {
		t.Fatalf("redirect error = %v", err)
	}

	content := []byte("candidate")
	digest := sha256.Sum256(content)
	manifestData, err := json.Marshal(remoteManifest("worker", digest))
	if err != nil {
		t.Fatal(err)
	}
	timedOut := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/manifest.json" {
			writer.Write(manifestData)
			return
		}
		<-request.Context().Done()
	}))
	defer timedOut.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := updatemanager.Prepare(ctx, base, timedOut.URL+"/manifest.json"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed out prepare error = %v, want context deadline", err)
	}
}

func remoteManifest(artifactPath string, digest [sha256.Size]byte) updatemanager.Manifest {
	return updatemanager.Manifest{
		ScannerID:        "scanner-a",
		WorkerVersion:    "2.0.0",
		SignatureVersion: "2",
		ArtifactPath:     artifactPath,
		SHA256:           hex.EncodeToString(digest[:]),
	}
}

func writeManifest(t *testing.T, path string, manifest updatemanager.Manifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, data, 0o600)
}

func writeTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func relativePath(t *testing.T, base string, path string) string {
	t.Helper()
	relative, err := filepath.Rel(base, path)
	if err != nil {
		t.Fatal(err)
	}
	return relative
}

func executableName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}
