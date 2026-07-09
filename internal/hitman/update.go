package hitman

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const updateRepo = "yanyaoer/hitman"

var (
	githubAPIBaseURL = "https://api.github.com"
	updateHTTPClient = &http.Client{Timeout: 5 * time.Minute}
)

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func cmdUpdate(args []string) error {
	target := "latest"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Fprintln(os.Stdout, "Usage: hitman update [latest|vX.Y.Z]")
			return nil
		case "latest":
			target = "latest"
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown update flag %q", args[i])
			}
			target = args[i]
		}
	}

	rel, err := fetchRelease(target)
	if err != nil {
		return err
	}
	if version != "dev" && version == rel.TagName {
		fmt.Fprintf(os.Stdout, "already up to date: %s\n", version)
		return nil
	}
	assetName := fmt.Sprintf("hitman_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	asset, err := releaseAsset(rel, assetName)
	if err != nil {
		return err
	}
	checksums, err := releaseAsset(rel, "checksums.txt")
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "hitman-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	archivePath := filepath.Join(tmp, assetName)
	sumPath := filepath.Join(tmp, "checksums.txt")
	if err := downloadAsset(asset, archivePath); err != nil {
		return err
	}
	if err := downloadAsset(checksums, sumPath); err != nil {
		return err
	}
	if err := verifyChecksum(sumPath, archivePath, assetName); err != nil {
		return err
	}
	newBinary := filepath.Join(tmp, "hitman")
	if err := extractBinary(archivePath, newBinary); err != nil {
		return err
	}
	if err := replaceCurrentExecutable(newBinary); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "updated hitman from %s to %s\n", version, rel.TagName)
	if _, err := os.Stat(launchAgentPathFromCurrentHome()); err == nil {
		fmt.Fprintln(os.Stdout, "installed services still need the new process image; run `hitman restart` when ready")
	}
	return nil
}

func fetchRelease(target string) (githubRelease, error) {
	path := "latest"
	if target != "" && target != "latest" {
		path = "tags/" + target
	}
	url := strings.TrimRight(githubAPIBaseURL, "/") + "/repos/" + updateRepo + "/releases/" + path
	var rel githubRelease
	if err := getJSON(url, &rel); err != nil {
		return githubRelease{}, err
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return githubRelease{}, fmt.Errorf("release response did not include tag_name")
	}
	return rel, nil
}

func releaseAsset(rel githubRelease, name string) (githubAsset, error) {
	for _, asset := range rel.Assets {
		if asset.Name == name && (asset.URL != "" || asset.BrowserDownloadURL != "") {
			return asset, nil
		}
	}
	return githubAsset{}, fmt.Errorf("release %s has no asset %s", rel.TagName, name)
}

func getJSON(url string, dst any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hitman/"+version)
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func downloadAsset(asset githubAsset, path string) error {
	url := asset.URL
	accept := "application/octet-stream"
	if url == "" {
		url = asset.BrowserDownloadURL
		accept = ""
	}
	return downloadFile(url, path, accept)
}

func downloadFile(url, path, accept string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "hitman/"+version)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func verifyChecksum(checksumsPath, archivePath, assetName string) error {
	data, err := os.ReadFile(checksumsPath)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == assetName {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", assetName)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: got %s want %s", assetName, got, want)
	}
	return nil
}

func extractBinary(archivePath, outPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Base(hdr.Name)
		if name != "hitman" || hdr.FileInfo().IsDir() {
			continue
		}
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
	return fmt.Errorf("archive does not contain hitman binary")
}

func replaceCurrentExecutable(newBinary string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	info, err := os.Stat(exe)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(exe), "."+filepath.Base(exe)+".new")
	if err := copyFile(newBinary, tmp, info.Mode().Perm()); err != nil {
		if os.IsPermission(err) {
			return runInteractive("sudo", "install", "-m", fmt.Sprintf("%o", info.Mode().Perm()), newBinary, exe)
		}
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		if os.IsPermission(err) {
			return runInteractive("sudo", "install", "-m", fmt.Sprintf("%o", info.Mode().Perm()), newBinary, exe)
		}
		return err
	}
	return nil
}
