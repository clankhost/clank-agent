package docker

import (
	"testing"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
)

func inventoryLabels(deploymentID, artifactType string) map[string]string {
	return map[string]string{
		"clank.managed":           "true",
		"clank.ownership_version": "1",
		"clank.artifact_type":     artifactType,
		"clank.deployment_id":     deploymentID,
		"clank.service_slug":      "api",
		"private.secret":          "must-not-be-returned",
	}
}

func TestBuildDockerArtifactInventoryClassifiesProspectiveOwnership(t *testing.T) {
	du := dockertypes.DiskUsage{
		Containers: []*container.Summary{
			{
				ID:      "container-running",
				Names:   []string{"/clank-api"},
				Image:   "clank-api:owned",
				ImageID: "sha256:owned",
				State:   container.StateRunning,
				SizeRw:  42,
				Labels:  inventoryLabels("dep-owned", "service_container"),
			},
			{
				ID:      "container-legacy",
				ImageID: "sha256:legacy",
				State:   container.StateExited,
				Labels:  map[string]string{"clank.managed": "true"},
			},
		},
		Images: []*image.Summary{
			{
				ID:       "sha256:owned",
				RepoTags: []string{"clank-api:owned"},
				Size:     100,
				Created:  10,
				Labels:   inventoryLabels("dep-owned", "service_image"),
			},
			{
				ID:       "sha256:legacy",
				RepoTags: []string{"legacy:latest"},
				Size:     200,
				Labels:   map[string]string{"clank.managed": "true"},
			},
		},
	}

	inventory := buildDockerArtifactInventory(du, 10)

	if !inventory.Complete || inventory.ImageCount != 2 || inventory.ContainerCount != 2 {
		t.Fatalf("unexpected inventory summary: %+v", inventory)
	}
	var owned, legacy *ImageInventoryItem
	for i := range inventory.Images {
		switch inventory.Images[i].ID {
		case "sha256:owned":
			owned = &inventory.Images[i]
		case "sha256:legacy":
			legacy = &inventory.Images[i]
		}
	}
	if owned == nil || !owned.Ownership.ProspectivelyOwned {
		t.Fatalf("owned image was not classified: %+v", owned)
	}
	if owned.Ownership.DeploymentID != "dep-owned" {
		t.Fatalf("deployment ID = %q", owned.Ownership.DeploymentID)
	}
	if len(owned.ContainerIDs) != 1 || owned.ContainerIDs[0] != "container-running" {
		t.Fatalf("container refs = %#v", owned.ContainerIDs)
	}
	if !owned.InUseByRunningContainer {
		t.Fatal("owned image should be marked in use by a running container")
	}
	if legacy == nil || legacy.Ownership.ProspectivelyOwned {
		t.Fatalf("legacy image must remain unowned: %+v", legacy)
	}
}

func TestBuildDockerArtifactInventoryTruncationFailsClosed(t *testing.T) {
	du := dockertypes.DiskUsage{
		Images: []*image.Summary{
			{ID: "sha256:b"},
			{ID: "sha256:a"},
		},
	}

	inventory := buildDockerArtifactInventory(du, 1)

	if inventory.Complete {
		t.Fatal("truncated inventory must be incomplete")
	}
	if inventory.ImageCount != 2 || len(inventory.Images) != 1 {
		t.Fatalf("unexpected truncation summary: %+v", inventory)
	}
	if inventory.Images[0].ID != "sha256:a" {
		t.Fatalf("inventory not deterministic: %q", inventory.Images[0].ID)
	}
	if inventory.Images[0].RepoTags == nil ||
		inventory.Images[0].RepoDigests == nil ||
		inventory.Images[0].ContainerIDs == nil {
		t.Fatalf("inventory list fields must encode as arrays, not null: %+v", inventory.Images[0])
	}
}
