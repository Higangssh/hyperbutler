package compat

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestTestcontainers is Testcontainers' basic sample: nginx with a wait strategy, reached
// through the mapped port from the host. With COMPAT_LEAK=1 it also leaves a labelled
// container running, for run.ps1 to see Ryuk remove it after this process ends.
func TestTestcontainers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	nginx, err := testcontainers.Run(ctx, "nginx:1.27-alpine",
		testcontainers.WithExposedPorts("80/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/").WithPort("80/tcp").WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		t.Fatalf("run nginx: %v", err)
	}
	defer func() {
		if err := testcontainers.TerminateContainer(nginx); err != nil {
			t.Errorf("terminate: %v", err)
		}
	}()

	endpoint, err := nginx.PortEndpoint(ctx, "80/tcp", "http")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	t.Logf("nginx at %s", endpoint)
	resp, err := http.Get(endpoint)
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "nginx") {
		t.Fatalf("GET %s: status %d, body %.200q", endpoint, resp.StatusCode, body)
	}

	if os.Getenv("COMPAT_LEAK") == "1" {
		if _, err := testcontainers.Run(ctx, sampleImage,
			testcontainers.WithCmd("sleep", "600"),
		); err != nil {
			t.Fatalf("run the container left for Ryuk: %v", err)
		}
	}
}
