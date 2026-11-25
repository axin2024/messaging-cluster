package media

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
)

const MaxBytes = 10 << 20

var (
	ErrLength = errors.New("attachment length differs from declaration")
	ErrDigest = errors.New("attachment digest differs from declaration")
	objectKey = regexp.MustCompile(`^[A-Z2-7]{26}$`)
)

type Blobs interface {
	Put(io.Reader, int64, [32]byte) (string, error)
	Open(string, int64) (io.ReadSeekCloser, error)
	Discard(string) error
}

// Local stores immutable objects in a directory shared by the gateway processes.
type Local struct {
	root *os.Root
}

func OpenLocal(path string) (*Local, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &Local{root: root}, nil
}

func (l *Local) Close() error { return l.root.Close() }

func (l *Local) Put(input io.Reader, size int64, digest [32]byte) (string, error) {
	if size < 1 || size > MaxBytes {
		return "", ErrLength
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	key := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random[:])
	f, err := l.root.OpenFile(key, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = l.root.Remove(key)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(input, size+1))
	if err != nil {
		return "", err
	}
	if written != size {
		return "", ErrLength
	}
	if actual := [32]byte(hash.Sum(nil)); actual != digest {
		return "", ErrDigest
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		directory, err := l.root.Open(".")
		if err != nil {
			return "", err
		}
		if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
			return "", err
		}
	}
	keep = true
	return key, nil
}

func (l *Local) Open(key string, size int64) (io.ReadSeekCloser, error) {
	if !objectKey.MatchString(key) {
		return nil, errors.New("invalid object key")
	}
	file, err := l.root.Open(key)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat attachment file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		_ = file.Close()
		return nil, errors.New("attachment file has invalid size or type")
	}
	return file, nil
}

func (l *Local) Discard(key string) error {
	if !objectKey.MatchString(key) {
		return errors.New("invalid object key")
	}
	err := l.root.Remove(key)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
