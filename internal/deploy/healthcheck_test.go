package deploy

import (
	"strings"
	"testing"
)

func TestMatchDBHealthcheck(t *testing.T) {
	tests := []struct {
		image   string
		wantNil bool
		wantCmd string // substring expected in test[1] (the CMD-SHELL arg)
	}{
		{"postgres:16-alpine", false, "pg_isready"},
		{"postgres:latest", false, "pg_isready"},
		{"library/postgres:15", false, "pg_isready"},
		{"docker.io/library/postgres:14", false, "pg_isready"},
		{"mysql:8", false, "mysqladmin"},
		{"mariadb:11", false, "mysqladmin"},
		{"redis:7-alpine", false, "redis-cli"},
		{"mongo:7", false, "mongosh"},
		{"listmonk/listmonk:latest", true, ""},
		{"wordpress:latest", true, ""},
		{"nginx:latest", true, ""},
		{"", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			hc := matchDBHealthcheck(tt.image)
			if tt.wantNil {
				if hc != nil {
					t.Errorf("expected nil for %q, got %+v", tt.image, hc)
				}
				return
			}
			if hc == nil {
				t.Fatalf("expected non-nil for %q", tt.image)
			}
			if len(hc.test) < 2 {
				t.Fatalf("expected test to have at least 2 elements, got %v", hc.test)
			}
			if hc.test[0] != "CMD-SHELL" {
				t.Errorf("expected test[0] to be CMD-SHELL, got %q", hc.test[0])
			}
			if !strings.Contains(hc.test[1], tt.wantCmd) {
				t.Errorf("expected test[1] to contain %q, got %q", tt.wantCmd, hc.test[1])
			}
		})
	}
}
