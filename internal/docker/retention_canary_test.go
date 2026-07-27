package docker

import (
	"archive/tar"
	"io"
	"strings"
	"testing"

	dockertypes "github.com/docker/docker/api/types"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
)

const testCanaryID = "12345678-1234-5678-1234-567812345678"

func TestRetentionCanaryBuildContextIsScratchOnly(t *testing.T) {
	reader, err := retentionCanaryBuildContext()
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	archive := tar.NewReader(reader)
	header, err := archive.Next()
	if err != nil {
		t.Fatalf("read Dockerfile header: %v", err)
	}
	if header.Name != "Dockerfile" {
		t.Fatalf("unexpected build-context file %q", header.Name)
	}
	content, err := io.ReadAll(archive)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if string(content) != "FROM scratch\n" {
		t.Fatalf("unexpected Dockerfile %q", string(content))
	}
	if _, err := archive.Next(); err != io.EOF {
		t.Fatalf("expected one-file build context, got %v", err)
	}
}

func TestRetentionCanaryIdentityRequiresExactRefIDAndLabels(t *testing.T) {
	ref, err := expectedRetentionCanaryRef(testCanaryID)
	if err != nil {
		t.Fatalf("expected ref: %v", err)
	}
	config := &dockerspec.DockerOCIImageConfig{}
	config.Labels = retentionCanaryLabels(testCanaryID)
	inspect := dockertypes.ImageInspect{
		ID:       "sha256:" + strings.Repeat("a", 64),
		RepoTags: []string{ref},
		Config:   config,
	}

	result, err := validateRetentionCanaryIdentity(
		testCanaryID,
		ref,
		inspect,
	)
	if err != nil {
		t.Fatalf("validate exact identity: %v", err)
	}
	if result.ImageID != inspect.ID || result.ImageRef != ref {
		t.Fatalf("unexpected identity: %#v", result)
	}

	forged := inspect
	forgedConfig := &dockerspec.DockerOCIImageConfig{}
	forgedConfig.Labels = map[string]string{
		retentionCanaryManagedLabel:  "true",
		retentionCanaryArtifactLabel: retentionCanaryArtifactType,
		retentionCanaryIDLabel:       testCanaryID,
	}
	forged.Config = forgedConfig
	if _, err := validateRetentionCanaryIdentity(
		testCanaryID,
		ref,
		forged,
	); err == nil {
		t.Fatal("expected missing ownership-version label to fail")
	}

	if _, err := validateRetentionCanaryIdentity(
		testCanaryID,
		retentionCanaryRepository+":different",
		inspect,
	); err == nil {
		t.Fatal("expected mismatched image ref to fail")
	}
}

func TestRetentionCanaryRequestRequiresCanonicalUUID(t *testing.T) {
	for _, invalid := range []string{
		"",
		"not-a-uuid",
		"12345678-1234-5678-1234-56781234567A",
	} {
		if _, err := expectedRetentionCanaryRef(invalid); err == nil {
			t.Fatalf("expected invalid canary ID %q to fail", invalid)
		}
	}
}
