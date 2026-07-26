package build

import "testing"

func TestImageOwnershipLabels(t *testing.T) {
	labels := imageOwnershipLabels("deployment-123", "api")
	expected := map[string]string{
		"clank.managed":           "true",
		"clank.ownership_version": "1",
		"clank.artifact_type":     "service_image",
		"clank.deployment_id":     "deployment-123",
		"clank.service_slug":      "api",
	}

	if len(labels) != len(expected) {
		t.Fatalf("got %d labels, want %d", len(labels), len(expected))
	}
	for key, want := range expected {
		if got := labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}
}
