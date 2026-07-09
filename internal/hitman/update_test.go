package hitman

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyChecksumAndExtractBinary(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "hitman_darwin_arm64.tar.gz")
	writeTestArchive(t, archivePath, []byte("new hitman binary"))

	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	checksumsPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(checksumsPath, []byte(fmt.Sprintf("%x  hitman_darwin_arm64.tar.gz\n", sum)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(checksumsPath, archivePath, "hitman_darwin_arm64.tar.gz"); err != nil {
		t.Fatalf("verifyChecksum: %v", err)
	}

	outPath := filepath.Join(dir, "hitman")
	if err := extractBinary(archivePath, outPath); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new hitman binary" {
		t.Fatalf("extracted binary = %q", string(got))
	}
}

func TestFetchReleaseAndDownloadAssetUseGitHubAPIAssetURL(t *testing.T) {
	var sawReleaseAuth, sawAssetAuth, sawAssetAccept bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/yanyaoer/hitman/releases/latest":
			sawReleaseAuth = r.Header.Get("Authorization") == "Bearer test-token"
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":"hitman_darwin_arm64.tar.gz","url":%q,"browser_download_url":"https://example.invalid/browser"}]}`, srvURL(r, "/assets/1"))
		case "/assets/1":
			sawAssetAuth = r.Header.Get("Authorization") == "Bearer test-token"
			sawAssetAccept = r.Header.Get("Accept") == "application/octet-stream"
			_, _ = w.Write([]byte("asset body"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withUpdateTestServer(t, srv)
	t.Setenv("GITHUB_TOKEN", "test-token")

	rel, err := fetchRelease("latest")
	if err != nil {
		t.Fatalf("fetchRelease: %v", err)
	}
	asset, err := releaseAsset(rel, "hitman_darwin_arm64.tar.gz")
	if err != nil {
		t.Fatalf("releaseAsset: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "asset")
	if err := downloadAsset(asset, outPath); err != nil {
		t.Fatalf("downloadAsset: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "asset body" {
		t.Fatalf("downloaded body = %q", string(got))
	}
	if !sawReleaseAuth || !sawAssetAuth || !sawAssetAccept {
		t.Fatalf("headers: release auth=%v asset auth=%v asset accept=%v", sawReleaseAuth, sawAssetAuth, sawAssetAccept)
	}
}

func TestReleaseAssetMissing(t *testing.T) {
	_, err := releaseAsset(githubRelease{TagName: "v1", Assets: []githubAsset{{Name: "other", URL: "https://example.invalid"}}}, "hitman_darwin_arm64.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "has no asset") {
		t.Fatalf("releaseAsset missing err = %v", err)
	}
}

func TestVerifyChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "hitman_darwin_arm64.tar.gz")
	if err := os.WriteFile(archivePath, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	checksumsPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(checksumsPath, []byte("000000  hitman_darwin_arm64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := verifyChecksum(checksumsPath, archivePath, "hitman_darwin_arm64.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("verifyChecksum mismatch err = %v", err)
	}
}

func withUpdateTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldBase := githubAPIBaseURL
	oldClient := updateHTTPClient
	githubAPIBaseURL = srv.URL
	updateHTTPClient = srv.Client()
	t.Cleanup(func() {
		githubAPIBaseURL = oldBase
		updateHTTPClient = oldClient
	})
}

func srvURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

func writeTestArchive(t *testing.T, path string, binary []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	if err := tw.WriteHeader(&tar.Header{Name: "hitman", Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
}
