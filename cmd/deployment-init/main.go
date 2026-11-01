// Command deployment-init creates the first-boot files for the portable
// Compose deployment. It is intentionally a one-shot container command: it
// never opens a network connection and stores each credential only in the
// named volume consumed by the service that needs it.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"passport-backend/internal/clientauth"
)

const (
	apiVersion       = "passport-portable-bootstrap/v1"
	upstreamVersion  = "4.10.0"
	modelLockVersion = "v11"
	appUID           = 10001
	appGID           = 10001
)

type topology struct {
	PrivateSubnet string `json:"private_subnet"`
	DynamicPool   string `json:"dynamic_pool"`
	APIIP         string `json:"api_ip"`
	WorkerIP      string `json:"worker_ip"`
	IngressIP     string `json:"ingress_ip"`
	PublicBind    string `json:"public_bind"`
	HTTPPort      int    `json:"http_port"`
	AppUID        int    `json:"app_uid"`
	AppGID        int    `json:"app_gid"`
}

type marker struct {
	SchemaVersion string              `json:"schema_version"`
	Topology      topologyFingerprint `json:"topology"`
	Files         map[string]string   `json:"files_sha256"`
}

type topologyFingerprint struct {
	PrivateSubnet string `json:"private_subnet"`
	DynamicPool   string `json:"dynamic_pool"`
	APIIP         string `json:"api_ip"`
	WorkerIP      string `json:"worker_ip"`
	IngressIP     string `json:"ingress_ip"`
	HTTPPort      int    `json:"http_port"`
	AppUID        int    `json:"app_uid"`
	AppGID        int    `json:"app_gid"`
}

type credentialRegistry struct {
	SchemaVersion string                `json:"schema_version"`
	Credentials   []workerCredentialRef `json:"credentials"`
}

type workerCredentialRef struct {
	WorkerID    string `json:"worker_id"`
	TokenSHA256 string `json:"token_sha256"`
}

type options struct {
	Topology      topology
	Roots         map[string]string
	PostgresOwner fileOwner
	RabbitOwner   fileOwner
	RuntimeOwner  fileOwner
}

type fileOwner struct {
	UID int
	GID int
}

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "deployment bootstrap failed")
		os.Exit(2)
	}
	if err := initialize(readOptions); err != nil {
		fmt.Fprintln(os.Stderr, "deployment bootstrap failed")
		os.Exit(1)
	}
	fmt.Println("deployment_bootstrap_ready")
}

func readOptions() (options, error) {
	get := func(key, fallback string) string {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
		return fallback
	}
	port := 8080
	if raw := get("PASSPORT_HTTP_PORT", "8080"); raw != "8080" {
		return options{}, errors.New("unsupported HTTP port")
	}
	t := topology{
		PrivateSubnet: get("PASSPORT_PRIVATE_SUBNET", "172.31.0.0/24"),
		DynamicPool:   get("PASSPORT_PRIVATE_DYNAMIC_POOL", "172.31.0.128/25"),
		APIIP:         get("PASSPORT_API_IP", "172.31.0.10"),
		WorkerIP:      get("PASSPORT_WORKER_IP", "172.31.0.11"),
		IngressIP:     get("PASSPORT_INGRESS_IP", "172.31.0.20"),
		PublicBind:    get("PASSPORT_PUBLIC_BIND", "127.0.0.1"),
		HTTPPort:      port,
		AppUID:        appUID,
		AppGID:        appGID,
	}
	if err := validateTopology(t); err != nil {
		return options{}, err
	}
	roots := map[string]string{
		"state":          "/bootstrap/state",
		"postgres":       "/bootstrap/postgres",
		"rabbitmq":       "/bootstrap/rabbitmq",
		"images":         "/bootstrap/images",
		"api_state":      "/bootstrap/api-state",
		"api_secrets":    "/bootstrap/api-secrets",
		"worker_secrets": "/bootstrap/worker-secrets",
		"api_config":     "/bootstrap/api-config",
		"worker_config":  "/bootstrap/worker-config",
	}
	return options{
		Topology: t, Roots: roots,
		PostgresOwner: fileOwner{UID: 70, GID: 70},
		RabbitOwner:   fileOwner{UID: 999, GID: 999},
		RuntimeOwner:  fileOwner{UID: 0, GID: 0},
	}, nil
}

