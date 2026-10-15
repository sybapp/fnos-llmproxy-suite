package fnos

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
)

func TestAESSecretRoundTrip(t *testing.T) {
	key, err := randomString(32)
	if err != nil {
		t.Fatal(err)
	}
	iv, err := randomBytes(16)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("0123456789abcdef0123456789abcdef")
	enc, err := aesCBCEncrypt(plain, key, iv)
	if err != nil {
		t.Fatal(err)
	}
	sign, err := decryptSecret(enc, key, iv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(sign)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Fatalf("round trip mismatch: %q", got)
	}
}

func TestRSAEncrypt(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	ct64, err := rsaEncryptPKCS1v15(pub, "secret")
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := base64.StdEncoding.DecodeString(ct64)
	got, err := rsa.DecryptPKCS1v15(rand.Reader, priv, ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret" {
		t.Fatalf("got %q", got)
	}
}
