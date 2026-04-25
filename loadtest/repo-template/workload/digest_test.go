package workload

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDigestDir(t *testing.T) {
	root := t.TempDir()

	for index := 0; index < 120; index++ {
		name := filepath.Join(root, "sample-"+strconv.Itoa(index)+".txt")
		if err := os.WriteFile(name, []byte("runner-load-lab-"+strconv.Itoa(index)), 0o644); err != nil {
			t.Fatalf("write file %d: %v", index, err)
		}
	}

	digest, err := DigestDir(root)
	if err != nil {
		t.Fatalf("DigestDir returned error: %v", err)
	}
	if len(digest) != 64 {
		t.Fatalf("unexpected digest length: %d", len(digest))
	}
}
