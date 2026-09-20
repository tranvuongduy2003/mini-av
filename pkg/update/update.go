package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Manifest struct {
	ScannerID        string `json:"scannerId"`
	WorkerVersion    string `json:"workerVersion"`
	SignatureVersion string `json:"signatureVersion"`
	ArtifactPath     string `json:"artifactPath"`
	SHA256           string `json:"sha256"`
}

type PreparedRelease struct {
	Manifest     Manifest
	ManifestPath string
	SourcePath   string
	StagedPath   string
}

func Prepare(ctx context.Context, configDir string, manifestPath string) (PreparedRelease, error) {
	if err := ctx.Err(); err != nil {
		return PreparedRelease{}, err
	}
	base, err := absoluteDirectory(configDir)
	if err != nil {
		return PreparedRelease{}, err
	}
	resolvedManifest, err := resolvePath(base, manifestPath, "manifest")
	if err != nil {
		return PreparedRelease{}, err
	}
	manifest, err := readManifest(resolvedManifest)
	if err != nil {
		return PreparedRelease{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return PreparedRelease{}, err
	}
	manifest.SHA256 = strings.ToLower(manifest.SHA256)

	sourcePath, err := resolvePath(base, manifest.ArtifactPath, "artifact")
	if err != nil {
		return PreparedRelease{}, err
	}
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		return PreparedRelease{}, fmt.Errorf("stat artifact %q: %w", sourcePath, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return PreparedRelease{}, fmt.Errorf("artifact %q is not a regular file", sourcePath)
	}
	expected, err := hex.DecodeString(manifest.SHA256)
	if err != nil {
		return PreparedRelease{}, fmt.Errorf("decode manifest SHA-256: %w", err)
	}
	actual, err := hashFile(ctx, sourcePath)
	if err != nil {
		return PreparedRelease{}, err
	}
	if !equalDigest(actual[:], expected) {
		return PreparedRelease{}, fmt.Errorf("artifact SHA-256 mismatch: got %x, want %s", actual, manifest.SHA256)
	}

	stagedPath, err := stageArtifact(ctx, base, manifest, sourcePath, sourceInfo.Mode().Perm(), actual)
	if err != nil {
		return PreparedRelease{}, err
	}
	return PreparedRelease{
		Manifest:     manifest,
		ManifestPath: resolvedManifest,
		SourcePath:   sourcePath,
		StagedPath:   stagedPath,
	}, nil
}

func absoluteDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("configuration directory must not be empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve configuration directory %q: %w", path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("stat configuration directory %q: %w", absolute, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("configuration directory %q is not a directory", absolute)
	}
	return filepath.Clean(absolute), nil
}

func resolvePath(base string, path string, name string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s path must not be empty", name)
	}
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(base, resolved)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve %s path %q: %w", name, path, err)
	}
	return filepath.Clean(absolute), nil
}

func readManifest(path string) (Manifest, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("stat manifest %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Manifest{}, fmt.Errorf("manifest %q is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("open manifest %q: %w", path, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	opening, err := decoder.Token()
	if err != nil {
		return Manifest{}, fmt.Errorf("decode manifest %q: %w", path, err)
	}
	if opening != json.Delim('{') {
		return Manifest{}, fmt.Errorf("decode manifest %q: top-level value must be an object", path)
	}

	var manifest Manifest
	seen := make(map[string]struct{}, 5)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Manifest{}, fmt.Errorf("decode manifest %q: %w", path, err)
		}
		name, ok := token.(string)
		if !ok {
			return Manifest{}, fmt.Errorf("decode manifest %q: object key must be a string", path)
		}
		if _, duplicate := seen[name]; duplicate {
			return Manifest{}, fmt.Errorf("decode manifest %q: duplicate field %q", path, name)
		}
		seen[name] = struct{}{}

		var target *string
		switch name {
		case "scannerId":
			target = &manifest.ScannerID
		case "workerVersion":
			target = &manifest.WorkerVersion
		case "signatureVersion":
			target = &manifest.SignatureVersion
		case "artifactPath":
			target = &manifest.ArtifactPath
		case "sha256":
			target = &manifest.SHA256
		default:
			return Manifest{}, fmt.Errorf("decode manifest %q: unknown field %q", path, name)
		}
		if err := decoder.Decode(target); err != nil {
			return Manifest{}, fmt.Errorf("decode manifest %q field %q: %w", path, name, err)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return Manifest{}, fmt.Errorf("decode manifest %q: %w", path, err)
	}
	if closing != json.Delim('}') {
		return Manifest{}, fmt.Errorf("decode manifest %q: invalid object ending", path)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, fmt.Errorf("decode manifest %q: multiple JSON values", path)
		}
		return Manifest{}, fmt.Errorf("decode manifest %q: trailing data: %w", path, err)
	}
	return manifest, nil
}

