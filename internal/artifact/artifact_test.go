package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRelativePath(t *testing.T) {
	valid := []string{"a.txt", "samples/judger.ndjson", "a/b/c.json", "run-1_2.plan.json"}
	for _, p := range valid {
		if err := ValidateRelativePath(p); err != nil {
			t.Errorf("ValidateRelativePath(%q) = %v, want nil", p, err)
		}
	}
	invalid := []string{"", "/abs", "../up", "a/../b", "./a", "a//b", "a\\b", "~/.ssh/key", "a/./b"}
	for _, p := range invalid {
		if err := ValidateRelativePath(p); err == nil {
			t.Errorf("ValidateRelativePath(%q) = nil, want error", p)
		}
	}
}

func TestHashAndValidSHA256(t *testing.T) {
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := HashBytes([]byte("hello")); got != want {
		t.Fatalf("HashBytes = %s, want %s", got, want)
	}
	if !ValidSHA256(want) {
		t.Fatal("ValidSHA256 rejected a valid digest")
	}
	for _, bad := range []string{"", "abc", strings.ToUpper(want), want[:63]} {
		if ValidSHA256(bad) {
			t.Errorf("ValidSHA256(%q) = true, want false", bad)
		}
	}
}

func TestBuildVerifyInventory(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "samples", "a.ndjson"), "one\n")
	mustWrite(t, filepath.Join(root, "manifest.json"), "{}\n")

	inv, err := BuildInventory(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(inv.Entries))
	}
	if inv.Entries[0].Path != "manifest.json" {
		t.Fatalf("first entry = %q, want sorted manifest.json", inv.Entries[0].Path)
	}
	if err := VerifyInventory(root, inv); err != nil {
		t.Fatalf("VerifyInventory: %v", err)
	}
	if err := VerifyComplete(root, inv); err != nil {
		t.Fatalf("VerifyComplete: %v", err)
	}

	// Tampering is detected.
	mustWrite(t, filepath.Join(root, "samples", "a.ndjson"), "two\n")
	if err := VerifyInventory(root, inv); err == nil {
		t.Fatal("VerifyInventory accepted tampered content")
	}

	// Extra unlisted files are detected by VerifyComplete.
	inv2, _ := BuildInventory(root)
	mustWrite(t, filepath.Join(root, "extra.bin"), "x")
	if err := VerifyComplete(root, inv2); err == nil {
		t.Fatal("VerifyComplete accepted an unlisted file")
	}
}

func TestInventoryRejectsTraversal(t *testing.T) {
	inv := Inventory{SchemaVersion: SchemaVersion, Entries: []Entry{
		{Path: "../escape", Size: 1, SHA256: strings.Repeat("a", 64)},
	}}
	if err := inv.Validate(); err == nil {
		t.Fatal("Validate accepted traversal path")
	}
}

func TestVerifyCompleteRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "listed.txt"), "ok\n")
	inv, err := BuildInventory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("listed.txt", filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyComplete(root, inv); err == nil {
		t.Fatal("VerifyComplete accepted an unlisted symlink")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "file.txt")
	if err := WriteFileAtomic(p, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "data" {
		t.Fatalf("read = %q, %v", got, err)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestScanForSecrets(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "logs", "app.txt"), "postgres://user:supersecret@db/bench\n")
	inv, err := BuildInventory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := ScanForSecrets(root, inv, []string{"supersecret"}); err == nil {
		t.Fatal("ScanForSecrets missed a known secret")
	}

	root2 := t.TempDir()
	mustWrite(t, filepath.Join(root2, "clean.txt"), "nothing sensitive here\n")
	inv2, _ := BuildInventory(root2)
	if err := ScanForSecrets(root2, inv2, []string{"supersecret"}); err != nil {
		t.Fatalf("ScanForSecrets on clean bundle: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
