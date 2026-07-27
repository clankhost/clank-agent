package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
)

const (
	retentionCanaryRepository       = "clank-retention-canary"
	retentionCanaryManagedLabel     = "clank.managed"
	retentionCanaryOwnershipLabel   = "clank.ownership_version"
	retentionCanaryArtifactLabel    = "clank.artifact_type"
	retentionCanaryIDLabel          = "clank.retention_canary_id"
	retentionCanaryArtifactType     = "retention_canary_image"
	retentionCanaryOwnershipVersion = "1"
)

var (
	retentionCanaryUUIDPattern = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
	)
	retentionCanaryImageIDPattern = regexp.MustCompile(
		`^sha256:[0-9a-f]{64}$`,
	)
)

// RetentionCanaryImage reports the exact image identity created by the agent.
type RetentionCanaryImage struct {
	CanaryID string `json:"canary_id"`
	ImageID  string `json:"image_id"`
	ImageRef string `json:"image_ref"`
}

func expectedRetentionCanaryRef(canaryID string) (string, error) {
	if !retentionCanaryUUIDPattern.MatchString(canaryID) {
		return "", fmt.Errorf("invalid retention canary request")
	}
	return retentionCanaryRepository + ":" + canaryID, nil
}

func retentionCanaryLabels(canaryID string) map[string]string {
	return map[string]string{
		retentionCanaryManagedLabel:   "true",
		retentionCanaryOwnershipLabel: retentionCanaryOwnershipVersion,
		retentionCanaryArtifactLabel:  retentionCanaryArtifactType,
		retentionCanaryIDLabel:        canaryID,
	}
}

func retentionCanaryBuildContext() (io.Reader, error) {
	const dockerfile = "FROM scratch\n"
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	header := &tar.Header{
		Name: "Dockerfile",
		Mode: 0o600,
		Size: int64(len(dockerfile)),
	}
	if err := archive.WriteHeader(header); err != nil {
		return nil, fmt.Errorf("creating retention canary context")
	}
	if _, err := archive.Write([]byte(dockerfile)); err != nil {
		return nil, fmt.Errorf("creating retention canary context")
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("creating retention canary context")
	}
	return bytes.NewReader(payload.Bytes()), nil
}

func validateRetentionCanaryIdentity(
	canaryID string,
	imageRef string,
	inspect dockertypes.ImageInspect,
) (*RetentionCanaryImage, error) {
	expectedRef, err := expectedRetentionCanaryRef(canaryID)
	if err != nil || imageRef != expectedRef {
		return nil, fmt.Errorf("retention canary identity verification failed")
	}
	if !retentionCanaryImageIDPattern.MatchString(inspect.ID) ||
		inspect.Config == nil {
		return nil, fmt.Errorf("retention canary identity verification failed")
	}

	hasExactTag := false
	for _, tag := range inspect.RepoTags {
		if tag == imageRef {
			hasExactTag = true
			break
		}
	}
	if !hasExactTag {
		return nil, fmt.Errorf("retention canary identity verification failed")
	}

	for key, value := range retentionCanaryLabels(canaryID) {
		if inspect.Config.Labels[key] != value {
			return nil, fmt.Errorf("retention canary identity verification failed")
		}
	}
	return &RetentionCanaryImage{
		CanaryID: canaryID,
		ImageID:  inspect.ID,
		ImageRef: imageRef,
	}, nil
}

// CreateRetentionCanary builds and verifies one tiny, network-isolated image.
//
// There is deliberately no compensating delete. Any partial or mismatched
// image remains unowned and protected for later operator inspection.
func (m *Manager) CreateRetentionCanary(
	ctx context.Context,
	canaryID string,
	imageRef string,
) (*RetentionCanaryImage, error) {
	expectedRef, err := expectedRetentionCanaryRef(canaryID)
	if err != nil || imageRef != expectedRef {
		return nil, fmt.Errorf("invalid retention canary request")
	}
	contextReader, err := retentionCanaryBuildContext()
	if err != nil {
		return nil, err
	}

	response, err := m.cli.ImageBuild(
		ctx,
		contextReader,
		build.ImageBuildOptions{
			Tags:        []string{imageRef},
			Dockerfile:  "Dockerfile",
			NoCache:     true,
			Remove:      true,
			ForceRemove: true,
			PullParent:  false,
			NetworkMode: "none",
			Memory:      64 * 1024 * 1024,
			CPUQuota:    100000,
			Labels:      retentionCanaryLabels(canaryID),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("retention canary build failed")
	}
	defer response.Body.Close()

	decoder := json.NewDecoder(response.Body)
	for {
		var message struct {
			Error string `json:"error"`
		}
		if err := decoder.Decode(&message); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("retention canary build failed")
		}
		if message.Error != "" {
			return nil, fmt.Errorf("retention canary build failed")
		}
	}

	inspect, _, err := m.cli.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		return nil, fmt.Errorf("retention canary identity verification failed")
	}
	return validateRetentionCanaryIdentity(canaryID, imageRef, inspect)
}
