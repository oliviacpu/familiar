package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// familiar keeps itself up to date from this repository's GitHub releases.
// A release carries a SHA256SUMS file signed with the release key; familiar
// only installs a build whose checksum is in a file that key signed.

const releasesURL = "https://api.github.com/repos/oliviacpu/familiar/releases/latest"

// releaseKey verifies SHA256SUMS.sig. Its private half is the repository's
// FAMILIAR_SIGNING_KEY secret.
var releaseKey = mustKey("zi7z58gHe05H4G2RWG7qLFT3rLDmlUtT+6iyl9zHGxs=")

const (
	updateEvery = 6 * time.Hour
	updateFirst = 2 * time.Minute // after starting, so a broken build can't loop fast
	maxBinary   = 128 << 20

	exitRestart = 3 // "start me again": the Mac LaunchAgent restarts after an unsuccessful exit
)

func mustKey(b64 string) ed25519.PublicKey {
	k, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(k) != ed25519.PublicKeySize {
		return nil
	}
	return k
}

// assetName is the release file for this machine.
func assetName(goos, goarch string) string {
	switch goos {
	case "darwin":
		return "familiar-macos" // universal: Apple silicon and Intel
	case "windows":
		return "familiar-windows.exe"
	}
	return "familiar-" + goos + "-" + goarch
}

// parseVersion reads "0.2.15" or "v0.2.15". Builds without a release version
// ("dev") never update.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func newer(latest, current string) bool {
	l, ok1 := parseVersion(latest)
	c, ok2 := parseVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

type release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r release) asset(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

type updater struct {
	http    *http.Client
	url     string
	key     ed25519.PublicKey
	next    time.Time
	current string
}

func newUpdater() *updater {
	return &updater{http: &http.Client{Timeout: 5 * time.Minute}, url: releasesURL, key: releaseKey,
		next: time.Now().Add(updateFirst), current: version}
}

func (u *updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "familiar/"+u.current)
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("download too large")
	}
	return data, err
}

// fetch returns the newer build for this machine, checked, or nil when this
// one is current.
func (u *updater) fetch(ctx context.Context, goos, goarch string) (string, []byte, error) {
	if u.key == nil {
		return "", nil, errors.New("this build has no release key")
	}
	data, err := u.get(ctx, u.url, 1<<20)
	if err != nil {
		return "", nil, err
	}
	var rel release
	if err := json.Unmarshal(data, &rel); err != nil {
		return "", nil, fmt.Errorf("reading the release: %w", err)
	}
	if !newer(rel.Tag, u.current) {
		return "", nil, nil
	}
	name := assetName(goos, goarch)
	sumsURL, sigURL, binURL := rel.asset("SHA256SUMS"), rel.asset("SHA256SUMS.sig"), rel.asset(name)
	if sumsURL == "" || sigURL == "" || binURL == "" {
		return "", nil, fmt.Errorf("release %s has no %s, SHA256SUMS or signature", rel.Tag, name)
	}
	sums, err := u.get(ctx, sumsURL, 1<<16)
	if err != nil {
		return "", nil, err
	}
	sig, err := u.get(ctx, sigURL, 1<<10)
	if err != nil {
		return "", nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(u.key, sums, raw) {
		return "", nil, fmt.Errorf("release %s: SHA256SUMS isn't signed by the release key", rel.Tag)
	}
	want := checksumFor(sums, name)
	if want == "" {
		return "", nil, fmt.Errorf("release %s: %s isn't in SHA256SUMS", rel.Tag, name)
	}
	bin, err := u.get(ctx, binURL, maxBinary)
	if err != nil {
		return "", nil, err
	}
	if got := sha256.Sum256(bin); hex.EncodeToString(got[:]) != want {
		return "", nil, fmt.Errorf("release %s: %s doesn't match its checksum", rel.Tag, name)
	}
	return strings.TrimPrefix(rel.Tag, "v"), bin, nil
}

// checksumFor finds name's line in a sha256sum file ("<hex>  <name>").
func checksumFor(sums []byte, name string) string {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// maybeUpdate checks for a new release when one is due, and if there is one
// installs it and restarts into it. It returns only when there's nothing to do
// or the update failed.
func (u *updater) maybeUpdate(ctx context.Context) {
	if time.Now().Before(u.next) {
		return
	}
	u.next = time.Now().Add(updateEvery)
	if !canReplaceSelf() {
		return
	}
	v, bin, err := u.fetch(ctx, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		log.Printf("update check: %v", err)
		return
	}
	if bin == nil {
		return
	}
	log.Printf("updating from %s to %s", u.current, v)
	if err := replaceSelfAndRestart(bin); err != nil {
		log.Printf("update to %s failed: %v", v, err)
	}
}
