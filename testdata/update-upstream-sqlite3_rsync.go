// update-upstream-sqlite3_rsync keeps the pinned reference C source
// (testdata/sqlite3_rsync.c) current with the latest sqlite3_rsync
// from sqlite.org, and verifies the Go port against the compiled
// binary of that release.
//
// What it does, in order:
//  1. find the latest SQLite release on sqlite.org
//  2. download the sqlite-src zip and the sqlite-amalgamation zip,
//     verifying each against its SHA3-256 hash
//  3. compare the src zip's tool/sqlite3_rsync.c with the pinned
//     source: if they differ, copy the new source into testdata/ and
//     recommend the audit; if identical, report it
//  4. build the reference binary from the amalgamation and the
//     (possibly new) pinned source
//  5. run the differential suite against the built binary
//  6. only if the suite passed, move the version record in
//     testdata/sqlite3_rsync_version.json to the latest release
//
// A committed version means the Go code behaves identically to that
// release's compiled binary — the differential suite proved it. The
// audit itself — diffing the old and new C files, categorizing the
// changes (hash / wire / SQL / roles / CLI-only), refactoring the Go
// port and regenerating the golden vectors — is deliberately not
// automated: it is a judgment call. See testdata/README.md "Upstream
// sync" and the upstream-sync guide.
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
//	PRODUCT,3.53.4,2026/sqlite-amalgamation-3530400.zip,2713415,84f6a123...
//	 -->
//
// (the SHA3-HASH values are truncated here; the real ones are full
// 64-hex-digit hashes). The PRODUCT column is the constant word
// "PRODUCT" on every row, so a row's product is identified by its URL
// filename — sqlite-src-3530400.zip is the source zip, and
// sqlite-amalgamation-3530400.zip is the amalgamation the sync
// compiles into the reference binary. The comment format is documented
// as stable on the page itself.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha3"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// sqliteOrgURL is the URL prefix of every download-page row: the
	// rows list relative paths like 2026/sqlite-src-3530400.zip, so
	// the full URL is the prefix plus the row's path.
	sqliteOrgURL = "https://www.sqlite.org/"
	// downloadPage is the sqlite.org page whose embedded CSV comment
	// lists the current build products with their URLs and hashes.
	downloadPage = sqliteOrgURL + "download.html"
	// srcZipGlob matches the row the sync needs as the source of the
	// pinned C file (sqlite-src-3530400.zip).
	srcZipGlob = "sqlite-src-*.zip"
	// amalgZipGlob matches the row the sync needs to build the
	// reference binary (sqlite-amalgamation-3530400.zip). Both rows
	// always belong to the same release.
	amalgZipGlob = "sqlite-amalgamation-*.zip"
	// sourcePath is where the pinned C source lands, relative to the
	// repo root.
	sourcePath = "testdata/sqlite3_rsync.c"
	// versionPath is where the sync record lands, relative to the
	// repo root.
	versionPath = "testdata/sqlite3_rsync_version.json"
	// zipSourcePath is the fixed location of tool/sqlite3_rsync.c
	// inside the extracted source zip.
	zipSourcePath = "tool/sqlite3_rsync.c"
	// zipAmalgFilePath is the fixed location of sqlite3.c inside the
	// extracted amalgamation zip.
	zipAmalgFilePath = "sqlite3.c"
	// tmpDirPattern is the prefix pattern of the temp dir that holds
	// the zips; os.MkdirTemp replaces the * with a random suffix,
	// e.g. /tmp/sqlite-src-2518031401.
	tmpDirPattern = "sqlite-src-*"
	// srcExtractDir is the subdirectory of the temp dir the source
	// zip is extracted into, e.g. /tmp/sqlite-src-2518031401/src.
	srcExtractDir = "src"
	// amalgExtractDir is the subdirectory of the temp dir the
	// amalgamation zip is extracted into, e.g.
	// /tmp/sqlite-src-2518031401/amalgamation.
	amalgExtractDir = "amalgamation"
	// compiledBinaryName is the name of the reference binary the sync
	// builds in the temp dir, e.g.
	// /tmp/sqlite-src-2518031401/sqlite3_rsync.
	compiledBinaryName = "sqlite3_rsync"
)

