package server

import (
	"net/http"
	"testing"
	"time"
)

func privateServerTestConfig(address string) PrivateConfig {
	return PrivateConfig{
		Address: address, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second,
		WriteTimeout: 3 * time.Second, IdleTimeout: time.Second,
		ShutdownTimeout: time.Second, MaxHeaderBytes: 4096,
	}
}

func TestNewPrivateServerRequiresConcretePrivateAddress(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8081", "[::]:8081", "localhost:8081", "127.0.0.1:0"} {
		t.Run(address, func(t *testing.T) {
			if _, err := NewPrivateServer(privateServerTestConfig(address), http.NotFoundHandler()); err == nil {
				t.Fatalf("NewPrivateServer accepted non-concrete bind %q", address)
			}
		})
	}
	if _, err := NewPrivateServer(privateServerTestConfig("127.0.0.1:8766"), http.NotFoundHandler()); err != nil {
		t.Fatalf("NewPrivateServer rejected concrete private bind: %v", err)
	}
}
