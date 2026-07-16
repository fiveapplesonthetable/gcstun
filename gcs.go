package main

// Minimal Google Cloud Storage client (stdlib only): service-account JWT -> OAuth
// token, then object put/get/getRange/list/delete over the JSON+download API. Reads
// from the RU box go to storage.googleapis.com, which TSPU whitelists (~24 MB/s), so
// the tunnel never touches a denylisted destination.

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type saKey struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
	ProjectID   string `json:"project_id"`
}

type GCS struct {
	bucket string
	key    saKey
	pk     *rsa.PrivateKey
	hc     *http.Client

	mu    sync.Mutex
	tok   string
	tokExp time.Time
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func NewGCS(keyJSON []byte, bucket string) (*GCS, error) {
	var k saKey
	if err := json.Unmarshal(keyJSON, &k); err != nil {
		return nil, err
	}
	block, _ := pem.Decode([]byte(k.PrivateKey))
	if block == nil {
		return nil, fmt.Errorf("bad private key PEM")
	}
	pkAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pk, ok := pkAny.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not RSA key")
	}
	// keep-alive transport, but FORCE HTTP/1.1: HTTP/2 would multiplex all our concurrent
	// reads onto one connection and serialize them (~5 MB/s). With HTTP/1.1 each parallel
	// read gets its own connection, so throughput scales toward the raw ~24 MB/s.
	tr := &http.Transport{
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // disable HTTP/2
	}
	return &GCS{bucket: bucket, key: k, pk: pk, hc: &http.Client{Transport: tr}}, nil
}

func (g *GCS) token() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tok != "" && time.Now().Before(g.tokExp.Add(-60*time.Second)) {
		return g.tok, nil
	}
	now := time.Now().Unix()
	hdr := b64url([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claim := b64url([]byte(fmt.Sprintf(
		`{"iss":"%s","scope":"https://www.googleapis.com/auth/devstorage.read_write","aud":"%s","iat":%d,"exp":%d}`,
		g.key.ClientEmail, g.key.TokenURI, now, now+3600)))
	signingInput := hdr + "." + claim
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.pk, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	jwt := signingInput + "." + b64url(sig)
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {jwt}}
	resp, err := g.hc.Post(g.key.TokenURI, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("no access token")
	}
	g.tok = out.AccessToken
	g.tokExp = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return g.tok, nil
}

func (g *GCS) auth(req *http.Request) error {
	t, err := g.token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t)
	return nil
}

// Put writes an object (simple media upload).
func (g *GCS) Put(obj string, data []byte) error {
	u := fmt.Sprintf("https://storage.googleapis.com/upload/storage/v1/b/%s/o?uploadType=media&name=%s",
		g.bucket, url.QueryEscape(obj))
	req, _ := http.NewRequest("POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	if err := g.auth(req); err != nil {
		return err
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("put %s: %d", obj, resp.StatusCode)
	}
	return nil
}

// Get returns the object bytes, or (nil, 404err) if it doesn't exist yet.
func (g *GCS) Get(obj string) ([]byte, int, error) {
	u := fmt.Sprintf("https://storage.googleapis.com/%s/%s", g.bucket, pathEscape(obj))
	req, _ := http.NewRequest("GET", u, nil)
	if err := g.auth(req); err != nil {
		return nil, 0, err
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// List returns object names under prefix (single page, up to maxResults).
func (g *GCS) List(prefix string, maxResults int) ([]string, error) {
	u := fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o?prefix=%s&maxResults=%d",
		g.bucket, url.QueryEscape(prefix), maxResults)
	req, _ := http.NewRequest("GET", u, nil)
	if err := g.auth(req); err != nil {
		return nil, err
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, len(out.Items))
	for i, it := range out.Items {
		names[i] = it.Name
	}
	return names, nil
}

func (g *GCS) Delete(obj string) error {
	u := fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o/%s", g.bucket, url.QueryEscape(obj))
	req, _ := http.NewRequest("DELETE", u, nil)
	if err := g.auth(req); err != nil {
		return err
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return nil
}

// pathEscape escapes an object name for the download path (keep '/' readable).
func pathEscape(obj string) string {
	parts := strings.Split(obj, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

var _ = strconv.Itoa
