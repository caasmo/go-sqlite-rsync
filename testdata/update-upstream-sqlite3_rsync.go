// update-upstream-sqlite3_rsync is the deterministic front half of the
// upstream-sync procedure: it brings the pinned reference C source up
// to date with the latest sqlite3_rsync from sqlite.org.
//
// What it does, in order:
//  1. find the latest SQLite release on sqlite.org
//  2. download the sqlite-src zip and verify its SHA3-256 hash
//  3. copy tool/sqlite3_rsync.c from the zip into testdata/
//  4. record the version in testdata/sqlite3_rsync_version.json
//  5. report every file it created or updated
//  6. print the audit steps that follow
//
// The audit itself — diffing the old and new C files, categorizing the
// changes (hash / wire / SQL / roles / CLI-only), refactoring the Go
// port and regenerating the golden vectors — is deliberately not
// automated: it is a judgment call. See testdata/README.md
// "Upstream sync" and the upstream-sync guide.
//
// Run from the repo root: go run ./testdata/update-upstream-sqlite3_rsync.go
//
// # The download page
//
// The nucleus of the tool is the machine-readable data that sqlite.org
// embeds in https://sqlite.org/download.html as an HTML comment. The
// comment reads:
//
//	<!-- Download product data for scripts to read
//	PRODUCT,VERSION,RELATIVE-URL,SIZE-IN-BYTES,SHA3-HASH
//	PRODUCT,2026-07-31 22:45 UTC,snapshot/sqlite-snapshot-202607312245.tar.gz,3312178,f7487e5c...
//	PRODUCT,3.53.4,2026/sqlite-src-3530400.zip,14557315,b834d474...
//	 -->
//
// (the SHA3-HASH values are truncated here; the real ones are full
// 64-hex-digit hashes). The PRODUCT column is the constant word
// "PRODUCT" on every row, so a row's product is identified by its URL
// filename — sqlite-src-3530400.zip is the source zip. The comment
// format is documented as stable on the page itself.
package main

import (
	"archive/zip"
	"crypto/sha3"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// downloadPage is the sqlite.org page whose embedded CSV comment
	// lists the current build products with their URLs and hashes.
	downloadPage = "https://www.sqlite.org/download.html"
	// srcZipGlob matches the one row the sync needs: the source zip
	// containing tool/sqlite3_rsync.c (sqlite-src-3530400.zip).
	srcZipGlob = "sqlite-src-*.zip"
	// sourcePath is where the pinned C source lands, relative to the
	// repo root.
	sourcePath = "testdata/sqlite3_rsync.c"
	// versionPath is where the sync record lands, relative to the
	// repo root.
	versionPath = "testdata/sqlite3_rsync_version.json"
)

// sqliteSrc is the one download-page row the sync needs: the source
// zip, identified by its URL filename matching sqlite-src-*.zip.
type sqliteSrc struct {
	version string // human, e.g. "3.53.4"
	url     string // relative, e.g. "2026/sqlite-src-3530400.zip"
	sha3    string
}

// versionInfo is the shape of testdata/sqlite3_rsync_version.json, the
// record of which SQLite release the pinned C source came from.
// SyncDate is the day the sync ran, not the release date — the
// download page does not carry release dates for source zips.
type versionInfo struct {
	SQLiteVersion string `json:"sqlite_version"` // e.g. "3.53.4"
	URL           string `json:"url"`
	SHA3          string `json:"sha3"`
	SyncDate      string `json:"sync_date"` // the day the sync ran
}

func main() {
	// Step 1: download the download page.
	page, err := fetchPage()
	if err != nil {
		fatal("%v", err)
	}
	// Step 2: parse the sqlite-src row out of it. If the page format
	// changed, this is where the sync stops.
	src, err := parseSQLiteSrc(page)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("latest SQLite release: %s\n", src.version)
	// Step 3: download the source zip into a temp dir and verify it.
	tmp, err := os.MkdirTemp("", "sqlite-src-*")
	if err != nil {
		fatal("create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmp)
	}()
	// The source zip is an intermediate: its only job is to give up
	// tool/sqlite3_rsync.c. It lives in a temp dir, never in the
	// working tree, and is removed when the sync finishes.
	zipPath := filepath.Join(tmp, path.Base(src.url))
	err = download(fullURL(src.url), src.sha3, zipPath)
	if err != nil {
		fatal("%v", err)
	}
	// Step 4: extract the pinned C source into testdata/.
	err = extractSource(zipPath, sourcePath)
	if err != nil {
		fatal("%v", err)
	}
	// Step 5: record which release the pinned source came from.
	err = writeVersionJSON(versionPath, src)
	if err != nil {
		fatal("%v", err)
	}
	// Step 6: report and point at the audit.
	report(sourcePath, versionPath)
	suggestAudit(src)
}

// fetchPage downloads the sqlite.org download page as text.
func fetchPage() (page string, err error) {
	resp, err := http.Get(downloadPage)
	if err != nil {
		return "", fmt.Errorf("GET %s: %v", downloadPage, err)
	}
	defer func() {
		err = errors.Join(err, resp.Body.Close())
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %s", downloadPage, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read %s: %v", downloadPage, err)
	}
	return string(data), nil
}

