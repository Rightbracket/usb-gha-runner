package gh

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
)

func TestAppJWT(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	now := time.Unix(1_800_000_000, 0)
	tok, err := AppJWT("123", pemKey, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("bad jwt %q", tok)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	json.Unmarshal(cb, &claims)
	if claims["iss"] != "123" || int64(claims["exp"].(float64))-now.Unix() > 600 {
		t.Errorf("claims %v", claims)
	}
	// PKCS#8 keys work too.
	p8, _ := x509.MarshalPKCS8PrivateKey(key)
	if _, err := AppJWT("1", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8})), now); err != nil {
		t.Errorf("pkcs8: %v", err)
	}
}

// TestJITFlow drives the client against a fake GitHub API: app token,
// runner-group lookup and JIT config generation for an org.
func TestJITFlow(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var gotJIT map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/app/installations/9/access_tokens":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				t.Errorf("app token request without JWT")
			}
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour)})
		case r.URL.Path == "/api/v3/orgs/acme/actions/runner-groups":
			json.NewEncoder(w).Encode(map[string]any{"runner_groups": []map[string]any{{"id": 1, "name": "Default"}, {"id": 7, "name": "ci"}}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/orgs/acme/actions/runners/generate-jitconfig":
			if r.Header.Get("Authorization") != "Bearer ghs_x" {
				t.Errorf("jit without installation token")
			}
			json.NewDecoder(r.Body).Decode(&gotJIT)
			json.NewEncoder(w).Encode(map[string]any{"runner": map[string]any{"id": 42, "name": gotJIT["name"]}, "encoded_jit_config": "ENC"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	cfg := &config.Config{URL: "http://" + host + "/acme", RunnerGroup: "ci", Auth: config.Auth{Type: "app", AppID: "1", InstallationID: "9", PrivateKey: pemKey}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	enc, r, err := c.JITConfig(context.Background(), "ghr-3f9a-1", []string{"self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if enc != "ENC" || r.ID != 42 || gotJIT["runner_group_id"].(float64) != 7 || gotJIT["name"] != "ghr-3f9a-1" {
		t.Errorf("enc=%s runner=%+v req=%v", enc, r, gotJIT)
	}
}
