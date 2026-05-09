package xray

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mhsanaei/3x-ui/v2/config"
	"github.com/mhsanaei/3x-ui/v2/logger"
)

// xrayLatestZipURL is GitHub's "latest release" redirect; we don't
// pin a specific xray-core release because the management gRPC
// (HandlerService / StatsService) is stable across versions, and
// pinning means we'd have to chase rotations as releases roll.
//
// %s is filled by archSuffix().
const xrayLatestZipURL = "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-%s.zip"

// downloadHTTPTimeout is generous on purpose — corporate egress over
// poor links can take minutes for the ~25 MB archive.
const downloadHTTPTimeout = 5 * time.Minute

// archSuffix maps GOARCH to the suffix used in XTLS release filenames.
// XTLS uses "64" for amd64, "arm64-v8a" for arm64, etc.
func archSuffix() string {
	switch runtime.GOARCH {
	case "amd64":
		return "64"
	case "arm64":
		return "arm64-v8a"
	case "arm":
		return "arm32-v7a"
	case "386":
		return "32"
	default:
		return runtime.GOARCH
	}
}

// EnsureBinary downloads xray-core (plus geo data) into the bin folder
// if the binary is missing. Idempotent: if everything's already there
// it returns immediately. Logs progress on the slow path so first-run
// startup isn't silent for minutes.
//
// Called from web.Server.Start before xray is first launched. Failure
// here is fatal — without xray-core the panel cannot serve traffic in
// any mode.
func EnsureBinary() error {
	binPath := GetBinaryPath()
	if isExec(binPath) {
		// Even if the binary's there, geo data might be missing on a
		// half-bootstrap; download those separately if needed.
		ensureGeoData(filepath.Dir(binPath))
		return nil
	}

	binDir := config.GetBinFolderPath()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("create bin folder %s: %w", binDir, err)
	}

	url := fmt.Sprintf(xrayLatestZipURL, archSuffix())
	logger.Infof("xray-core: not found at %s, downloading from %s", binPath, url)

	zipPath := filepath.Join(binDir, ".xray-download.zip")
	defer os.Remove(zipPath)

	if err := downloadFile(url, zipPath); err != nil {
		return fmt.Errorf("xray-core download: %w", err)
	}

	extracted, err := unzipXrayBundle(zipPath, binDir)
	if err != nil {
		return fmt.Errorf("xray-core unzip: %w", err)
	}

	// XTLS zips ship the binary as plain "xray". Rename to the OS/arch
	// suffix our GetBinaryName expects.
	rawBinary := filepath.Join(binDir, "xray")
	if _, err := os.Stat(rawBinary); err == nil {
		if err := os.Rename(rawBinary, binPath); err != nil {
			return fmt.Errorf("rename xray binary: %w", err)
		}
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return fmt.Errorf("chmod xray: %w", err)
	}

	logger.Infof("xray-core: installed (%d files extracted) at %s", extracted, binDir)
	return nil
}

func ensureGeoData(binDir string) {
	missing := []string{}
	for _, name := range []string{"geosite.dat", "geoip.dat"} {
		if _, err := os.Stat(filepath.Join(binDir, name)); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return
	}
	logger.Warningf("xray-core: missing geo files %v — re-downloading bundle", missing)

	// Re-fetch the zip just for the geo data.
	zipPath := filepath.Join(binDir, ".xray-download.zip")
	defer os.Remove(zipPath)
	url := fmt.Sprintf(xrayLatestZipURL, archSuffix())
	if err := downloadFile(url, zipPath); err != nil {
		logger.Warningf("xray-core: geo re-download failed: %v", err)
		return
	}
	if _, err := unzipXrayBundle(zipPath, binDir); err != nil {
		logger.Warningf("xray-core: geo extract failed: %v", err)
	}
}

func isExec(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.Mode()&0o111 != 0 && !st.IsDir()
}

func downloadFile(url, dst string) error {
	client := &http.Client{Timeout: downloadHTTPTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return out.Sync()
}

// unzipXrayBundle extracts only the entries we actually need from the
// XTLS release archive. Returns the count of files written. Skips
// LICENSE/README/etc. — we want a deterministic minimal install.
func unzipXrayBundle(src, destDir string) (int, error) {
	r, err := zip.OpenReader(src)
	if err != nil {
		return 0, err
	}
	defer r.Close()

	wanted := map[string]bool{
		"xray":        true,
		"geosite.dat": true,
		"geoip.dat":   true,
	}
	count := 0
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(f.Name)
		if !wanted[name] {
			continue
		}
		// Refuse zip-slip attempts even though the source is GitHub —
		// belt and suspenders.
		dst := filepath.Join(destDir, name)
		if !filepathHasPrefix(dst, destDir) {
			return count, fmt.Errorf("zip entry %q escapes target dir", f.Name)
		}
		if err := extractZipEntry(f, dst); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func extractZipEntry(f *zip.File, dst string) error {
	in, err := f.Open()
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// filepathHasPrefix is a clean version of strings.HasPrefix that
// resolves both inputs through filepath.Clean to defeat "../" tricks.
func filepathHasPrefix(p, prefix string) bool {
	pAbs, err1 := filepath.Abs(p)
	prefAbs, err2 := filepath.Abs(prefix)
	if err1 != nil || err2 != nil {
		return false
	}
	if !filepath.IsAbs(pAbs) || !filepath.IsAbs(prefAbs) {
		return false
	}
	rel, err := filepath.Rel(prefAbs, pAbs)
	if err != nil {
		return false
	}
	return !startsWithDotDot(rel)
}

func startsWithDotDot(rel string) bool {
	return len(rel) >= 2 && rel[0] == '.' && rel[1] == '.'
}