// parseSQLiteSrc extracts the sqlite-src row from the download page
// (see the structure in the package comment) by locating the header
// line — the rows follow it directly — and picking the row whose URL
// filename matches sqlite-src-*.zip.
func parseSQLiteSrc(page string) (sqliteSrc, error) {
	const header = "PRODUCT,VERSION,RELATIVE-URL,SIZE-IN-BYTES,SHA3-HASH"
	idx := strings.Index(page, header)
	if idx < 0 {
		return sqliteSrc{}, fmt.Errorf("download page format changed: product data header not found")
	}
	for _, line := range strings.Split(page[idx+len(header):], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "-->" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 5 {
			continue
		}
		if !matchesGlob(path.Base(fields[2]), srcZipGlob) {
			continue
		}
		return sqliteSrc{version: fields[1], url: fields[2], sha3: fields[4]}, nil
	}
	return sqliteSrc{}, fmt.Errorf("download page format changed: no %s row", srcZipGlob)
}

// matchesGlob reports whether name matches the sqlite-src-*.zip shape:
// a fixed prefix, anything, and a fixed suffix.
func matchesGlob(name, glob string) bool {
	prefix, suffix := glob[:strings.Index(glob, "*")], glob[strings.Index(glob, "*")+1:]
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)
}

// fullURL turns a relative URL from the product CSV (for example
// "2026/sqlite-src-3530400.zip") into an absolute one.
func fullURL(relative string) string {
	return "https://www.sqlite.org/" + strings.TrimPrefix(relative, "/")
}

// download fetches the zip at url into dest, verifying its SHA3-256
// hash against wantSHA3. An empty wantSHA3 skips the verification with
// the caller having printed a warning.
func download(url, wantSHA3, dest string) (err error) {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %v", url, err)
	}
	defer func() {
		err = errors.Join(err, resp.Body.Close())
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %s", url, resp.Status)
	}
	file, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create %s: %v", dest, err)
	}
	hash := sha3.New256()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("download %s: %v", url, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %v", dest, closeErr)
	}
	if wantSHA3 != "" {
		got := hex.EncodeToString(hash.Sum(nil))
		if !strings.EqualFold(got, wantSHA3) {
			return fmt.Errorf("download %s: SHA3-256 mismatch: got %s, want %s", url, got, wantSHA3)
		}
	}
	return nil
}

// extractSource copies tool/sqlite3_rsync.c out of the source zip into
// dest, byte for byte.
func extractSource(zipPath, dest string) (err error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open %s: %v", zipPath, err)
	}
	defer func() {
		err = errors.Join(err, r.Close())
	}()
	for _, f := range r.File {
		if !strings.HasSuffix(f.Name, "tool/sqlite3_rsync.c") {
			continue
		}
		return writeZipEntry(f, dest)
	}
	return fmt.Errorf("no tool/sqlite3_rsync.c in %s", zipPath)
}

// writeZipEntry writes one zip file's content to dest.
func writeZipEntry(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open %s: %v", f.Name, err)
	}
	file, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create %s: %v", dest, err)
	}
	_, copyErr := io.Copy(file, rc)
	closeFileErr := file.Close()
	closeRCErr := rc.Close()
	if copyErr != nil {
		return fmt.Errorf("extract %s: %v", f.Name, copyErr)
	}
	if closeFileErr != nil {
		return fmt.Errorf("close %s: %v", dest, closeFileErr)
	}
	return closeRCErr
}

// writeVersionJSON records which SQLite release the sync produced, so
// the repo always states what it ports.
func writeVersionJSON(dest string, src sqliteSrc) error {
	info := versionInfo{
		SQLiteVersion: src.version,
		URL:           fullURL(src.url),
		SHA3:          src.sha3,
		SyncDate:      time.Now().Format("2006-01-02"),
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	err = os.WriteFile(dest, data, 0o644)
	if err != nil {
		return fmt.Errorf("write %s: %v", dest, err)
	}
	return nil
}

// report prints the files the sync created or updated.
func report(files ...string) {
	fmt.Println("created/updated:")
	for _, f := range files {
		fmt.Printf("  %s\n", f)
	}
}

// suggestAudit prints the steps that follow the sync: the audit that
// decides whether the Go port needs to change. The reference-binary
// steps are conditional — they only apply when the audit found changes.
func suggestAudit(src sqliteSrc) {
	toolsZip := strings.Replace(src.url, "sqlite-src-", "sqlite-tools-linux-x64-", 1)
	toolsDir := strings.TrimSuffix(path.Base(toolsZip), ".zip")
	fmt.Println()
	fmt.Println("next: audit the new source before committing")
	fmt.Println("  1. diff testdata/sqlite3_rsync.c against the previous one")
	fmt.Println("  2. categorize the changes: hash / wire / SQL / roles / CLI-only")
	fmt.Println("  3. where a category requires it, refactor the Go port and re-check line-number references")
	fmt.Println("  4. only if the hash algorithm changed: go run ./testdata/generate-hash-golden-vectors.go")
	fmt.Println("  5. if the audit found changes: download " + toolsZip + " from the download page and extract it into references/")
	fmt.Println("  6. then run the differential gate: SQLITE3_RSYNC_BIN=$PWD/references/" + toolsDir + "/sqlite3_rsync go test -tags differential ./sqlitersync/")
}

// fatal prints a message to stderr and exits with status 1.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
