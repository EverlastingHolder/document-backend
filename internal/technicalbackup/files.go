package technicalbackup

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var ErrPrivateFile = errors.New("invalid private technical-backup file")

func ReadPrivate(path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, ErrPrivateFile
	}
	canonical, parent, name, err := privatePath(path)
	if err != nil {
		return nil, ErrPrivateFile
	}
	if err := checkPrivateDirectory(parent); err != nil {
		return nil, ErrPrivateFile
	}
	info, err := os.Lstat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(info) {
		return nil, ErrPrivateFile
	}
	fd, err := syscall.Open(filepath.Join(parent, name), syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrPrivateFile
	}
	file := os.NewFile(uintptr(fd), "private-input")
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(opened) ||
		!os.SameFile(info, opened) || opened.Size() < 0 || opened.Size() > maxBytes {
		return nil, ErrPrivateFile
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes {
		return nil, ErrPrivateFile
	}
	return data, nil
}

func WritePrivateNew(path string, data []byte) error {
	canonical, parent, _, err := privatePath(path)
	if err != nil || len(data) == 0 {
		return ErrPrivateFile
	}
	if err := checkPrivateDirectory(parent); err != nil {
		return ErrPrivateFile
	}
	if _, err := os.Lstat(canonical); err == nil || !errors.Is(err, os.ErrNotExist) {
		return ErrPrivateFile
	}
	temp, err := os.OpenFile(filepath.Join(parent, ".technical-backup-"+randomSuffix()), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrPrivateFile
	}
	tempName := temp.Name()
	cleanup := func() { _ = os.Remove(tempName) }
	if written, err := temp.Write(data); err != nil || written != len(data) {
		_ = temp.Close()
		cleanup()
		return ErrPrivateFile
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		cleanup()
		return ErrPrivateFile
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return ErrPrivateFile
	}
	if err := os.Link(tempName, canonical); err != nil {
		cleanup()
		return ErrPrivateFile
	}
	cleanup()
	directory, err := os.Open(parent)
	if err != nil {
		return ErrPrivateFile
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return ErrPrivateFile
	}
	return nil
}

func ReadKey(path string) ([]byte, error) {
	data, err := ReadPrivate(path, 32)
	if err != nil || len(data) != 32 {
		return nil, ErrPrivateFile
	}
	return data, nil
}

func GenerateKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, ErrPrivateFile
	}
	return key, nil
}

func privatePath(path string) (canonical, parent, name string, err error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return "", "", "", ErrPrivateFile
	}
	abs := filepath.Clean(path)
	name = filepath.Base(abs)
	if name == "." || name == string(filepath.Separator) || name == ".." {
		return "", "", "", ErrPrivateFile
	}
	parent, err = filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", "", "", ErrPrivateFile
	}
	canonical = filepath.Join(parent, name)
	return canonical, parent, name, nil
}

func checkPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || !ownedByCurrentUser(info) {
		return ErrPrivateFile
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func randomSuffix() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "random-unavailable"
	}
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(value)*2)
	for index, item := range value {
		encoded[index*2] = digits[item>>4]
		encoded[index*2+1] = digits[item&15]
	}
	return string(encoded)
}
