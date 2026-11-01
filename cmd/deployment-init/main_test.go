package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateTopologyRejectsUnsafeOrInvalidValues(t *testing.T) {
	valid := testOptions(t).Topology
	for name, mutate := range map[string]func(*topology){
		"wildcard bind": func(value *topology) { value.PublicBind = "0.0.0.0" },
		"public bind":   func(value *topology) { value.PublicBind = "8.8.8.8" },
		"duplicate ip":  func(value *topology) { value.WorkerIP = value.APIIP },
		"outside subnet": func(value *topology) {
			value.IngressIP = "192.168.1.20"
		},
		"ipv6 subnet": func(value *topology) { value.PrivateSubnet = "fd00::/64" },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			mutate(&value)
			if err := validateTopology(value); err == nil {
				t.Fatal("unsafe topology was accepted")
			}
		})
	}
}

func TestBootstrapRerunPreservesSecretsAndMarker(t *testing.T) {
	opts := testOptions(t)
	if err := initializeAt(opts); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	paths := []string{
		filepath.Join(opts.Roots["postgres"], "postgres_password"),
		filepath.Join(opts.Roots["rabbitmq"], "rabbitmq_password"),
		filepath.Join(opts.Roots["worker_secrets"], "ocr_worker_token"),
		filepath.Join(opts.Roots["api_secrets"], "worker_credentials"),
		filepath.Join(opts.Roots["state"], "install.json"),
	}
	before := make([][]byte, len(paths))
	for i, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read generated state: %v", err)
		}
		before[i] = data
	}
	if err := initializeAt(opts); err != nil {
		t.Fatalf("idempotent rerun: %v", err)
	}
	for i, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reread generated state: %v", err)
		}
		if string(after) != string(before[i]) {
			t.Fatalf("rerun changed persisted state file %d", i)
		}
	}
}

func TestBootstrapResumesPartialStateWithoutRotatingExistingSecret(t *testing.T) {
	opts := testOptions(t)
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	password := base64.RawURLEncoding.EncodeToString(raw[:])
	if err := writeOrVerify(opts.Roots["postgres"], "postgres_password", []byte(password), opts.PostgresOwner.UID, opts.PostgresOwner.GID, 0o400); err != nil {
		t.Fatalf("prepare partial postgres volume: %v", err)
	}
	if err := initializeAt(opts); err != nil {
		t.Fatalf("resume partial bootstrap: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(opts.Roots["postgres"], "postgres_password"))
	if err != nil || string(got) != password {
		t.Fatal("partial bootstrap did not preserve the existing database password")
	}
}

func TestBootstrapRejectsModeOrTopologyChanges(t *testing.T) {
	opts := testOptions(t)
	if err := initializeAt(opts); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	pgPath := filepath.Join(opts.Roots["postgres"], "postgres_password")
	before, err := os.ReadFile(pgPath)
	if err != nil {
		t.Fatal("read generated secret")
	}
	changed := opts
	changed.Topology.PrivateSubnet = "172.32.0.0/24"
	changed.Topology.DynamicPool = "172.32.0.128/25"
	changed.Topology.APIIP = "172.32.0.10"
	changed.Topology.WorkerIP = "172.32.0.11"
	changed.Topology.IngressIP = "172.32.0.20"
	if err := initializeAt(changed); err == nil {
		t.Fatal("changed topology was silently accepted")
	}
	after, err := os.ReadFile(pgPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed topology change altered the database secret")
	}
	workerToken := filepath.Join(opts.Roots["worker_secrets"], "ocr_worker_token")
	if err := os.Chmod(workerToken, 0o644); err != nil {
		t.Fatal("make malformed secret mode")
	}
	if err := initializeAt(opts); err == nil {
		t.Fatal("incorrect secret permissions were silently repaired")
	}
}

func TestBootstrapAllowsPublicBindChangeWithoutRotatingState(t *testing.T) {
	opts := testOptions(t)
	if err := initializeAt(opts); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	secretPath := filepath.Join(opts.Roots["worker_secrets"], "ocr_worker_token")
	before, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatal("read worker credential")
	}
	changed := opts
	changed.Topology.PublicBind = "192.168.1.40"
	if err := initializeAt(changed); err != nil {
		t.Fatalf("change host bind: %v", err)
	}
	after, err := os.ReadFile(secretPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("host bind change rotated a worker credential")
	}
}

func TestBootstrapRejectsUnexpectedVolumeEntries(t *testing.T) {
	opts := testOptions(t)
	if err := os.WriteFile(filepath.Join(opts.Roots["api_secrets"], "unexpected"), []byte("x"), 0o600); err != nil {
		t.Fatal("prepare unexpected entry")
	}
	if err := initializeAt(opts); err == nil {
		t.Fatal("unexpected volume entry was accepted")
	}
}

func testOptions(t *testing.T) options {
	t.Helper()
	base := t.TempDir()
	roots := map[string]string{}
	for _, name := range []string{"state", "postgres", "rabbitmq", "api_secrets", "worker_secrets", "api_config", "worker_config"} {
		path := filepath.Join(base, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal("create bootstrap volume fixture")
		}
		roots[name] = path
	}
	owner := fileOwner{UID: os.Geteuid(), GID: os.Getegid()}
	return options{
		Topology: topology{
			PrivateSubnet: "172.31.0.0/24", DynamicPool: "172.31.0.128/25", APIIP: "172.31.0.10", WorkerIP: "172.31.0.11",
			IngressIP: "172.31.0.20", PublicBind: "127.0.0.1", HTTPPort: 8080, AppUID: appUID, AppGID: appGID,
		},
		Roots: roots, PostgresOwner: owner, RabbitOwner: owner, RuntimeOwner: owner,
	}
}
