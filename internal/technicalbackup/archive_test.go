package technicalbackup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"passport-backend/internal/jobstore/postgres"
)

func TestArchiveEncryptsAndAuthenticatesTechnicalSnapshot(t *testing.T) {
	key := bytes.Repeat([]byte{0x71}, 32)
	snapshot := postgres.TechnicalBackup{
		SchemaVersion: postgres.TechnicalBackupSchemaVersion, DatabaseSchemaVersion: 9,
		Clients: []postgres.TechnicalBackupClient{{
			ClientID: "7147cc0b-1485-49b2-9b09-3aa3c1218430", TokenSHA256: strings.Repeat("a", 64), CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		}},
		Jobs: []postgres.TechnicalBackupJob{{
			JobID: "1fe895e7-fc87-44ae-a98d-3c400083ab66", OwnerID: "7147cc0b-1485-49b2-9b09-3aa3c1218430",
			IdempotencyKey: "scan-1", RequestSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", CreatedAt: time.Unix(1_700_000_001, 0).UTC(),
		}},
	}
	archive, err := Encrypt(snapshot, key)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if bytes.Contains(archive, []byte("scan-1")) || bytes.Contains(archive, key) {
		t.Fatal("archive contains plaintext technical fields or key")
	}
	decoded, err := Decrypt(archive, key)
	if err != nil || decoded.Jobs[0].JobID != snapshot.Jobs[0].JobID || decoded.Clients[0].TokenSHA256 != snapshot.Clients[0].TokenSHA256 {
		t.Fatalf("Decrypt() = (%+v, %v), want original technical metadata", decoded, err)
	}
	wrongKey := bytes.Repeat([]byte{0x72}, 32)
	if _, err := Decrypt(archive, wrongKey); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("Decrypt() with wrong key = %v, want ErrInvalidArchive", err)
	}
	archive[len(archive)-1] ^= 1
	if _, err := Decrypt(archive, key); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("Decrypt() after ciphertext tamper = %v, want ErrInvalidArchive", err)
	}
}

func TestDecryptRejectsUnknownAndDuplicateJSONFields(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, 32)
	for _, plain := range [][]byte{
		[]byte(`{"schema_version":1,"schema_version":1,"clients":[],"jobs":[]}`),
		[]byte(`{"schema_version":1,"clients":[],"jobs":[],"unexpected":true}`),
	} {
		archive := sealForTest(t, plain, key)
		if _, err := Decrypt(archive, key); !errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("Decrypt() error = %v, want ErrInvalidArchive", err)
		}
	}
}

func sealForTest(t *testing.T, plain, key []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, archiveHeaderSize+archiveNonceSize)
	copy(header[:8], archiveMagic[:])
	binary.BigEndian.PutUint32(header[8:12], FormatVersion)
	if _, err := rand.Read(header[archiveHeaderSize:]); err != nil {
		t.Fatal(err)
	}
	return aead.Seal(header, header[archiveHeaderSize:], plain, header[:archiveHeaderSize])
}
