package fleet

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTempSpec(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "fleet.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestLoad_Valid(t *testing.T) {
	path := writeTempSpec(t, `
pools:
  - name: linux
    image: ghcr.io/example/linux:latest
    perWorker: 1
    cpu: 4
    memory: 8192
  - name: macos
    image: ghcr.io/example/macos:latest
    perWorker: 2
`)

	spec, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(spec.Pools) != 2 {
		t.Fatalf("want 2 pools, got %d", len(spec.Pools))
	}

	if spec.Pools[0].Name != "linux" || spec.Pools[0].CPU != 4 {
		t.Errorf("unexpected pool[0]: %+v", spec.Pools[0])
	}
}

func TestLoad_JobCPUAndJobMemory(t *testing.T) {
	path := writeTempSpec(t, `
pools:
  - name: linux
    image: ghcr.io/example/linux:latest
    perWorker: 1
    jobCPU: 1500
    jobMemory: 3072
  - name: macos
    image: ghcr.io/example/macos:latest
    perWorker: 1
`)

	spec, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if spec.Pools[0].JobCPU != 1500 || spec.Pools[0].JobMemory != 3072 {
		t.Errorf("unexpected pool[0] jobCPU/jobMemory: %+v", spec.Pools[0])
	}
	if got := spec.Pools[0].JobCPUOrDefault(); got != 1500 {
		t.Errorf("JobCPUOrDefault() = %d, want the explicit 1500", got)
	}
	if got := spec.Pools[0].JobMemoryOrDefault(); got != 3072 {
		t.Errorf("JobMemoryOrDefault() = %d, want the explicit 3072", got)
	}

	// pool[1] (macos) didn't set jobCPU/jobMemory — should fall back to the built-in defaults.
	if got := spec.Pools[1].JobCPUOrDefault(); got != DefaultJobCPU {
		t.Errorf("JobCPUOrDefault() = %d, want the default %d", got, DefaultJobCPU)
	}
	if got := spec.Pools[1].JobMemoryOrDefault(); got != DefaultJobMemory {
		t.Errorf("JobMemoryOrDefault() = %d, want the default %d", got, DefaultJobMemory)
	}
}

func TestLoad_DuplicatePoolNames(t *testing.T) {
	path := writeTempSpec(t, `
pools:
  - name: linux
    image: a
    perWorker: 1
  - name: linux
    image: b
    perWorker: 1
`)

	if _, err := Load(path); err == nil {
		t.Fatal("want error for duplicate pool names, got nil")
	}
}

func TestLoad_PerWorkerZero(t *testing.T) {
	path := writeTempSpec(t, `
pools:
  - name: linux
    image: a
    perWorker: 0
`)

	if _, err := Load(path); err == nil {
		t.Fatal("want error for perWorker < 1, got nil")
	}
}

func TestLoad_MissingImage(t *testing.T) {
	path := writeTempSpec(t, `
pools:
  - name: linux
    perWorker: 1
`)

	if _, err := Load(path); err == nil {
		t.Fatal("want error for missing image, got nil")
	}
}

func TestLoad_MissingName(t *testing.T) {
	path := writeTempSpec(t, `
pools:
  - image: a
    perWorker: 1
`)

	if _, err := Load(path); err == nil {
		t.Fatal("want error for missing pool name, got nil")
	}
}

func TestLoad_PoolNameWithDash_Rejected(t *testing.T) {
	// Pool names must be dash/dot-free so ParseVMName can split a VM name
	// "<pool>-<worker>-<n>" on the first "-" unambiguously, even though worker names (real
	// hostnames) commonly contain dashes.
	path := writeTempSpec(t, `
pools:
  - name: mac-os
    image: a
    perWorker: 1
`)

	if _, err := Load(path); err == nil {
		t.Fatal("want error for pool name containing a dash, got nil")
	}
}

func TestLoad_PoolNameWithDot_Rejected(t *testing.T) {
	path := writeTempSpec(t, `
pools:
  - name: mac.os
    image: a
    perWorker: 1
`)

	if _, err := Load(path); err == nil {
		t.Fatal("want error for pool name containing a dot, got nil")
	}
}

func TestLoad_PoolNameUppercase_Rejected(t *testing.T) {
	path := writeTempSpec(t, `
pools:
  - name: MacOS
    image: a
    perWorker: 1
`)

	if _, err := Load(path); err == nil {
		t.Fatal("want error for uppercase pool name, got nil")
	}
}

func TestLoad_PoolNameAlphanumeric_Accepted(t *testing.T) {
	path := writeTempSpec(t, `
pools:
  - name: macos2
    image: a
    perWorker: 1
`)

	if _, err := Load(path); err != nil {
		t.Fatalf("want alphanumeric pool name accepted, got: %v", err)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("want error for missing file, got nil")
	}
}
