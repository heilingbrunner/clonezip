package lockx

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clonezip-service.yaml.lock")

	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() first call: %v", err)
	}

	if _, err := Acquire(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire() while locked = %v, want ErrLocked", err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("Release(): %v", err)
	}

	lock2, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() after release: %v", err)
	}
	if err := lock2.Release(); err != nil {
		t.Fatalf("Release() second lock: %v", err)
	}
}