func initialize(read func() (options, error)) error {
	if os.Geteuid() != 0 {
		return errors.New("bootstrap requires root inside its container")
	}
	opts, err := read()
	if err != nil {
		return err
	}
	return initializeAt(opts)
}

func initializeAt(opts options) error {
	if err := validateTopology(opts.Topology); err != nil {
		return err
	}
	allowed := map[string][]string{
		"state":          {"install.json", "worker_id"},
		"postgres":       {"postgres_password"},
		"rabbitmq":       {"rabbitmq_password"},
		"api_secrets":    {"postgres_dsn", "broker_dsn", "worker_credentials"},
		"worker_secrets": {"rabbitmq_password", "ocr_worker_token"},
		"api_config":     {"api.queue.json"},
		"worker_config":  {"ocr-worker.json"},
	}
	dirOwners := map[string]fileOwner{
		"state": opts.RuntimeOwner, "postgres": opts.PostgresOwner, "rabbitmq": opts.RabbitOwner,
		"api_secrets": opts.RuntimeOwner, "worker_secrets": opts.RuntimeOwner,
		"api_config": opts.RuntimeOwner, "worker_config": opts.RuntimeOwner,
	}
	for name, root := range opts.Roots {
		if name == "images" || name == "api_state" {
			if root == "" {
				return errors.New("invalid data volume")
			}
			continue
		}
		if _, ok := allowed[name]; !ok || root == "" {
			return errors.New("invalid bootstrap volume")
		}
		owner := dirOwners[name]
		if err := ensurePrivateRoot(root, allowed[name], owner.UID, owner.GID); err != nil {
			return err
		}
	}
	if len(opts.Roots) != len(allowed)+2 {
		return errors.New("incomplete bootstrap volumes")
	}
	if opts.Roots["images"] == "" || opts.Roots["api_state"] == "" {
		return errors.New("incomplete data volumes")
	}
	if err := ensureDataRoot(opts.Roots["images"], opts.Topology.AppUID, opts.Topology.AppGID); err != nil {
		return err
	}
	if err := ensureDataRoot(opts.Roots["api_state"], opts.Topology.AppUID, opts.Topology.AppGID); err != nil {
		return err
	}

	pgRoot, rabbitRoot := opts.Roots["postgres"], opts.Roots["rabbitmq"]
	apiSecretRoot, workerSecretRoot := opts.Roots["api_secrets"], opts.Roots["worker_secrets"]
	apiConfigRoot, workerConfigRoot := opts.Roots["api_config"], opts.Roots["worker_config"]

	pgPassword, err := secretValue(pgRoot, "postgres_password", opts.PostgresOwner.UID, opts.PostgresOwner.GID)
	if err != nil {
		return err
	}
	rabbitPassword, err := secretValue(rabbitRoot, "rabbitmq_password", opts.RabbitOwner.UID, opts.RabbitOwner.GID)
	if err != nil {
		return err
	}
	workerToken, err := secretToken(workerSecretRoot, opts.RuntimeOwner.UID, opts.RuntimeOwner.GID)
	if err != nil {
		return err
	}
	workerID, err := workerIDFor(opts.Roots["state"], opts.RuntimeOwner.UID, opts.RuntimeOwner.GID)
	if err != nil {
		return err
	}
	workerHash := sha256Hex([]byte(workerToken))

	files := map[string]struct {
		root string
		name string
		data []byte
		uid  int
		gid  int
		mode os.FileMode
	}{
		"postgres/postgres_password": {pgRoot, "postgres_password", []byte(pgPassword), opts.PostgresOwner.UID, opts.PostgresOwner.GID, 0o400},
		"rabbitmq/rabbitmq_password": {rabbitRoot, "rabbitmq_password", []byte(rabbitPassword), opts.RabbitOwner.UID, opts.RabbitOwner.GID, 0o400},
		"api_secrets/postgres_dsn":   {apiSecretRoot, "postgres_dsn", []byte(postgresDSN(pgPassword)), opts.RuntimeOwner.UID, opts.RuntimeOwner.GID, 0o600},
		"api_secrets/broker_dsn":     {apiSecretRoot, "broker_dsn", []byte(rabbitDSN(rabbitPassword)), opts.RuntimeOwner.UID, opts.RuntimeOwner.GID, 0o600},
		"api_secrets/worker_credentials": {apiSecretRoot, "worker_credentials", jsonBytes(credentialRegistry{
			SchemaVersion: "passport-worker-credentials/v1",
			Credentials:   []workerCredentialRef{{WorkerID: workerID, TokenSHA256: workerHash}},
		}), opts.RuntimeOwner.UID, opts.RuntimeOwner.GID, 0o600},
		"worker_secrets/rabbitmq_password": {workerSecretRoot, "rabbitmq_password", []byte(rabbitPassword), opts.RuntimeOwner.UID, opts.RuntimeOwner.GID, 0o600},
		"worker_secrets/ocr_worker_token":  {workerSecretRoot, "ocr_worker_token", []byte(workerToken), opts.RuntimeOwner.UID, opts.RuntimeOwner.GID, 0o600},
		"state/worker_id":                  {opts.Roots["state"], "worker_id", []byte(workerID), opts.RuntimeOwner.UID, opts.RuntimeOwner.GID, 0o600},
	}
	apiConfig := jsonBytes(apiRuntimeConfig(opts.Topology))
	workerConfig := jsonBytes(ocrRuntimeConfig(opts.Topology))
	files["api_config/api.queue.json"] = struct {
		root string
		name string
		data []byte
		uid  int
		gid  int
		mode os.FileMode
	}{apiConfigRoot, "api.queue.json", apiConfig, opts.RuntimeOwner.UID, opts.RuntimeOwner.GID, 0o600}
	files["worker_config/ocr-worker.json"] = struct {
		root string
		name string
		data []byte
		uid  int
		gid  int
		mode os.FileMode
	}{workerConfigRoot, "ocr-worker.json", workerConfig, opts.RuntimeOwner.UID, opts.RuntimeOwner.GID, 0o600}

	hashes := make(map[string]string, len(files))
	for name, item := range files {
		if err := writeOrVerify(item.root, item.name, item.data, item.uid, item.gid, item.mode); err != nil {
			return err
		}
		hashes[name] = sha256Hex(item.data)
	}
	markerValue := marker{SchemaVersion: apiVersion, Topology: fingerprint(opts.Topology), Files: hashes}
	return writeOrVerify(opts.Roots["state"], "install.json", jsonBytes(markerValue), opts.RuntimeOwner.UID, opts.RuntimeOwner.GID, 0o600)
}

