package signatures

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"unicode/utf8"
)

const matchBufferSize = 32 * 1024

type Store struct {
	current atomic.Pointer[database]
}

type database struct {
	patterns         []string
	patternSet       map[string]struct{}
	maxPatternLength int
}

func NewStore(path string) (*Store, error) {
	database, err := load(path)
	if err != nil {
		return nil, err
	}
	store := &Store{}
	store.current.Store(database)
	return store, nil
}

func (store *Store) Reload(path string) (int, error) {
	database, err := load(path)
	if err != nil {
		return 0, err
	}
	store.current.Store(database)
	return len(database.patternSet), nil
}

func (store *Store) Count() int {
	database := store.current.Load()
	if database == nil {
		return 0
	}
	return len(database.patternSet)
}

func (store *Store) Match(reader io.Reader) (string, bool, error) {
	if reader == nil {
		return "", false, errors.New("signature match reader must not be nil")
	}
	database := store.current.Load()
	if database == nil {
		return "", false, errors.New("signature store is not initialized")
	}
	return database.match(reader)
}

func load(path string) (*database, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat signature database %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("signature database %q is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signature database %q: %w", path, err)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("signature database %q is not valid UTF-8", path)
	}

	patterns := make([]string, 0)
	patternSet := make(map[string]struct{})
	maxPatternLength := 0
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		pattern := string(line)
		if _, exists := patternSet[pattern]; exists {
			continue
		}
		patternSet[pattern] = struct{}{}
		patterns = append(patterns, pattern)
		if len(line) > maxPatternLength {
			maxPatternLength = len(line)
		}
	}

	return &database{
		patterns:         patterns,
		patternSet:       patternSet,
		maxPatternLength: maxPatternLength,
	}, nil
}

func (database *database) match(reader io.Reader) (string, bool, error) {
	buffer := make([]byte, matchBufferSize)
	var tail []byte
	for {
		read, err := reader.Read(buffer)
		if read > 0 {
			window := make([]byte, len(tail)+read)
			copy(window, tail)
			copy(window[len(tail):], buffer[:read])
			for _, pattern := range database.patterns {
				if bytes.Contains(window, []byte(pattern)) {
					return pattern, true, nil
				}
			}
			keep := database.maxPatternLength - 1
			if keep > len(window) {
				keep = len(window)
			}
			if keep > 0 {
				tail = append(tail[:0], window[len(window)-keep:]...)
			} else {
				tail = tail[:0]
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("read scan content: %w", err)
		}
		if read == 0 {
			return "", false, io.ErrNoProgress
		}
	}
}
