// whatsapp_pairing_crypto.go — encrypts a WhatsApp pairing payload (a QR ref
// or an 8-character phone code) for the ONE browser that asked for it.
//
// The connect dialog generates an ephemeral ECDH P-256 key pair with WebCrypto
// and sends only the public key (`browserPub`) inside the signed link-start
// command. For every payload this agent makes a fresh P-256 key, derives
// AES-256-GCM key = HKDF-SHA256(ECDH(agentPriv, browserPub), salt, info) and
// posts {ephemeralPub, iv, ciphertext}. The cloud stores and relays only that
// ciphertext, so a pairing secret never exists in plaintext anywhere but this
// process and the browser tab — not in Firestore, not in a log.
//
// The parameters are shared-constants WHATSAPP_PAIRING_CRYPTO; the frontend's
// whatsappPairingCrypto.js decrypts with the same ones, and both sides assert
// the vectors in testdata/whatsapp_pairing_vectors.json. Change all three or
// none.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
)

const (
	whatsAppPairingHKDFSalt = "aix-whatsapp-pairing-v1"
	whatsAppPairingHKDFInfo = "aix-whatsapp-pairing-payload"
	whatsAppPairingIVBytes  = 12
)

// whatsAppPairingPlaintext is what the browser renders: a QR ref to draw, or
// the phone code to type on the phone.
type whatsAppPairingPlaintext struct {
	Method    string `json:"method"`
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
}

// whatsAppPairingSealed is the wire half of a `kind: pairing` report.
type whatsAppPairingSealed struct {
	EphemeralPub string `json:"ephemeralPub"`
	IV           string `json:"iv"`
	Ciphertext   string `json:"ciphertext"`
}

var b64url = base64.RawURLEncoding

// parseWhatsAppBrowserPub decodes the browser's raw uncompressed P-256 point.
func parseWhatsAppBrowserPub(browserPub string) (*ecdh.PublicKey, error) {
	raw, err := b64url.DecodeString(browserPub)
	if err != nil {
		return nil, errors.New("browserPub is not base64url")
	}
	if len(raw) != 65 || raw[0] != 0x04 {
		return nil, errors.New("browserPub is not an uncompressed P-256 point")
	}
	return ecdh.P256().NewPublicKey(raw)
}

// sealWhatsAppPairing encrypts plaintext for browserPub with a fresh agent key
// and a fresh iv.
func sealWhatsAppPairing(browserPub string, plaintext whatsAppPairingPlaintext) (whatsAppPairingSealed, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return whatsAppPairingSealed{}, err
	}
	iv := make([]byte, whatsAppPairingIVBytes)
	if _, err := rand.Read(iv); err != nil {
		return whatsAppPairingSealed{}, err
	}
	return sealWhatsAppPairingWith(priv, iv, browserPub, plaintext)
}

// sealWhatsAppPairingWith is sealWhatsAppPairing with the agent key and iv
// supplied, so the shared vectors are reproducible (Go ignores a caller's
// entropy source for key generation).
func sealWhatsAppPairingWith(priv *ecdh.PrivateKey, iv []byte, browserPub string, plaintext whatsAppPairingPlaintext) (whatsAppPairingSealed, error) {
	var sealed whatsAppPairingSealed
	if len(iv) != whatsAppPairingIVBytes {
		return sealed, errors.New("pairing iv must be 12 bytes")
	}
	peer, err := parseWhatsAppBrowserPub(browserPub)
	if err != nil {
		return sealed, err
	}
	key, err := deriveWhatsAppPairingKey(priv, peer)
	if err != nil {
		return sealed, err
	}
	defer clear(key)
	body, err := json.Marshal(plaintext)
	if err != nil {
		return sealed, err
	}
	aead, err := newWhatsAppGCM(key)
	if err != nil {
		return sealed, err
	}
	return whatsAppPairingSealed{
		EphemeralPub: b64url.EncodeToString(priv.PublicKey().Bytes()),
		IV:           b64url.EncodeToString(iv),
		Ciphertext:   b64url.EncodeToString(aead.Seal(nil, iv, body, nil)),
	}, nil
}

// openWhatsAppPairing is the browser's half, kept here for the round-trip
// tests and the vector generator.
func openWhatsAppPairing(browserPriv *ecdh.PrivateKey, sealed whatsAppPairingSealed) (whatsAppPairingPlaintext, error) {
	var out whatsAppPairingPlaintext
	pubRaw, err := b64url.DecodeString(sealed.EphemeralPub)
	if err != nil {
		return out, err
	}
	peer, err := ecdh.P256().NewPublicKey(pubRaw)
	if err != nil {
		return out, err
	}
	key, err := deriveWhatsAppPairingKey(browserPriv, peer)
	if err != nil {
		return out, err
	}
	iv, err := b64url.DecodeString(sealed.IV)
	if err != nil {
		return out, err
	}
	ct, err := b64url.DecodeString(sealed.Ciphertext)
	if err != nil {
		return out, err
	}
	aead, err := newWhatsAppGCM(key)
	if err != nil {
		return out, err
	}
	body, err := aead.Open(nil, iv, ct, nil)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(body, &out)
	return out, err
}

func deriveWhatsAppPairingKey(priv *ecdh.PrivateKey, peer *ecdh.PublicKey) ([]byte, error) {
	shared, err := priv.ECDH(peer)
	if err != nil {
		return nil, err
	}
	defer clear(shared)
	return hkdf.Key(sha256.New, shared, []byte(whatsAppPairingHKDFSalt), whatsAppPairingHKDFInfo, 32)
}

func newWhatsAppGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