func fingerprint(t topology) topologyFingerprint {
	return topologyFingerprint{
		PrivateSubnet: t.PrivateSubnet, DynamicPool: t.DynamicPool,
		APIIP: t.APIIP, WorkerIP: t.WorkerIP, IngressIP: t.IngressIP,
		HTTPPort: t.HTTPPort, AppUID: t.AppUID, AppGID: t.AppGID,
	}
}

func validateTopology(t topology) error {
	subnet, err := netip.ParsePrefix(t.PrivateSubnet)
	if err != nil || !subnet.Addr().Is4() || subnet != subnet.Masked() || subnet.Bits() > 29 || subnet.Bits() < 24 {
		return errors.New("invalid private subnet")
	}
	dynamic, err := netip.ParsePrefix(t.DynamicPool)
	if err != nil || !dynamic.Addr().Is4() || dynamic != dynamic.Masked() || dynamic.Bits() < subnet.Bits() || !subnet.Contains(dynamic.Addr()) {
		return errors.New("invalid dynamic address pool")
	}
	if !subnet.Contains(lastPrefixAddr(dynamic)) {
		return errors.New("invalid dynamic address pool")
	}
	addresses := []string{t.APIIP, t.WorkerIP, t.IngressIP}
	seen := map[netip.Addr]bool{}
	for _, raw := range addresses {
		addr, parseErr := netip.ParseAddr(raw)
		if parseErr != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() || !subnet.Contains(addr) || seen[addr] {
			return errors.New("invalid private service address")
		}
		seen[addr] = true
	}
	api, _ := netip.ParseAddr(t.APIIP)
	worker, _ := netip.ParseAddr(t.WorkerIP)
	ingress, _ := netip.ParseAddr(t.IngressIP)
	if api == subnet.Addr() || worker == subnet.Addr() || ingress == subnet.Addr() ||
		api == firstHostAddr(subnet) || worker == firstHostAddr(subnet) || ingress == firstHostAddr(subnet) ||
		dynamic.Contains(api) || dynamic.Contains(worker) || dynamic.Contains(ingress) {
		return errors.New("invalid private service address")
	}
	bind, err := netip.ParseAddr(t.PublicBind)
	if err != nil || !bind.Is4() || bind.IsUnspecified() || bind.IsMulticast() || !(bind.IsLoopback() || bind.IsPrivate()) || t.HTTPPort != 8080 {
		return errors.New("invalid public binding")
	}
	return nil
}

