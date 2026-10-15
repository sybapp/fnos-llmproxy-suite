package fnos

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
)

const alphaNum = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func randomString(n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphaNum)))
	for i := range out {
		x, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphaNum[x.Int64()]
	}
	return string(out), nil
}

func randomBytes(n int) ([]byte, error) {
	out := make([]byte, n)
	_, err := rand.Read(out)
	return out, err
}

func parseRSAPublicKey(pemText string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("invalid RSA public key PEM")
	}
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if pub, ok := k.(*rsa.PublicKey); ok {
			return pub, nil
		}
	}
	if pub, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return pub, nil
	}
	if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
		if pub, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			return pub, nil
		}
	}
	return nil, errors.New("unsupported RSA public key format")
}

func rsaEncryptPKCS1v15(pubPEM, plaintext string) (string, error) {
	pub, err := parseRSAPublicKey(pubPEM)
	if err != nil {
		return "", err
	}
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

func pkcs7Pad(in []byte, blockSize int) []byte {
	n := blockSize - len(in)%blockSize
	out := make([]byte, len(in)+n)
	copy(out, in)
	for i := len(in); i < len(out); i++ {
		out[i] = byte(n)
	}
	return out
}

func pkcs7Unpad(in []byte, blockSize int) ([]byte, error) {
	if len(in) == 0 || len(in)%blockSize != 0 {
		return nil, errors.New("invalid PKCS7 data")
	}
	n := int(in[len(in)-1])
	if n == 0 || n > blockSize || n > len(in) {
		return nil, errors.New("invalid PKCS7 padding")
	}
	for _, b := range in[len(in)-n:] {
		if int(b) != n {
			return nil, errors.New("invalid PKCS7 padding")
		}
	}
	return in[:len(in)-n], nil
}

func aesCBCEncrypt(plaintext []byte, key string, iv []byte) (string, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	if len(iv) != block.BlockSize() {
		return "", errors.New("invalid AES IV length")
	}
	p := pkcs7Pad(plaintext, block.BlockSize())
	out := make([]byte, len(p))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, p)
	return base64.StdEncoding.EncodeToString(out), nil
}

func decryptSecret(secret, key string, iv []byte) (string, error) {
	ct, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	if len(ct)%block.BlockSize() != 0 || len(iv) != block.BlockSize() {
		return "", errors.New("invalid encrypted secret")
	}
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ct)
	out, err = pkcs7Unpad(out, block.BlockSize())
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

func hmacSignature(jsonBytes []byte, signKey string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(signKey)
	if err != nil {
		return "", fmt.Errorf("decode sign key: %w", err)
	}
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(jsonBytes)
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}
