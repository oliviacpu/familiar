package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVersions(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.10", "0.2.9", true}, {"0.2.9", "0.2.10", false}, {"v0.3.0", "0.2.99", true},
		{"v0.2.9", "0.2.9", false}, {"v0.2.9", "dev", false}, {"nightly", "0.2.9", false}, {"v1.0", "0.2.9", false},
	}
	for _, c := range cases {
		if got := newer(c.latest, c.current); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.latest, c.current, got)
		}
	}
	if assetName("darwin", "arm64") != "familiar-macos" || assetName("windows", "amd64") != "familiar-windows.exe" || assetName("linux", "arm64") != "familiar-linux-arm64" {
		t.Error("asset names")
	}
}

// fakeReleases serves a release the way GitHub does, signed with key.
func fakeReleases(t *testing.T, key ed25519.PrivateKey, tag string, bin []byte, tamper bool) *httptest.Server {
	sum := sha256.Sum256(bin)
	name := assetName("linux", "amd64")
	sums := []byte(fmt.Sprintf("%s  %s\n%s  familiar-macos\n", hex.EncodeToString(sum[:]), name, hex.EncodeToString(make([]byte, 32))))
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, sums))
	served := bin
	if tamper {
		served = append([]byte("evil"), bin...)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []map[string]string{
				{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/sums"},
				{"name": "SHA256SUMS.sig", "browser_download_url": srv.URL + "/sig"},
				{"name": name, "browser_download_url": srv.URL + "/bin"},
			}})
		case "/sums":
			w.Write(sums)
		case "/sig":
			w.Write([]byte(sig + "\n"))
		case "/bin":
			w.Write(served)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchUpdate(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bin := []byte("new familiar build")
	u := func(srv *httptest.Server, key ed25519.PublicKey) *updater {
		x := newUpdater()
		x.url, x.key, x.current = srv.URL+"/latest", key, "0.2.1"
		return x
	}
	ctx := context.Background()

	v, got, err := u(fakeReleases(t, priv, "v0.2.2", bin, false), pub).fetch(ctx, "linux", "amd64")
	if err != nil || v != "0.2.2" || string(got) != string(bin) {
		t.Fatalf("good release: %q %q %v", v, got, err)
	}
	if _, got, err := u(fakeReleases(t, priv, "v0.2.1", bin, false), pub).fetch(ctx, "linux", "amd64"); err != nil || got != nil {
		t.Errorf("same version should do nothing: %v %v", got, err)
	}
	if _, _, err := u(fakeReleases(t, priv, "v0.2.2", bin, true), pub).fetch(ctx, "linux", "amd64"); err == nil {
		t.Error("a binary that doesn't match its checksum was accepted")
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := u(fakeReleases(t, priv, "v0.2.2", bin, false), otherPub).fetch(ctx, "linux", "amd64"); err == nil {
		t.Error("SHA256SUMS signed by another key was accepted")
	}
	if _, _, err := u(fakeReleases(t, priv, "v0.2.2", bin, false), pub).fetch(ctx, "windows", "amd64"); err == nil {
		t.Error("a release without this machine's build was accepted")
	}
}