func firstHostAddr(prefix netip.Prefix) netip.Addr {
	value := prefix.Masked().Addr().As4()
	value[3]++
	return netip.AddrFrom4(value)
}

func lastPrefixAddr(prefix netip.Prefix) netip.Addr {
	bits := prefix.Bits()
	value := prefix.Masked().Addr().As4()
	for bit := bits; bit < 32; bit++ {
		index := bit / 8
		value[index] |= 1 << (7 - uint(bit%8))
	}
	return netip.AddrFrom4(value)
}

func ensurePrivateRoot(root string, allowed []string, uid, gid int) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("bootstrap volume unavailable")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return errors.New("bootstrap volume unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("bootstrap volume ownership unavailable")
	}
	if stat.Uid != uint32(uid) || stat.Gid != uint32(gid) || info.Mode().Perm() != 0o700 {
		if len(entries) != 0 || stat.Uid != 0 || stat.Gid != 0 {
			return errors.New("bootstrap volume ownership invalid")
		}
		if err := os.Chown(root, uid, gid); err != nil {
			return errors.New("bootstrap volume ownership invalid")
		}
		if err := os.Chmod(root, 0o700); err != nil {
			return errors.New("bootstrap volume permissions invalid")
		}
	}
	accepted := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		accepted[name] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		if accepted[name] {
			if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
				return errors.New("unexpected bootstrap volume entry")
			}
			continue
		}
		if strings.HasPrefix(name, ".passport-init-tmp-") {
			path := filepath.Join(root, name)
			st, statErr := os.Lstat(path)
			if statErr != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o600 || !ownedByRoot(st) {
				return errors.New("unsafe interrupted bootstrap file")
			}
			if err := os.Remove(path); err != nil {
				return errors.New("bootstrap recovery failed")
			}
			continue
		}
		return errors.New("unexpected bootstrap volume entry")
	}
	return nil
}

func ensureDataRoot(root string, uid, gid int) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runtime data volume unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("runtime data volume ownership unavailable")
	}
	if stat.Uid == uint32(uid) && stat.Gid == uint32(gid) && info.Mode().Perm() == 0o700 {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 || stat.Uid != 0 || stat.Gid != 0 {
		return errors.New("runtime data volume ownership invalid")
	}
	if err := os.Chown(root, uid, gid); err != nil {
		return errors.New("runtime data volume ownership invalid")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return errors.New("runtime data volume permissions invalid")
	}
	return nil
}

func ownedByRoot(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}