func validateManifest(manifest Manifest) error {
	fields := []struct {
		name  string
		value string
	}{
		{name: "scannerId", value: manifest.ScannerID},
		{name: "workerVersion", value: manifest.WorkerVersion},
		{name: "signatureVersion", value: manifest.SignatureVersion},
		{name: "artifactPath", value: manifest.ArtifactPath},
		{name: "sha256", value: manifest.SHA256},
	}
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("manifest field %s must not be empty", field.name)
		}
	}
	if !safePathComponent(manifest.ScannerID) {
		return fmt.Errorf("manifest field scannerId %q must be a safe path component", manifest.ScannerID)
	}
	if !safePathComponent(manifest.WorkerVersion) {
		return fmt.Errorf("manifest field workerVersion %q must be a safe path component", manifest.WorkerVersion)
	}
	if len(manifest.SHA256) != sha256.Size*2 {
		return fmt.Errorf("manifest field sha256 must contain exactly %d hexadecimal characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(manifest.SHA256); err != nil {
		return fmt.Errorf("manifest field sha256 is not hexadecimal: %w", err)
	}
	return nil
}

func safePathComponent(value string) bool {
	return value != "." && value != ".." && !strings.ContainsAny(value, `/\\`) && filepath.VolumeName(value) == ""
}

func hashFile(ctx context.Context, path string) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("open artifact %q: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx: ctx, reader: file}); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("hash artifact %q: %w", path, err)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func stageArtifact(ctx context.Context, base string, manifest Manifest, sourcePath string, mode os.FileMode, expected [sha256.Size]byte) (string, error) {
	artifactName := filepath.Base(sourcePath)
	if artifactName == "." || artifactName == string(filepath.Separator) {
		return "", fmt.Errorf("artifact path %q has no file name", sourcePath)
	}
	scannerRoot := filepath.Join(base, "staging", manifest.ScannerID)
	versionRoot := filepath.Join(scannerRoot, manifest.WorkerVersion)
	stagedPath := filepath.Join(versionRoot, artifactName)
	if existing, err := reusableStagedArtifact(ctx, stagedPath, mode, expected); err != nil {
		return "", err
	} else if existing {
		return stagedPath, nil
	}
	if err := os.MkdirAll(scannerRoot, 0o755); err != nil {
		return "", fmt.Errorf("create staging directory %q: %w", scannerRoot, err)
	}
	temporaryRoot, err := os.MkdirTemp(scannerRoot, "."+manifest.WorkerVersion+"-")
	if err != nil {
		return "", fmt.Errorf("create temporary staging directory: %w", err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			os.RemoveAll(temporaryRoot)
		}
	}()

	temporaryPath := filepath.Join(temporaryRoot, artifactName)
	if err := copyAndVerify(ctx, sourcePath, temporaryPath, mode, expected); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryRoot, versionRoot); err != nil {
		if existing, checkErr := reusableStagedArtifact(ctx, stagedPath, mode, expected); checkErr != nil {
			return "", checkErr
		} else if existing {
			return stagedPath, nil
		}
		return "", fmt.Errorf("publish staged artifact %q: %w", stagedPath, err)
	}
	removeTemporary = false
	return stagedPath, nil
}

func reusableStagedArtifact(ctx context.Context, path string, expectedMode os.FileMode, expected [sha256.Size]byte) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat staged artifact %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("staged artifact %q is not a regular file", path)
	}
	if info.Mode().Perm() != expectedMode {
		return false, fmt.Errorf("staged artifact %q conflicts with immutable release permissions", path)
	}
	digest, err := hashFile(ctx, path)
	if err != nil {
		return false, err
	}
	if digest != expected {
		return false, fmt.Errorf("staged artifact %q conflicts with immutable release", path)
	}
	return true, nil
}

func copyAndVerify(ctx context.Context, sourcePath string, destinationPath string, mode os.FileMode, expected [sha256.Size]byte) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open artifact %q for staging: %w", sourcePath, err)
	}
	defer source.Close()
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create staged artifact %q: %w", destinationPath, err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(destination, hash), contextReader{ctx: ctx, reader: source})
	closeErr := destination.Close()
	if copyErr != nil {
		return fmt.Errorf("copy artifact to staging: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close staged artifact: %w", closeErr)
	}
	if err := os.Chmod(destinationPath, mode); err != nil {
		return fmt.Errorf("preserve staged artifact permissions: %w", err)
	}
	if !equalDigest(hash.Sum(nil), expected[:]) {
		return errors.New("artifact changed while it was being staged")
	}
	return nil
}

func equalDigest(left []byte, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
