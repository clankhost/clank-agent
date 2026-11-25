package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
)

func TestWaitContainerChannels_ClosedErrChDoesNotReturnZero(t *testing.T) {
	statusCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error)
	close(errCh) // closed without error before status arrives

	statusCh <- container.WaitResponse{StatusCode: 42}
	close(statusCh)

	code, err := waitContainerChannels(context.Background(), statusCh, errCh)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 42 {
		t.Fatalf("code = %d, want 42", code)
	}
}

func TestWaitContainerChannels_ContextCancelled(t *testing.T) {
	statusCh := make(chan container.WaitResponse)
	errCh := make(chan error)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, err := waitContainerChannels(ctx, statusCh, errCh)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if code != -1 {
		t.Fatalf("code = %d, want -1", code)
	}
}

func TestWaitContainerChannels_BothClosedWithoutStatus(t *testing.T) {
	statusCh := make(chan container.WaitResponse)
	errCh := make(chan error)
	close(statusCh)
	close(errCh)

	code, err := waitContainerChannels(context.Background(), statusCh, errCh)
	if err == nil {
		t.Fatal("expected error when both channels close without status")
	}
	if code != -1 {
		t.Fatalf("code = %d, want -1", code)
	}
}

func TestIsValidImageUser(t *testing.T) {
	valid := []string{"1000", "1000:1000", "node", "www-data:www-data", "_apt", "postgres:999"}
	for _, u := range valid {
		if !IsValidImageUser(u) {
			t.Errorf("IsValidImageUser(%q) = false, want true", u)
		}
	}

	invalid := []string{"", "-R", "root;rm -rf /", "user name", "$(id)", "a:b:c", "1000:'"}
	for _, u := range invalid {
		if IsValidImageUser(u) {
			t.Errorf("IsValidImageUser(%q) = true, want false", u)
		}
	}
}

func TestUniqueInitContainerName(t *testing.T) {
	a := uniqueInitContainerName()
	b := uniqueInitContainerName()
	if a == b {
		t.Fatalf("expected distinct container names, both were %q", a)
	}
}
