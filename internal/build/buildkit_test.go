package build

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestBuildKitCommandArgs_BuildxAndCLI(t *testing.T) {
	req := buildRequest{
		ContextDir:     "/tmp/repo",
		DockerfilePath: "docker/Dockerfile.prod",
		FullDockerfile: "/tmp/repo/docker/Dockerfile.prod",
		ImageTag:       "clank-web:abc123",
		CacheFrom:      []string{"clank-web:prev1"},
		Labels: map[string]string{
			"clank.managed":      "true",
			"clank.service_slug": "web",
		},
		BuildArgs: map[string]string{
			"NODE_ENV": "production",
			"API_URL":  "https://api.example.com",
		},
		Target:  "runner",
		NoCache: false,
	}

	gotBuildx := buildKitCommandArgs(buildBackendBuildx, req)
	wantBuildx := []string{
		"buildx", "build", "--load",
		"--progress=plain",
		"-t", "clank-web:abc123",
		"-f", "/tmp/repo/docker/Dockerfile.prod",
		"--target", "runner",
		"--cache-from", "clank-web:prev1",
		"--build-arg", "API_URL=https://api.example.com",
		"--build-arg", "NODE_ENV=production",
		"--label", "clank.managed=true",
		"--label", "clank.service_slug=web",
		"/tmp/repo",
	}
	if !reflect.DeepEqual(gotBuildx, wantBuildx) {
		t.Fatalf("buildx args mismatch:\n got: %v\nwant: %v", gotBuildx, wantBuildx)
	}

	// Verify DOCKER_BUILDKIT=1 docker build omits buildx/--load and honors --no-cache.
	req.NoCache = true
	gotCLI := buildKitCommandArgs(buildBackendBuildKitCLI, req)
	wantCLI := []string{
		"build",
		"--progress=plain",
		"-t", "clank-web:abc123",
		"-f", "/tmp/repo/docker/Dockerfile.prod",
		"--target", "runner",
		"--no-cache",
		"--build-arg", "API_URL=https://api.example.com",
		"--build-arg", "NODE_ENV=production",
		"--label", "clank.managed=true",
		"--label", "clank.service_slug=web",
		"/tmp/repo",
	}
	if !reflect.DeepEqual(gotCLI, wantCLI) {
		t.Fatalf("cli args mismatch:\n got: %v\nwant: %v", gotCLI, wantCLI)
	}
}

func TestExecuteBuild_SelectsBuildxWhenAvailable(t *testing.T) {
	var usedBackend buildBackend
	var legacyCalled bool
	var logs []string

	b := &Builder{
		detectBuildKitFn: func(context.Context) (buildBackend, string) {
			return buildBackendBuildx, ""
		},
		runBuildKitFn: func(_ context.Context, backend buildBackend, req buildRequest, onLog func(string)) error {
			usedBackend = backend
			onLog("#1 [internal] load build definition")
			return nil
		},
		legacyBuildFn: func(context.Context, buildRequest, func(string)) error {
			legacyCalled = true
			return nil
		},
	}

	err := b.executeBuild(context.Background(), buildRequest{
		ContextDir: "/tmp/repo",
		ImageTag:   "clank-api:123",
		CacheFrom:  []string{"clank-api:old"},
	}, func(line string) { logs = append(logs, line) })
	if err != nil {
		t.Fatalf("executeBuild: %v", err)
	}
	if usedBackend != buildBackendBuildx {
		t.Fatalf("backend = %v, want buildBackendBuildx", usedBackend)
	}
	if legacyCalled {
		t.Fatal("legacy builder should not have been called")
	}
	if len(logs) < 2 || !strings.Contains(logs[0], "docker buildx (BuildKit)") {
		t.Fatalf("unexpected logs: %v", logs)
	}
}

func TestExecuteBuild_FallsBackToLegacyWhenBuildKitUnavailable(t *testing.T) {
	var buildKitCalled bool
	var legacyReq buildRequest
	var logs []string

	b := &Builder{
		detectBuildKitFn: func(context.Context) (buildBackend, string) {
			return buildBackendLegacy, "docker CLI not found in PATH"
		},
		runBuildKitFn: func(context.Context, buildBackend, buildRequest, func(string)) error {
			buildKitCalled = true
			return nil
		},
		legacyBuildFn: func(_ context.Context, req buildRequest, onLog func(string)) error {
			legacyReq = req
			onLog("Step 1/2 : FROM alpine")
			return nil
		},
	}

	req := buildRequest{
		ContextDir:     "/tmp/repo",
		DockerfilePath: "Dockerfile",
		ImageTag:       "clank-api:123",
		Target:         "prod",
		NoCache:        true,
		BuildArgs:      map[string]string{"FOO": "bar"},
	}
	if err := b.executeBuild(context.Background(), req, func(line string) { logs = append(logs, line) }); err != nil {
		t.Fatalf("executeBuild: %v", err)
	}
	if buildKitCalled {
		t.Fatal("BuildKit should not be called when unavailable")
	}
	if legacyReq.ImageTag != "clank-api:123" || legacyReq.Target != "prod" || !legacyReq.NoCache || legacyReq.BuildArgs["FOO"] != "bar" {
		t.Fatalf("legacy build did not receive full options: %+v", legacyReq)
	}
}

