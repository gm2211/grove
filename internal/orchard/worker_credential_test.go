package orchard

import (
	"strings"
	"testing"

	v1 "github.com/cirruslabs/orchard/pkg/resource/v1"
)

func TestNewWorkerCredentialFitsMacOSKeychainInteractiveLimit(t *testing.T) {
	sa, bootstrap, err := newWorkerCredential(strings.Repeat("long-worker-name-", 20))
	if err != nil {
		t.Fatal(err)
	}
	if len(bootstrap) > 127 {
		t.Fatalf("bootstrap token length = %d, must fit security(1)'s 128-byte input", len(bootstrap))
	}
	if len(sa.Name) > 40 || !strings.HasPrefix(sa.Name, "grove-worker-") {
		t.Fatalf("worker account name = %q", sa.Name)
	}
	if len(sa.Token) != 32 {
		t.Fatalf("worker token length = %d, want 32 base64url characters (192 bits)", len(sa.Token))
	}
	if len(sa.Roles) != 2 || sa.Roles[0] != v1.ServiceAccountRoleComputeWrite || sa.Roles[1] != v1.ServiceAccountRoleComputeConnect {
		t.Fatalf("worker roles = %v", sa.Roles)
	}
}

func TestNewWorkerCredentialBindsExactWorkerNameAndMinimalRoles(t *testing.T) {
	workerNames := []string{
		"studio.mac-2",
		"worker-with-many-hyphens-and-dots.example.internal",
		strings.Repeat("long.worker-name-", 20),
	}

	for _, workerName := range workerNames {
		t.Run(workerName, func(t *testing.T) {
			sa, _, err := newWorkerCredential(workerName)
			if err != nil {
				t.Fatal(err)
			}
			if sa.WorkerName != workerName {
				t.Fatalf("worker binding = %q, want exact name %q", sa.WorkerName, workerName)
			}
			if len(sa.Roles) != 2 || sa.Roles[0] != v1.ServiceAccountRoleComputeWrite || sa.Roles[1] != v1.ServiceAccountRoleComputeConnect {
				t.Fatalf("worker roles = %v, want only compute:write and compute:connect", sa.Roles)
			}
		})
	}
}
