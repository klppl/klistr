// Package nostr implements Nostr protocol handling for the klistr bridge.
package nostr

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/nbd-wtf/go-nostr"
)

// Signer provides signing for both the local Nostr user and derived keys for
// ActivityPub actors. The local user's actual private key is used for their own
// events; deterministic derived keys (SHA-256(localPrivKey + ":" + apID)) are
// used for bridged AP actors.
type Signer struct {
	localPrivKey string
	localPubKey  string
	mu           sync.RWMutex
	cache        map[string]string // apID → derived hex privkey
}

// NewSigner creates a new Signer with the user's Nostr private and public keys.
func NewSigner(privKey, pubKey string) *Signer {
	return &Signer{
		localPrivKey: privKey,
		localPubKey:  pubKey,
		cache:        make(map[string]string),
	}
}

// SignAsUser signs a Nostr event with the user's actual private key.
func (s *Signer) SignAsUser(event *nostr.Event) error {
	return event.Sign(s.localPrivKey)
}

// LocalPublicKey returns the user's real Nostr public key.
func (s *Signer) LocalPublicKey() string {
	return s.localPubKey
}

// derivedPrivKey returns the deterministic private key for an AP actor ID.
// Derivation: SHA-256(localPrivKey + ":" + apID). Result is cached.
func (s *Signer) derivedPrivKey(apID string) string {
	s.mu.RLock()
	if key, ok := s.cache[apID]; ok {
		s.mu.RUnlock()
		return key
	}
	s.mu.RUnlock()

	seed := s.localPrivKey + ":" + apID
	hash := sha256.Sum256([]byte(seed))
	key := hex.EncodeToString(hash[:])

	s.mu.Lock()
	s.cache[apID] = key
	s.mu.Unlock()
	return key
}

// PublicKey returns the derived secp256k1 public key for an AP actor ID.
func (s *Signer) PublicKey(apID string) (string, error) {
	return nostr.GetPublicKey(s.derivedPrivKey(apID))
}

// Sign derives a deterministic key for an AP actor and signs the event.
func (s *Signer) Sign(event *nostr.Event, apID string) error {
	return event.Sign(s.derivedPrivKey(apID))
}
