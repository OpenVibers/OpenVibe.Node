package profilesig

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// testdata/ballbot.json was made by OpenVibe.Bot's own code (server/submissions.js canonicalJson and loadKey, a fresh
// Ed25519 key): a community profile with keys out of order, escapes, non-ASCII, U+2028, a control character and
// numbers JavaScript writes in exponent form, its signature and Bot's public key, and the canonical bytes Bot signed.
type fixture struct {
	Served struct {
		Profile   json.RawMessage `json:"profile"`
		Signature Signature       `json:"signature"`
	} `json:"served"`
	Key       Key    `json:"key"`
	Canonical string `json:"canonical"`
}

func load(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("testdata/ballbot.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCanonicalIsBots(t *testing.T) {
	f := load(t)
	got, err := Canonical(f.Served.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != f.Canonical {
		t.Fatalf("canonical JSON differs from Bot's:\n got %s\nwant %s", got, f.Canonical)
	}
	if _, err := Canonical([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Fatal("two values were accepted")
	}
}

func TestVerify(t *testing.T) {
	f := load(t)
	if err := Verify(f.Served.Profile, f.Served.Signature, f.Key); err != nil {
		t.Fatalf("Bot's signature: %v", err)
	}
	changed := []byte(strings.Replace(string(f.Served.Profile), `"max_speed": 0.5`, `"max_speed": 5`, 1))
	if string(changed) == string(f.Served.Profile) {
		t.Fatal("the fixture has no max_speed to change")
	}
	if err := Verify(changed, f.Served.Signature, f.Key); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a changed profile: %v", err)
	}
	other := f.Served.Signature
	other.KeyID = "0000000000000000"
	if err := Verify(f.Served.Profile, other, f.Key); err == nil || !strings.Contains(err.Error(), "signed with key") {
		t.Fatalf("a signature by another key: %v", err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	swapped := f.Key
	swapped.PublicKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if err := Verify(f.Served.Profile, f.Served.Signature, swapped); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("a key that is not the published id: %v", err)
	}
	swapped.KeyID = KeyID(der)
	sig := f.Served.Signature
	sig.KeyID = swapped.KeyID
	if err := Verify(f.Served.Profile, sig, swapped); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("another key under its own id: %v", err)
	}
}

func TestCheck(t *testing.T) {
	f := load(t)
	signed := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/profiles/signing-key":
			_ = json.NewEncoder(w).Encode(map[string]any{"key": f.Key})
		case "/api/v1/profiles/community.ballbot":
			doc := map[string]any{"profile": f.Served.Profile}
			if signed {
				doc["signature"] = f.Served.Signature
			}
			_ = json.NewEncoder(w).Encode(doc)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	res, err := Check(ctx, srv.Client(), srv.URL, "community.ballbot", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Plugin != "ballbot" || res.Version != "1" || res.KeyID != f.Key.KeyID {
		t.Fatalf("%+v", res)
	}
	if _, err := Check(ctx, srv.Client(), srv.URL, "community.ballbot", f.Served.Profile, f.Key.KeyID); err != nil {
		t.Fatalf("the same profile, the pinned key: %v", err)
	}
	if _, err := Check(ctx, srv.Client(), srv.URL, "community.ballbot", nil, "ffffffffffffffff"); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("another pinned key: %v", err)
	}
	sent := json.RawMessage(strings.Replace(string(f.Served.Profile), `"max_turn": 0.6`, `"max_turn": 1`, 1))
	if _, err := Check(ctx, srv.Client(), srv.URL, "community.ballbot", sent, ""); err == nil || !strings.Contains(err.Error(), "not the signed one") {
		t.Fatalf("a profile sent at pairing that is not the catalogue's: %v", err)
	}
	if _, err := Check(ctx, srv.Client(), srv.URL, "community.nope", nil, ""); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("an unknown profile: %v", err)
	}
	if _, err := Check(ctx, srv.Client(), srv.URL, "adeept.adr036", nil, ""); err == nil || !strings.Contains(err.Error(), "not a community profile") {
		t.Fatalf("a built-in profile: %v", err)
	}
	signed = false
	if _, err := Check(ctx, srv.Client(), srv.URL, "community.ballbot", nil, ""); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("no signature: %v", err)
	}
}