// sqliteSrc is one download-page row the sync needs: either the
// source zip or the amalgamation zip, identified by its URL filename
// matching sqlite-src-*.zip or sqlite-amalgamation-*.zip.
type sqliteSrc struct {
	version string // human, e.g. "3.53.4"
	url     string // absolute, e.g. "https://www.sqlite.org/2026/sqlite-src-3530400.zip"
	sha3    string
}

// versionInfo is the shape of testdata/sqlite3_rsync_version.json, the
// record of the latest SQLite release the sync ran against. The pinned
// source is current as of that release — byte-identical to the
// release's tool/sqlite3_rsync.c. SyncDate is the day the sync ran,
// not the release date — the download page does not carry release
// dates for source zips.
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

	// Step 2: parse the sqlite-src and sqlite-amalgamation rows out
	// of it. If the page format changed, this is where the sync stops.
	src, amalgamation, err := parseSqliteZipFilenames(page)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("latest SQLite release: %s\n", src.version)

	// Step 3: download both zips into a temp dir, verifying each
	// against its SHA3 hash. Both are intermediates: their only job
	// is to give up the source and the amalgamation. They live in a
	// temp dir, never in the working tree, and are removed when the
	// sync finishes.
	tmp, err := os.MkdirTemp("", tmpDirPattern)
	if err != nil {
		fatal("create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmp)
	}()
	srcZipTmpPath := srcZipTmpPath(tmp, src.url)
	err = downloadAndVerify(src.url, src.sha3, srcZipTmpPath)
	if err != nil {
		fatal("%v", err)
	}
	amalgZipTmpPath := amalgZipTmpPath(tmp, amalgamation.url)
	err = downloadAndVerify(amalgamation.url, amalgamation.sha3, amalgZipTmpPath)
	if err != nil {
		fatal("%v", err)
	}

	// Step 4: extract the source zip and read tool/sqlite3_rsync.c
	// from its fixed location (zipSourcePath), then compare it with
	// the pinned one. Identical or not, the sync continues — the
	// reference binary is always built and tested.
	// srcExtractDir is the temp dir plus the "src" subdirectory, e.g.
	// /tmp/sqlite-src-2518031401/src.
	srcExtractDir := filepath.Join(tmp, srcExtractDir)
	err = extractZip(srcZipTmpPath, srcExtractDir)
	if err != nil {
		fatal("%v", err)
	}
	newSource, err := os.ReadFile(filepath.Join(srcExtractDir, zipSourcePath))
	if err != nil {
		fatal("%v", err)
	}
	sourceChanged, err := compareSource(newSource)
	if err != nil {
		fatal("%v", err)
	}
	if sourceChanged {
		// The pinned source differs: copy the new one so the audit
		// can diff it against the previous pin, and recommend the
		// audit.
		err = os.WriteFile(sourcePath, newSource, 0o644)
		if err != nil {
			fatal("write %s: %v", sourcePath, err)
		}
		fmt.Printf("updated %s\n", sourcePath)
		suggestAudit()
	} else {
		fmt.Println("tool/sqlite3_rsync.c is byte-identical to the pinned source")
	}

	// Step 5: build the reference binary from the amalgamation and
	// the pinned source — after a change, the pin already holds the
	// new source.
	// amalgExtractDir is the temp dir plus the "amalgamation"
	// subdirectory, e.g. /tmp/sqlite-src-2518031401/amalgamation.
	amalgExtractDir := filepath.Join(tmp, amalgExtractDir)
	err = extractZip(amalgZipTmpPath, amalgExtractDir)
	if err != nil {
		fatal("%v", err)
	}
	sqlite3File := filepath.Join(amalgExtractDir, zipAmalgFilePath)
	compiledBinaryPath := filepath.Join(tmp, compiledBinaryName)
	err = compile(sourcePath, sqlite3File, amalgExtractDir, compiledBinaryPath)
	if err != nil {
		fatal("%v", err)
	}

	// Step 6: run the differential suite against the built binary.
	// The suite decides whether the Go port behaves like the binary.
	err = runDifferentialSuite(compiledBinaryPath)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Println("differential suite passed")

	// Step 7: only a passing differential suite moves the version
	// record: the committed version means the Go code behaves
	// identically to that release's binary.
	updated, err := writeVersionJSON(versionPath, src)
	if err != nil {
		fatal("%v", err)
	}
	if sourceChanged {
		reportCreatedOrUpdatedFiles(sourcePath, versionPath)
	} else if updated {
		reportCreatedOrUpdatedFiles(versionPath)
	} else {
		fmt.Printf("%s already current\n", versionPath)
	}
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

// parseSqliteZipFilenames extracts the two download-page rows the sync needs
// (see the structure in the package comment): the source zip and the
// amalgamation zip, both of the same release. The url field of each
// row is absolute. If the page format changed, this is where the sync
// stops.
func parseSqliteZipFilenames(page string) (src sqliteSrc, amalgamation sqliteSrc, err error) {
	const header = "PRODUCT,VERSION,RELATIVE-URL,SIZE-IN-BYTES,SHA3-HASH"
	_, rows, ok := strings.Cut(page, header)
	if !ok {
		return sqliteSrc{}, sqliteSrc{}, fmt.Errorf("download page format changed: product data header not found")
	}
	for _, line := range strings.Split(rows, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "-->" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 5 {
			continue
		}
		if matchesGlob(path.Base(fields[2]), srcZipGlob) {
			src = sqliteSrc{version: fields[1], url: sqliteOrgURL + fields[2], sha3: fields[4]}
		} else if matchesGlob(path.Base(fields[2]), amalgZipGlob) {
			amalgamation = sqliteSrc{version: fields[1], url: sqliteOrgURL + fields[2], sha3: fields[4]}
		}
		if src.version != "" && amalgamation.version != "" {
			return src, amalgamation, nil
		}
	}
	missing := amalgZipGlob
	if src.version == "" {
		missing = srcZipGlob
	}
	return sqliteSrc{}, sqliteSrc{}, fmt.Errorf("download page format changed: no %s row", missing)
}

// compareSource reports whether the pinned source differs from
// newSource. A missing pin (first sync) counts as different.
func compareSource(newSource []byte) (changed bool, err error) {
	pinned, err := os.ReadFile(sourcePath)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %v", sourcePath, err)
	}
	return !bytes.Equal(newSource, pinned), nil
}

