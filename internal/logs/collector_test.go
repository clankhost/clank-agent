package logs

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"unicode/utf8"

	"github.com/clankhost/clank-agent/internal/docker"
)

func makeDockerFrame(streamType byte, payload []byte) []byte {
	hdr := make([]byte, 8)
	hdr[0] = streamType
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	return append(hdr, payload...)
}

func TestConsumeLogStreamSanitizesUTF8(t *testing.T) {
	c := NewCollector(nil)
	ci := docker.ContainerInfo{
		ContainerID: "cid-1",
		Name:        "test-app",
	}

	raw := makeDockerFrame(1, []byte("hello \xff\xfe world\n"))
	c.consumeLogStream(context.Background(), ci, "dep1", bytes.NewReader(raw))

	select {
	case entry := <-c.Entries():
		if !utf8.ValidString(entry.Line) {
			t.Fatalf("expected valid UTF-8 line, got %q", entry.Line)
		}
		if entry.Stream != "stdout" {
			t.Fatalf("stream = %q, want stdout", entry.Stream)
		}
	default:
		t.Fatal("expected a log entry to be emitted")
	}
}

func TestConsumeLogStreamDropsOversizedFrame(t *testing.T) {
	c := NewCollector(nil)
	ci := docker.ContainerInfo{
		ContainerID: "cid-tty",
		Name:        "tty-app",
	}

	// Simulate TTY ASCII bytes interpreted as a huge 4-byte big-endian frame size.
	hdr := []byte{'2', '0', '2', '6', 0x7f, 0xff, 0xff, 0xff}
	c.consumeLogStream(context.Background(), ci, "short", bytes.NewReader(hdr))

	select {
	case entry := <-c.Entries():
		t.Fatalf("unexpected log entry from oversized frame: %+v", entry)
	default:
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("abc", 8); got != "abc" {
		t.Fatalf("shortID(abc, 8) = %q, want abc", got)
	}
	if got := shortID("1234567890", 8); got != "12345678" {
		t.Fatalf("shortID(1234567890, 8) = %q, want 12345678", got)
	}
}
