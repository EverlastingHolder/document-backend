// Command client-token creates one offline client credential pair. The raw
// token is written only to a new private file; stdout and registry files
// contain no token secret.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"passport-backend/internal/clientauth"
)

type credentialFile struct {
	SchemaVersion string       `json:"schema_version"`
	Credentials   []credential `json:"credentials"`
}

type credential struct {
	OwnerID     string `json:"owner_id"`
	TokenSHA256 string `json:"token_sha256"`
}

type workerCredentialFile struct {
	SchemaVersion string             `json:"schema_version"`
	Credentials   []workerCredential `json:"credentials"`
}

type workerCredential struct {
	WorkerID    string `json:"worker_id"`
	TokenSHA256 string `json:"token_sha256"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "client credential generation failed")
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if stdout == nil {
		return errors.New("invalid output")
	}
	flags := flag.NewFlagSet("client-token", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	privateDir := flags.String("private-dir", "", "existing private directory outside the repository")
	purposeFlag := flags.String("purpose", "client", "credential purpose: client or worker")
	ownerIDFlag := flags.String("owner-id", "", "existing opaque owner UUID for credential rotation")
	workerIDFlag := flags.String("worker-id", "", "existing opaque worker UUID for credential rotation")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*privateDir) == "" {
		return errors.New("invalid arguments")
	}
	root, err := validatePrivateDir(*privateDir)
	if err != nil {
		return err
	}
	purpose := strings.TrimSpace(*purposeFlag)
	var principalID, token, tokenSHA256, filePrefix string
	var registry []byte
	switch purpose {
	case "client":
		if strings.TrimSpace(*workerIDFlag) != "" {
			return errors.New("worker ID is only valid for worker purpose")
		}
		principalID = strings.TrimSpace(*ownerIDFlag)
		if principalID == "" {
			principalID, err = clientauth.NewOwnerID()
			if err != nil {
				return err
			}
		} else if !validOwnerID(principalID) {
			return errors.New("invalid owner ID")
		}
		token, tokenSHA256, err = clientauth.NewToken()
		filePrefix = "passport-client"
		encoded, encodeErr := json.Marshal(credentialFile{
			SchemaVersion: "passport-client-credentials/v1",
			Credentials:   []credential{{OwnerID: principalID, TokenSHA256: tokenSHA256}},
		})
		if encodeErr != nil {
			return errors.New("credential encoding failed")
		}
		registry = encoded
	case "worker":
		if strings.TrimSpace(*ownerIDFlag) != "" {
			return errors.New("owner ID is only valid for client purpose")
		}
		principalID = strings.TrimSpace(*workerIDFlag)
		if principalID == "" {
			principalID, err = clientauth.NewOwnerID()
			if err != nil {
				return err
			}
		} else if !validOwnerID(principalID) {
			return errors.New("invalid worker ID")
		}
		token, tokenSHA256, err = clientauth.NewWorkerToken()
		filePrefix = "passport-worker"
		encoded, encodeErr := json.Marshal(workerCredentialFile{
			SchemaVersion: "passport-worker-credentials/v1",
			Credentials:   []workerCredential{{WorkerID: principalID, TokenSHA256: tokenSHA256}},
		})
		if encodeErr != nil {
			return errors.New("credential encoding failed")
		}
		registry = encoded
	default:
		return errors.New("invalid credential purpose")
	}
	if err != nil {
		return err
	}
	fileID, err := clientauth.NewOwnerID()
	if err != nil {
		return err
	}
	tokenName := filePrefix + "-" + fileID + ".token"
	registryName := filePrefix + "-" + fileID + ".registry.json"
	registry = append(registry, '\n')

	dir, err := os.OpenRoot(root)
	if err != nil {
		return errors.New("private output directory unavailable")
	}
	openedInfo, err := dir.Stat(".")
	expectedInfo, expectedErr := os.Lstat(root)
	if err != nil || expectedErr != nil || !os.SameFile(openedInfo, expectedInfo) || !openedInfo.IsDir() || openedInfo.Mode().Perm() != 0o700 {
		_ = dir.Close()
		return errors.New("private output directory changed")
	}
	defer func() { _ = dir.Close() }()
	if err := writeNewPrivateFile(dir, tokenName, []byte(token+"\n")); err != nil {
		return err
	}
	if err := writeNewPrivateFile(dir, registryName, registry); err != nil {
		_ = dir.Remove(tokenName)
		_ = syncDirectory(dir)
		return err
	}
	if err := syncDirectory(dir); err != nil {
		_ = dir.Remove(tokenName)
		_ = dir.Remove(registryName)
		_ = syncDirectory(dir)
		return errors.New("private output could not be persisted")
	}
	_, _ = fmt.Fprintf(stdout, "Generated %s %s. Raw token saved to %s; hash-only registry entry saved to %s.\n", purpose, principalID,
		filepath.Join(root, tokenName), filepath.Join(root, registryName))
	return nil
}

func validatePrivateDir(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", errors.New("invalid private directory")
	}
	absolute, err = filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return "", errors.New("private directory unavailable")
	}
	absolute, err = filepath.Abs(absolute)
	if err != nil {
		return "", errors.New("invalid private directory")
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", errors.New("private directory must exist with mode 0700")
	}
	repositoryRoot, err := findRepositoryRoot()
	if err != nil {
		return "", err
	}
	repositoryRoot, err = filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return "", errors.New("repository location unavailable")
	}
	relative, err := filepath.Rel(repositoryRoot, absolute)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
		return "", errors.New("private directory must be outside the repository")
	}
	if hasGitAncestor(absolute) {
		return "", errors.New("private directory must be outside the repository")
	}
	return absolute, nil
}

func hasGitAncestor(path string) bool {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return true
		} else if !errors.Is(err, os.ErrNotExist) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func findRepositoryRoot() (string, error) {
	workingDir, err := os.Getwd()
	if err != nil {
		return "", errors.New("repository location unavailable")
	}
	for current := filepath.Clean(workingDir); ; current = filepath.Dir(current) {
		module, moduleErr := os.Stat(filepath.Join(current, "go.mod"))
		_, gitErr := os.Lstat(filepath.Join(current, ".git"))
		if moduleErr == nil && module.Mode().IsRegular() && gitErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", errors.New("repository location unavailable")
			}
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("run this helper from the repository checkout")
		}
	}
}

func validOwnerID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return value[19] == '8' || value[19] == '9' || value[19] == 'a' || value[19] == 'b'
}

func writeNewPrivateFile(root *os.Root, name string, data []byte) error {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("private output file could not be created")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = root.Remove(name)
		return errors.New("private output file could not be secured")
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		_ = file.Close()
		_ = root.Remove(name)
		return errors.New("private output file could not be written")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = root.Remove(name)
		return errors.New("private output file could not be persisted")
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(name)
		return errors.New("private output file could not be closed")
	}
	return nil
}

func syncDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
