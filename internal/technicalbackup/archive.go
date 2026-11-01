// Package technicalbackup encrypts the privacy-minimal PostgreSQL backup
// format. It never accepts arbitrary SQL dumps or filesystem archives.
package technicalbackup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"passport-backend/internal/jobstore/postgres"
	"passport-backend/internal/jsonstrict"
)

const (
	FormatVersion     = uint32(1)
	MaxArchiveSize    = 64 << 20
	archiveHeaderSize = 12
	archiveNonceSize  = 12
)

var (
	ErrInvalidArchive = errors.New("invalid technical backup archive")
	archiveMagic      = [8]byte{'P', 'S', 'T', 'T', 'E', 'C', 'H', '1'}
)

func Encrypt(snapshot postgres.TechnicalBackup, key []byte) ([]byte, error) {
	if len(key) != 32 || postgres.ValidateTechnicalBackup(snapshot) != nil {
		return nil, ErrInvalidArchive
	}
	plain, err := json.Marshal(snapshot)
	if err != nil || len(plain) > MaxArchiveSize {
		return nil, ErrInvalidArchive
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrInvalidArchive
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrInvalidArchive
	}
	archive := make([]byte, archiveHeaderSize+archiveNonceSize, archiveHeaderSize+archiveNonceSize+len(plain)+aead.Overhead())
	copy(archive[:8], archiveMagic[:])
	binary.BigEndian.PutUint32(archive[8:12], FormatVersion)
	nonce := archive[archiveHeaderSize : archiveHeaderSize+archiveNonceSize]
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrInvalidArchive
	}
	archive = aead.Seal(archive, nonce, plain, archive[:archiveHeaderSize])
	if len(archive) > MaxArchiveSize {
		return nil, ErrInvalidArchive
	}
	return archive, nil
}

func Decrypt(archive, key []byte) (postgres.TechnicalBackup, error) {
	if len(key) != 32 || len(archive) < archiveHeaderSize+archiveNonceSize+16 || len(archive) > MaxArchiveSize {
		return postgres.TechnicalBackup{}, ErrInvalidArchive
	}
	if !bytes.Equal(archive[:8], archiveMagic[:]) || binary.BigEndian.Uint32(archive[8:12]) != FormatVersion {
		return postgres.TechnicalBackup{}, ErrInvalidArchive
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return postgres.TechnicalBackup{}, ErrInvalidArchive
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return postgres.TechnicalBackup{}, ErrInvalidArchive
	}
	nonce := archive[archiveHeaderSize : archiveHeaderSize+archiveNonceSize]
	plain, err := aead.Open(nil, nonce, archive[archiveHeaderSize+archiveNonceSize:], archive[:archiveHeaderSize])
	if err != nil || len(plain) > MaxArchiveSize {
		return postgres.TechnicalBackup{}, ErrInvalidArchive
	}
	if err := jsonstrict.RejectDuplicateKeys(plain); err != nil {
		return postgres.TechnicalBackup{}, ErrInvalidArchive
	}
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	var snapshot postgres.TechnicalBackup
	if err := decoder.Decode(&snapshot); err != nil {
		return postgres.TechnicalBackup{}, ErrInvalidArchive
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return postgres.TechnicalBackup{}, ErrInvalidArchive
	}
	if err := postgres.ValidateTechnicalBackup(snapshot); err != nil {
		return postgres.TechnicalBackup{}, ErrInvalidArchive
	}
	return snapshot, nil
}
