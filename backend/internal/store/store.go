package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ObjectStore abstracts MinIO / S3 so that local development needs no broker.
type ObjectStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	URL(key string) string
}

// LocalObjectStore stores objects on the local filesystem under root.
type LocalObjectStore struct {
	root string
	base string
}

func NewLocalObjectStore(root, publicBase string) (*LocalObjectStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &LocalObjectStore{root: root, base: strings.TrimRight(publicBase, "/")}, nil
}

func (s *LocalObjectStore) resolve(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	full := filepath.Join(s.root, clean)
	if !strings.HasPrefix(full, filepath.Clean(s.root)+string(os.PathSeparator)) {
		return "", errors.New("object key escapes root")
	}
	return full, nil
}

func (s *LocalObjectStore) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	full, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func (s *LocalObjectStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	full, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	return os.Open(full)
}

func (s *LocalObjectStore) Delete(_ context.Context, key string) error {
	full, err := s.resolve(key)
	if err != nil {
		return err
	}
	return os.Remove(full)
}

func (s *LocalObjectStore) URL(key string) string {
	return s.base + "/" + strings.TrimLeft(key, "/")
}