// matchesGlob reports whether name matches the sqlite-src-*.zip shape:
// a fixed prefix, anything, and a fixed suffix.
func matchesGlob(name, glob string) bool {
	prefix, suffix := glob[:strings.Index(glob, "*")], glob[strings.Index(glob, "*")+1:]
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)
}

// downloadAndVerify fetches the zip at url into dest, verifying its
// SHA3-256 hash against wantSHA3. It prints the download as it
// starts. An empty wantSHA3 skips the verification with the caller
// having printed a warning.
func downloadAndVerify(url, wantSHA3, dest string) (err error) {
	printInfo("downloading %s", path.Base(url))
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

// extractZip unzips every entry of zipPath into dir, dropping the
// zip's single version-named top directory so the files land
// directly in dir. It prints the zip as it starts extracting.
func extractZip(zipPath, dir string) error {
	printInfo("extracting %s", path.Base(zipPath))
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open %s: %v", zipPath, err)
	}
	defer func() {
		err = errors.Join(err, r.Close())
	}()
	// The sqlite zips wrap their files in one version-named directory
	// (sqlite-src-3530400/); cut it off every entry so the files land
	// directly in dir.
	for _, f := range r.File {
		_, rest, ok := strings.Cut(f.Name, "/")
		if !ok || rest == "" {
			continue // the version directory entry itself
		}
		dest := filepath.Join(dir, rest)
		if f.FileInfo().IsDir() {
			err = os.MkdirAll(dest, 0o755)
			if err != nil {
				return fmt.Errorf("mkdir %s: %v", dest, err)
			}
			continue
		}
		err = os.MkdirAll(filepath.Dir(dest), 0o755)
		if err != nil {
			return fmt.Errorf("mkdir %s: %v", filepath.Dir(dest), err)
		}
		rc, openErr := f.Open()
		if openErr != nil {
			return fmt.Errorf("open %s: %v", f.Name, openErr)
		}
		data, readErr := io.ReadAll(rc)
		closeErr := rc.Close()
		if readErr != nil {
			return fmt.Errorf("read %s: %v", f.Name, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %v", f.Name, closeErr)
		}
		err = os.WriteFile(dest, data, 0o644)
		if err != nil {
			return fmt.Errorf("write %s: %v", dest, err)
		}
	}
	return nil
}

// srcZipTmpPath is where the source zip at url lands in the temp dir:
// the temp dir plus the zip's filename, e.g.
// /tmp/sqlite-src-2518031401/sqlite-src-3530400.zip.
func srcZipTmpPath(tmp, url string) string {
	return filepath.Join(tmp, path.Base(url))
}

// amalgZipTmpPath is where the amalgamation zip at url lands in the
// temp dir, e.g.
// /tmp/sqlite-src-2518031401/sqlite-amalgamation-3530400.zip.
func amalgZipTmpPath(tmp, url string) string {
	return filepath.Join(tmp, path.Base(url))
}

// compile builds the reference sqlite3_rsync binary from the pinned
// source and the amalgamation, with the same flags the authors' build
// uses: main.mk's sqlite3_rsync target compiles with $(RSYNC_OPT)
// (SQLITE_ENABLE_DBPAGE_VTAB and friends — without DBPAGE_VTAB the
// binary cannot serve the protocol's sqlite_dbpage queries):
//
//	cc -O1 -I amalgDir -DSQLITE_ENABLE_DBPAGE_VTAB -USQLITE_THREADSAFE -DSQLITE_THREADSAFE=0 -DSQLITE_OMIT_LOAD_EXTENSION -DSQLITE_OMIT_DEPRECATED source sqlite3File -o out
//
// It prints the destination binary as the compile starts.
func compile(source, sqlite3File, amalgDir, out string) error {
	printInfo("compiling %s", out)
	cmd := exec.Command("cc", "-O1", "-I", amalgDir,
		"-DSQLITE_ENABLE_DBPAGE_VTAB",
		"-USQLITE_THREADSAFE",
		"-DSQLITE_THREADSAFE=0",
		"-DSQLITE_OMIT_LOAD_EXTENSION",
		"-DSQLITE_OMIT_DEPRECATED",
		source, sqlite3File, "-o", out)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("compile %s: %v\n%s", out, err, output)
	}
	return nil
}

