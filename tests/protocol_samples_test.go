package tests

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"miniav/pkg/ipc"
)

func TestProtocolSamplesAreValidNDJSON(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	repositoryRoot := filepath.Dir(filepath.Dir(sourceFile))
	samples := []struct {
		name  string
		count int
	}{
		{name: "core-to-worker.ndjson", count: 5},
		{name: "worker-to-core.ndjson", count: 5},
	}

	for _, sample := range samples {
		t.Run(sample.name, func(t *testing.T) {
			path := filepath.Join(repositoryRoot, "samples", "protocol-v1", sample.name)
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()

			decoder := ipc.NewDecoder(file)
			count := 0
			for {
				if _, err := decoder.Decode(); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatalf("frame %d: %v", count+1, err)
				}
				count++
			}
			if count != sample.count {
				t.Fatalf("frame count = %d, want %d", count, sample.count)
			}
		})
	}
}
