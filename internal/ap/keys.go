package ap

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
)

// KeyPair holds the RSA key pair used for ActivityPub HTTP signatures.
type KeyPair struct {
	Private    *rsa.PrivateKey
	Public     *rsa.PublicKey
	PublicPEM  string
}

// LoadOrGenerateKeyPair loads an RSA key pair from PEM files, or generates
// a new one if the files do not exist. This means zero-setup for new installs.
func LoadOrGenerateKeyPair(privatePath, publicPath string) (*KeyPair, error) {
	privPEM, err := os.ReadFile(privatePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read private key: %w", err)
		}
		slog.Info("RSA key pair not found, generating new one", "private", privatePath, "public", publicPath)
		return generateAndSaveKeyPair(privatePath, publicPath)
	}

	pubPEM, err := os.ReadFile(publicPath)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}

	return parseKeyPair(privPEM, pubPEM)
}

func generateAndSaveKeyPair(privatePath, publicPath string) (*KeyPair, error) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate RSA key: %w", err)
	}

	// Encode private key.
	privBytes := x509.MarshalPKCS1PrivateKey(privKey)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privBytes})

	// Encode public key.
	pubBytes, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})

	if err := os.WriteFile(privatePath, privPEM, 0600); err != nil {
		return nil, fmt.Errorf("write private key: %w", err)
	}
	if err := os.WriteFile(publicPath, pubPEM, 0644); err != nil {
		return nil, fmt.Errorf("write public key: %w", err)
	}

	slog.Info("generated RSA key pair", "private", privatePath, "public", publicPath)
	return parseKeyPair(privPEM, pubPEM)
}

func parseKeyPair(privPEM, pubPEM []byte) (*KeyPair, error) {
	privBlock, _ := pem.Decode(privPEM)
	if privBlock == nil {
		return nil, fmt.Errorf("failed to decode private key PEM")
	}

	privKey, err := x509.ParsePKCS1PrivateKey(privBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	pubBlock, _ := pem.Decode(pubPEM)
	if pubBlock == nil {
		return nil, fmt.Errorf("failed to decode public key PEM")
	}

	pubInterface, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}

	pubKey, ok := pubInterface.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA public key")
	}

	return &KeyPair{
		Private:   privKey,
		Public:    pubKey,
		PublicPEM: string(pubPEM),
	}, nil
}
