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
	"sort"
	"strconv"
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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	host := strings.TrimPrefix(srv.URL, "https://")
	cfg := &config.Config{URL: "https://" + host + "/acme", RunnerGroup: "ci", Auth: config.Auth{Type: "app", AppID: "1", InstallationID: "9", PrivateKey: pemKey}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = srv.Client().Transport
	enc, r, err := c.JITConfig(context.Background(), "ghr-3f9a-1", []string{"self-hosted"})
	if err != nil {
		t.Fatal(err)
	}
	if enc != "ENC" || r.ID != 42 || gotJIT["runner_group_id"].(float64) != 7 || gotJIT["name"] != "ghr-3f9a-1" {
		t.Errorf("enc=%s runner=%+v req=%v", enc, r, gotJIT)
	}
}

func TestDeleteSlotRunnersExactNames(t *testing.T) {
	var deleted []string
	names := map[int64]string{1: "build-ghr-3f9a-1", 2: "build-ghr-3f9a-12", 3: "build-ghr-3f9a-box", 4: "build-ghr-3f9a2-1", 5: "other-1"}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/repos/acme/app/actions/runners":
			var rs []map[string]any
			for id, n := range names {
				rs = append(rs, map[string]any{"id": id, "name": n})
			}
			json.NewEncoder(w).Encode(map[string]any{"runners": rs})
		case r.Method == "DELETE":
			id, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
			deleted = append(deleted, names[id])
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(&config.Config{URL: srv.URL + "/acme/app", Auth: config.Auth{Type: "pat", PAT: "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = srv.Client().Transport
	n, err := c.DeleteSlotRunners(context.Background(), "build-ghr-3f9a")
	sort.Strings(deleted)
	if err != nil || n != 2 || strings.Join(deleted, ",") != "build-ghr-3f9a-1,build-ghr-3f9a-12" {
		t.Fatalf("deleted %v (n=%d, err=%v)", deleted, n, err)
	}
}

func TestNoCredentialOnCrossHostRedirect(t *testing.T) {
	leaked := make(chan string, 1)
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked <- r.Header.Get("Authorization")
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, _ := New(&config.Config{URL: srv.URL + "/acme/app", Auth: config.Auth{Type: "pat", PAT: "tok"}})
	c.http.Transport = srv.Client().Transport
	if err := c.Check(context.Background()); err == nil {
		t.Fatal("redirect to another host was followed")
	}
	select {
	case h := <-leaked:
		t.Fatalf("other host got a request (Authorization %q)", h)
	default:
	}
}

func TestDeleteRunnerNotFooledByTokenOr301(t *testing.T) {
	// A 404 from the App token request is not "already deleted".
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	c, err := New(&config.Config{URL: srv.URL + "/acme/app", Auth: config.Auth{Type: "app", AppID: "1", InstallationID: "2", PrivateKey: pemKey}})
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = srv.Client().Transport
	if err := c.DeleteRunner(context.Background(), 5); err == nil {
		t.Error("token 404 reported as a successful delete")
	}

	// A redirected DELETE must not turn into a GET that "succeeds".
	var gets int
	srv2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusMovedPermanently)
			return
		}
		gets++
		w.WriteHeader(200)
	}))
	defer srv2.Close()
	c2, _ := New(&config.Config{URL: srv2.URL + "/acme/app", Auth: config.Auth{Type: "pat", PAT: "tok"}})
	c2.http.Transport = srv2.Client().Transport
	if err := c2.DeleteRunner(context.Background(), 5); err == nil || gets != 0 {
		t.Errorf("redirected DELETE: err=%v, followed with %d GETs", err, gets)
	}
}

func TestDeleteFollows307WithSameMethod(t *testing.T) {
	var methods []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/api/v3/repos/") {
			http.Redirect(w, r, "/api/v3/repositories/9/actions/runners/5", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c, _ := New(&config.Config{URL: srv.URL + "/acme/app", Auth: config.Auth{Type: "pat", PAT: "tok"}})
	c.http.Transport = srv.Client().Transport
	if err := c.DeleteRunner(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || !strings.HasPrefix(methods[1], "DELETE /api/v3/repositories/9/") {
		t.Fatalf("requests: %v", methods)
	}
}

// A renamed repo answers POSTs with a same-host 307; the JIT request must
// follow it as a POST with its body.
func TestJITFollows307WithBody(t *testing.T) {
	var hops []string
	var got map[string]any
	var auth2 string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops = append(hops, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v3/repos/acme/app/actions/runners/generate-jitconfig":
			http.Redirect(w, r, "/api/v3/repositories/9/actions/runners/generate-jitconfig", http.StatusTemporaryRedirect)
		case "/api/v3/repositories/9/actions/runners/generate-jitconfig":
			auth2 = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode body on hop 2: %v", err)
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"runner": map[string]any{"id": 42}, "encoded_jit_config": "ENC"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(&config.Config{URL: srv.URL + "/acme/app", Auth: config.Auth{Type: "pat", PAT: "tok"}})
	c.http.Transport = srv.Client().Transport
	enc, r, err := c.JITConfig(context.Background(), "ghr-3456-0123abcd-1", []string{"self-hosted"})
	if err != nil || enc != "ENC" || r.ID != 42 {
		t.Fatalf("enc=%q runner=%+v err=%v hops=%v", enc, r, err, hops)
	}
	if len(hops) != 2 || hops[1] != "POST /api/v3/repositories/9/actions/runners/generate-jitconfig" {
		t.Fatalf("hops=%v", hops)
	}
	if auth2 != "Bearer tok" {
		t.Errorf("Authorization on hop 2 = %q", auth2)
	}
	labels, _ := got["labels"].([]any)
	if got["name"] != "ghr-3456-0123abcd-1" || got["runner_group_id"] != float64(1) || len(labels) != 1 || labels[0] != "self-hosted" {
		t.Errorf("body on hop 2 = %v", got)
	}
}

// DeleteSlotRunners reads every page of runners, and one failed DELETE
// doesn't stop the others.
func TestDeleteSlotRunnersPagesAndErrors(t *testing.T) {
	var pages []string
	var deleted []int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/orgs/acme/actions/runners":
			if r.URL.Query().Get("per_page") != "100" {
				t.Errorf("per_page=%q, want 100", r.URL.Query().Get("per_page"))
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			pages = append(pages, r.URL.Query().Get("page"))
			var rs []map[string]any
			n := 100
			if page == 2 {
				n = 30
			}
			for i := 0; i < n; i++ {
				id := (page-1)*100 + i + 1
				name := "other-" + strconv.Itoa(id)
				if id == 5 || id == 120 || id == 130 {
					name = "ghr-3f9a-0123abcd-" + strconv.Itoa(id)
				}
				rs = append(rs, map[string]any{"id": id, "name": name})
			}
			json.NewEncoder(w).Encode(map[string]any{"runners": rs})
		case r.Method == "DELETE":
			id, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
			if id == 5 {
				w.WriteHeader(500)
				return
			}
			deleted = append(deleted, id)
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := NewWithTransport(&config.Config{URL: srv.URL + "/acme", Auth: config.Auth{Type: "pat", PAT: "tok"}}, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.DeleteSlotRunners(context.Background(), "ghr-3f9a-0123abcd")
	if !IsStatus(err, 500) || n != 2 {
		t.Errorf("n=%d err=%v, want 2 and the 500", n, err)
	}
	if strings.Join(pages, ",") != "1,2" {
		t.Errorf("pages read: %v", pages)
	}
	if len(deleted) != 2 || deleted[0] != 120 || deleted[1] != 130 {
		t.Errorf("deleted %v, want [120 130]", deleted)
	}
}
