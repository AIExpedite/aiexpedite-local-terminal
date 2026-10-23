package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// whatsAppPairingVector is one entry of testdata/whatsapp_pairing_vectors.json,
// the file the frontend's whatsappPairingCrypto.test.js decrypts too.
type whatsAppPairingVector struct {
	Name string `json:"name"`
	// The browser's private scalar (base64url, 32 bytes) and its JWK, so the
	// browser test can import it with WebCrypto.
	BrowserPrivD string `json:"browserPrivD"`
	BrowserJWK   struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
		D   string `json:"d"`
	} `json:"browserJwk"`
	BrowserPub string `json:"browserPub"`
	// The agent's ephemeral scalar and iv, recorded so the seal reproduces.
	AgentPrivD string                   `json:"agentPrivD"`
	IV         string                   `json:"iv"`
	Sealed     whatsAppPairingSealed    `json:"sealed"`
	Plaintext  whatsAppPairingPlaintext `json:"plaintext"`
}

func loadWhatsAppPairingVectors(t *testing.T) []whatsAppPairingVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/whatsapp_pairing_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var file struct {
		Vectors []whatsAppPairingVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(file.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	return file.Vectors
}

func TestWhatsAppPairingVectorsOpen(t *testing.T) {
	for _, v := range loadWhatsAppPairingVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			d, err := b64url.DecodeString(v.BrowserPrivD)
			if err != nil {
				t.Fatal(err)
			}
			priv, err := ecdh.P256().NewPrivateKey(d)
			if err != nil {
				t.Fatal(err)
			}
			if got := b64url.EncodeToString(priv.PublicKey().Bytes()); got != v.BrowserPub {
				t.Fatalf("browserPub mismatch: %s", got)
			}
			got, err := openWhatsAppPairing(priv, v.Sealed)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if got != v.Plaintext {
				t.Fatalf("plaintext = %+v, want %+v", got, v.Plaintext)
			}
		})
	}
}

// The vectors are reproducible: sealing with the recorded agent scalar and iv
// yields exactly the recorded ciphertext, so the
// file is the agent's real output and not hand-made.
func TestWhatsAppPairingVectorsReproduce(t *testing.T) {
	for _, v := range loadWhatsAppPairingVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			d, _ := b64url.DecodeString(v.AgentPrivD)
			agent, err := ecdh.P256().NewPrivateKey(d)
			if err != nil {
				t.Fatal(err)
			}
			iv, _ := b64url.DecodeString(v.IV)
			sealed, err := sealWhatsAppPairingWith(agent, iv, v.BrowserPub, v.Plaintext)
			if err != nil {
				t.Fatal(err)
			}
			if sealed != v.Sealed {
				t.Fatalf("sealed = %+v, want %+v", sealed, v.Sealed)
			}
		})
	}
}

func TestWhatsAppPairingRoundTripAndTamper(t *testing.T) {
	browser, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := b64url.EncodeToString(browser.PublicKey().Bytes())
	pt := whatsAppPairingPlaintext{Method: "qr", Code: "2@abc,def,ghi,jkl", ExpiresAt: 1800000000000}
	sealed, err := sealWhatsAppPairing(pub, pt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed.Ciphertext, "2@abc") {
		t.Fatal("ciphertext carries the plaintext")
	}
	got, err := openWhatsAppPairing(browser, sealed)
	if err != nil || got != pt {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	// Two seals of the same payload never share a key or an iv.
	again, _ := sealWhatsAppPairing(pub, pt)
	if again.EphemeralPub == sealed.EphemeralPub || again.IV == sealed.IV {
		t.Fatal("fresh key / iv per payload")
	}
	// A flipped ciphertext byte fails authentication.
	ct, _ := b64url.DecodeString(sealed.Ciphertext)
	ct[0] ^= 0x01
	tampered := sealed
	tampered.Ciphertext = b64url.EncodeToString(ct)
	if _, err := openWhatsAppPairing(browser, tampered); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	// Another browser cannot open it.
	other, _ := ecdh.P256().GenerateKey(rand.Reader)
	if _, err := openWhatsAppPairing(other, sealed); err == nil {
		t.Fatal("another key opened the payload")
	}
}

func TestWhatsAppPairingRefusesBadBrowserKeys(t *testing.T) {
	pt := whatsAppPairingPlaintext{Method: "phoneCode", Code: "ABCD-EFGH"}
	for _, bad := range []string{"", "not base64!", b64url.EncodeToString(bytes.Repeat([]byte{4}, 65)), b64url.EncodeToString([]byte{2, 1, 2})} {
		if _, err := sealWhatsAppPairing(bad, pt); err == nil {
			t.Fatalf("accepted browserPub %q", bad)
		}
	}
}
