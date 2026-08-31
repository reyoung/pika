package webui

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedDistributionIsProductionBuild(t *testing.T) {
	t.Parallel()
	index, err := fs.ReadFile(distribution, "dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(index)
	if strings.Contains(contents, "assets have not been built") || !strings.Contains(contents, "/assets/") {
		t.Fatalf("embedded index is not a Vite production build: %s", contents)
	}
	entries, err := fs.ReadDir(distribution, "dist/assets")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("embedded asset count = %d", len(entries))
	}
}
