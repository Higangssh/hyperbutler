// Package compat is the Go part of bench/compat-tools (see ../README.md): the Docker SDK
// for Go and Testcontainers for Go against a running engine. It is a module of its own
// so that the main module does not depend on either; ../run.ps1 runs it with
// `go test -count=1 -run NAME`.
package compat

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// sampleImage is what the SDK samples pull: small, and the same in every language.
const sampleImage = "alpine:3.20"

// TestDockerSDK is the SDK's own basic sample: ping, pull, run, logs, remove. The client
// finds the engine as a program would, through client.FromEnv.
func TestDockerSDK(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	ping, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Logf("ping: API %s, OS %s", ping.APIVersion, ping.OSType)

	pull, err := cli.ImagePull(ctx, sampleImage, client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if _, err := io.Copy(io.Discard, pull); err != nil {
		t.Fatalf("pull: %v", err)
	}
	_ = pull.Close()

	const word = "hello-from-moby-client"
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Image:  sampleImage,
		Config: &container.Config{Cmd: []string{"echo", word}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() {
		if _, err := cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			t.Errorf("remove: %v", err)
		}
	}()
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	wait := cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case err := <-wait.Error:
		t.Fatalf("wait: %v", err)
	case st := <-wait.Result:
		if st.StatusCode != 0 {
			t.Fatalf("exit code %d", st.StatusCode)
		}
	}
	logs, err := cli.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer func() { _ = logs.Close() }()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, logs); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(stdout.String(), word) {
		t.Fatalf("logs: want %q, got stdout %q stderr %q", word, stdout.String(), stderr.String())
	}
}