// runDifferentialSuite runs the differential suite against the
// reference binary at bin, from the repo root:
//
//	SQLITE3_RSYNC_BIN=bin go test -tags differential ./sqlitersync/
//
// It prints the suite as it starts. A failing suite means the Go port
// does not behave like the release's binary — the caller must not move
// the version record.
func runDifferentialSuite(bin string) error {
	printInfo("running the differential suite")
	cmd := exec.Command("go", "test", "-tags", "differential", "./sqlitersync/")
	cmd.Env = append(os.Environ(), "SQLITE3_RSYNC_BIN="+bin)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("differential suite failed against %s: %v\n%s", bin, err, output)
	}
	return nil
}

// writeVersionJSON records the latest SQLite release in dest, unless
// dest already holds exactly that record — then it writes nothing. It
// reports whether it wrote, so the caller can say which happened. The
// record and the pinned source stay in step: both come from the same
// download in the same run.
func writeVersionJSON(dest string, src sqliteSrc) (updated bool, err error) {
	info := versionInfo{
		SQLiteVersion: src.version,
		URL:           src.url,
		SHA3:          src.sha3,
		SyncDate:      time.Now().Format("2006-01-02"),
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	existing, err := os.ReadFile(dest)
	if err == nil && bytes.Equal(existing, data) {
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read %s: %v", dest, err)
	}
	err = os.WriteFile(dest, data, 0o644)
	if err != nil {
		return false, fmt.Errorf("write %s: %v", dest, err)
	}
	return true, nil
}

// reportCreatedOrUpdatedFiles prints the files the sync created or
// updated.
func reportCreatedOrUpdatedFiles(files ...string) {
	fmt.Println("created/updated:")
	for _, f := range files {
		fmt.Printf("  %s\n", f)
	}
}

// suggestAudit prints the steps that follow a changed pinned source:
// the audit that decides whether the Go port needs to change. The
// differential suite has already run and passed — the audit covers
// what the suite cannot see: the categorization of the changes and
// the line-number references in the port's comments.
func suggestAudit() {
	fmt.Println()
	fmt.Println("next: audit the new source before committing")
	fmt.Println("  1. diff testdata/sqlite3_rsync.c against the previous one")
	fmt.Println("  2. categorize the changes: hash / wire / SQL / roles / CLI-only")
	fmt.Println("  3. where a category requires it, refactor the Go port and re-check line-number references")
	fmt.Println("  4. only if the hash algorithm changed: go run ./testdata/generate-hash-golden-vectors.go")
}

// printInfo prints a line about what the sync is doing as it happens,
// e.g. "downloading sqlite-src-3530400.zip".
func printInfo(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

// fatal prints a message to stderr and exits with status 1.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
