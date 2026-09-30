package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeRuntimePathsDoNotBecomeCandidateSource(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		filepath.Join("src", "main.ts"),
		filepath.Join("node_modules", "library", "index.js"),
		filepath.Join("packages", "child", "node_modules", "dependency", "index.js"),
		filepath.Join(".swe-build", "cmake", "CMakeCache.txt"),
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hashes, err := snapshotHashes(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 1 || hashes["src/main.ts"] == "" {
		t.Fatalf("candidate source contains native runtime files: %v", hashes)
	}
	destination := filepath.Join(t.TempDir(), "view")
	if err := copyProjectTree(root, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "node_modules", "library", "index.js")); err != nil {
		t.Fatalf("shell view lost its prepared dependencies: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".swe-build")); !os.IsNotExist(err) {
		t.Fatalf("shell view copied a source-bound build cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.ts"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, err := snapshotHashes(root)
	if err != nil || hashes["src/main.ts"] == updated["src/main.ts"] {
		t.Fatalf("business source change was hidden: %v", err)
	}
}
