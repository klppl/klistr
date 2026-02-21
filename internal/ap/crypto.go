package ap

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

func decodePEM(data []byte) (*pem.Block, []byte) {
	return pem.Decode(data)
}

func parsePublicKey(b []byte) (*rsa.PublicKey, error) {
	pub, err := x509.ParsePKIXPublicKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA public key")
	}
	return rsaPub, nil
}