func secretValue(root, name string, uid, gid int) (string, error) {
	path := filepath.Join(root, name)
	if raw, err := readVerified(path, uid, gid, 0o400); err == nil {
		value := strings.TrimSpace(string(raw))
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(value)
		if decodeErr != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != value {
			return "", errors.New("invalid persisted bootstrap secret")
		}
		return value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", errors.New("secret generation failed")
	}
	value := base64.RawURLEncoding.EncodeToString(secret[:])
	if err := writeOrVerify(root, name, []byte(value), uid, gid, 0o400); err != nil {
		return "", err
	}
	return value, nil
}

func secretToken(root string, uid, gid int) (string, error) {
	path := filepath.Join(root, "ocr_worker_token")
	if raw, err := readVerified(path, uid, gid, 0o600); err == nil {
		token := strings.TrimSpace(string(raw))
		if !regexp.MustCompile(`^pwt1_[A-Za-z0-9_-]{43}$`).MatchString(token) {
			return "", errors.New("invalid persisted worker secret")
		}
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	token, _, err := clientauth.NewWorkerToken()
	if err != nil {
		return "", errors.New("worker secret generation failed")
	}
	if err := writeOrVerify(root, "ocr_worker_token", []byte(token), uid, gid, 0o600); err != nil {
		return "", err
	}
	return token, nil
}

func workerIDFor(root string, uid, gid int) (string, error) {
	// The token is generated once. A stable worker ID is separately persisted
	// as a private marker so a restart does not change the credential owner.
	path := filepath.Join(root, "worker_id")
	if raw, err := readVerified(path, uid, gid, 0o600); err == nil {
		id := strings.TrimSpace(string(raw))
		if !validUUIDv4(id) {
			return "", errors.New("invalid persisted worker identity")
		}
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id, err := clientauth.NewOwnerID()
	if err != nil {
		return "", errors.New("worker identity generation failed")
	}
	if err := writeOrVerify(root, "worker_id", []byte(id), uid, gid, 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func validUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	return err == nil && len(decoded) == 16 && value[14] == '4' && strings.ContainsRune("89ab", rune(value[19]))
}

func writeOrVerify(root, name string, contents []byte, uid, gid int, mode os.FileMode) error {
	path := filepath.Join(root, name)
	if existing, err := readVerified(path, uid, gid, mode); err == nil {
		if !bytes.Equal(existing, contents) {
			return errors.New("persisted bootstrap state differs")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp := filepath.Join(root, ".passport-init-tmp-"+name)
	if _, err := os.Lstat(temp); err == nil {
		return errors.New("interrupted bootstrap file exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("bootstrap file unavailable")
	}
	fd, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return errors.New("bootstrap file create failed")
	}
	writeErr := func() error {
		if _, err := fd.Write(contents); err != nil {
			return err
		}
		if err := fd.Sync(); err != nil {
			return err
		}
		if os.Geteuid() != uid || os.Getegid() != gid {
			if err := fd.Chown(uid, gid); err != nil {
				return err
			}
		}
		if err := fd.Chmod(mode); err != nil {
			return err
		}
		return fd.Close()
	}()
	if writeErr != nil {
		_ = fd.Close()
		return errors.New("bootstrap file write failed")
	}
	if err := os.Rename(temp, path); err != nil {
		return errors.New("bootstrap file commit failed")
	}
	dir, err := os.Open(root)
	if err != nil {
		return errors.New("bootstrap volume unavailable")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return errors.New("bootstrap volume sync failed")
	}
	return nil
}

func readVerified(path string, uid, gid int, mode os.FileMode) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode {
		return nil, errors.New("persisted bootstrap file permissions invalid")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != uid || int(stat.Gid) != gid || uint64(stat.Nlink) != 1 {
		return nil, errors.New("persisted bootstrap file ownership invalid")
	}
	return os.ReadFile(path)
}

func postgresDSN(password string) string {
	return (&url.URL{Scheme: "postgres", User: url.UserPassword("passport", password), Host: "postgres:5432", Path: "/passport", RawQuery: "sslmode=disable"}).String()
}

func rabbitDSN(password string) string {
	return (&url.URL{Scheme: "amqp", User: url.UserPassword("passport", password), Host: "rabbitmq:5672", Path: "/"}).String()
}

func apiRuntimeConfig(t topology) map[string]any {
	return map[string]any{
		"listen_address":          t.APIIP + ":8080",
		"allowed_client_networks": []string{t.IngressIP + "/32"},
		"max_upload_bytes":        8 * 1024 * 1024,
		"max_concurrent":          1,
		"pipeline_version":        "russiandocs-" + upstreamVersion + "-models-" + modelLockVersion,
		"sync_ocr_enabled":        false,
		"ocr_timeout":             "360s",
		"read_header_timeout":     "5s",
		"read_timeout":            "30s",
		"write_timeout":           "420s",
		"idle_timeout":            "30s",
		"shutdown_timeout":        "30s",
		"process_stop_grace":      "30s",
		"max_header_bytes":        16384,
		"max_output_bytes":        2 * 1024 * 1024,
		"max_image_pixels":        40_000_000,
		"logging":                 map[string]any{"path": "/var/lib/passport/state/logs/api.jsonl", "level": "info", "max_size_bytes": 5_242_880, "max_backups": 3},
		"metrics":                 map[string]any{"state_path": "/var/lib/passport/state/metrics/state.json"},
		"async_jobs":              map[string]any{"database_dsn_file": "/run/passport-api/secrets/postgres_dsn", "connect_timeout": "5s", "migration_lock_timeout": "30s", "image_root": "/var/lib/passport/images", "max_stored_image_bytes": 10 * 1024 * 1024 * 1024, "max_concurrent_uploads": 8, "client_enrollment": map[string]any{"max_clients": 10000, "max_new_clients_per_minute": 30}, "retention": map[string]any{"claim_lease": "30s", "interval": "1s", "operation_timeout": "10s", "batch_size": 100}},
		"queue_processing":        map[string]any{"worker_credentials_file": "/run/passport-api/secrets/worker_credentials", "private_listener_address": t.APIIP + ":8081", "broker_dsn_file": "/run/passport-api/secrets/broker_dsn", "connect_timeout": "5s", "poll_interval": "1s", "publisher_lease": "30s", "publish_timeout": "10s", "publish_backoff": "2s", "reconnect_min": "1s", "reconnect_max": "30s", "consumer_ack_timeout": "35m", "max_attempts": 5, "retry_initial_delay": "2s", "retry_max_delay": "1m", "hard_processing_timeout": "25m", "completion_budget": "2m", "attempt_lease": "30m", "recovery_interval": "15s"},
	}
}

func ocrRuntimeConfig(t topology) map[string]any {
	return map[string]any{
		"rabbit_host": "rabbitmq", "rabbit_port": 5672, "rabbit_user": "passport", "rabbit_password_file": "/run/secrets/rabbitmq_password", "rabbit_vhost": "/",
		"queue": "passport.recognition.work", "exchange": "passport.jobs", "routing_key": "recognition.requested",
		"dead_letter_exchange": "passport.jobs.dlx", "dead_letter_queue": "passport.recognition.dead",
		"go_worker_url": "http://api:8081", "worker_token_file": "/run/secrets/ocr_worker_token", "readiness_marker": "/run/passport-ocr/ready",
		"image_root": "/var/lib/passport/images", "russiandocs_repo": "/opt/russiandocs", "models_root": "/opt/passport-models", "work_root": "/var/lib/passport/ocr-work",
		"max_input_bytes": 8 * 1024 * 1024, "max_image_pixels": 40_000_000, "expected_upstream_version": upstreamVersion, "expected_model_lock_version": modelLockVersion,
		"startup_timeout_seconds": 600, "processing_stop_grace_seconds": 30, "http_timeout_ms": 10_000, "boundary_attempts": 3,
		"reconnect_backoff_seconds": 2, "wait_poll_max_seconds": 2, "rabbit_heartbeat_seconds": 60, "rabbit_blocked_connection_timeout_seconds": 30, "consumer_ack_timeout_ms": 2_100_000,
	}
}

func jsonBytes(value any) []byte {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		panic("internal bootstrap encoding failure")
	}
	return append(data, '\n')
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