func TestExecuteBuild_FallsBackToLegacyOnRuntimeBuildKitUnsupportedError(t *testing.T) {
	var legacyCalled bool
	b := &Builder{
		detectBuildKitFn: func(context.Context) (buildBackend, string) {
			return buildBackendBuildKitCLI, ""
		},
		runBuildKitFn: func(context.Context, buildBackend, buildRequest, func(string)) error {
			return errors.New("exit status 1: buildkit is not supported by daemon")
		},
		legacyBuildFn: func(context.Context, buildRequest, func(string)) error {
			legacyCalled = true
			return nil
		},
	}

	if err := b.executeBuild(context.Background(), buildRequest{ImageTag: "clank-api:123"}, func(string) {}); err != nil {
		t.Fatalf("executeBuild should have succeeded via legacy fallback: %v", err)
	}
	if !legacyCalled {
		t.Fatal("expected fallback to legacy builder on BuildKit unsupported error")
	}
}

func TestExecuteBuild_RetriesBuildKitWithoutCacheFromOnCacheError(t *testing.T) {
	var attempts []buildRequest
	var legacyCalled bool
	b := &Builder{
		detectBuildKitFn: func(context.Context) (buildBackend, string) {
			return buildBackendBuildx, ""
		},
		runBuildKitFn: func(_ context.Context, _ buildBackend, req buildRequest, _ func(string)) error {
			attempts = append(attempts, req)
			if len(attempts) == 1 {
				return errors.New("failed to import cache manifest")
			}
			return nil
		},
		legacyBuildFn: func(context.Context, buildRequest, func(string)) error {
			legacyCalled = true
			return nil
		},
	}

	err := b.executeBuild(context.Background(), buildRequest{
		ImageTag:  "clank-api:123",
		CacheFrom: []string{"clank-api:old"},
	}, func(string) {})
	if err != nil {
		t.Fatalf("executeBuild: %v", err)
	}
	if legacyCalled {
		t.Fatal("should retry BuildKit without cache-from, not fall back to legacy")
	}
	if len(attempts) != 2 {
		t.Fatalf("expected 2 BuildKit attempts, got %d", len(attempts))
	}
	if len(attempts[0].CacheFrom) != 1 || len(attempts[1].CacheFrom) != 0 {
		t.Fatalf("cacheFrom progression = %v -> %v, want [clank-api:old] -> []", attempts[0].CacheFrom, attempts[1].CacheFrom)
	}
}

func TestIsBuildKitUnavailableError(t *testing.T) {
	if !isBuildKitUnavailableError(exec.ErrNotFound) {
		t.Fatal("exec.ErrNotFound should be treated as BuildKit unavailable")
	}
	if !isBuildKitUnavailableError(errors.New("unknown flag: --progress")) {
		t.Fatal("unknown --progress flag should trigger fallback")
	}
	if isBuildKitUnavailableError(errors.New("exit status 1: npm ERR! code ERESOLVE")) {
		t.Fatal("ordinary Dockerfile build error must not trigger legacy fallback")
	}
}

func TestLineLogWriterStreamsLinesAndLongOutput(t *testing.T) {
	var lines []string
	w := newLineLogWriter(func(line string) { lines = append(lines, line) })

	longPayload := strings.Repeat("x", 80*1024) // > 64KB bufio.Scanner limit
	_, _ = w.Write([]byte("line one\r\n  line two  \n" + longPayload + "\ntrailing"))
	w.Flush()

	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4", len(lines))
	}
	if lines[0] != "line one" || lines[1] != "line two" || lines[2] != longPayload || lines[3] != "trailing" {
		t.Fatalf("unexpected streamed lines: [0]=%q [1]=%q len[2]=%d [3]=%q", lines[0], lines[1], len(lines[2]), lines[3])
	}
	if w.LastLine() != "trailing" {
		t.Fatalf("LastLine = %q, want %q", w.LastLine(), "trailing")
	}
}
