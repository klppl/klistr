// Package nostr implements Nostr protocol handling for the klistr bridge.
package nostr

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip04"
	"golang.org/x/crypto/hkdf"
)

// Signer provides signing for both the local Nostr user and derived keys for
// ActivityPub actors. The local user's actual private key is used for their own
// events; deterministic derived keys are used for bridged AP actors, derived via
// HKDF-SHA256(ikm=privkey_bytes, salt=nil, info="klistr-ap-actor:"+apID).
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
//
// Derivation: HKDF-SHA256(ikm=privkey_bytes, salt=nil, info="klistr-ap-actor:"+apID)
//
// Using HKDF with a domain-separated info label instead of the previous naive
// SHA-256(hex(privkey)+":"+apID) concatenation eliminates the second-preimage
// risk where an attacker-controlled apID could be chosen to collide with
// another valid seed string. salt=nil is safe because the IKM already carries
// 256 bits of entropy. Result is cached.
func (s *Signer) derivedPrivKey(apID string) string {
	s.mu.RLock()
	if key, ok := s.cache[apID]; ok {
		s.mu.RUnlock()
		return key
	}
	s.mu.RUnlock()

	privKeyBytes, err := hex.DecodeString(s.localPrivKey)
	if err != nil || len(privKeyBytes) != 32 {
		// Should never happen: the private key is validated at startup.
		panic("signer: invalid local private key")
	}
	r := hkdf.New(sha256.New, privKeyBytes, nil, []byte("klistr-ap-actor:"+apID))
	var derived [32]byte
	if _, err := io.ReadFull(r, derived[:]); err != nil {
		// Cannot fail: hkdf.Reader is an infinite stream of key material.
		panic("signer: hkdf read failed: " + err.Error())
	}
	key := hex.EncodeToString(derived[:])

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

// CreateDMToSelf creates a NIP-04 encrypted direct message addressed to the
// local user's own pubkey. This is used for self-notifications (e.g. new
// Fediverse follower alerts). The returned event is already signed.
func (s *Signer) CreateDMToSelf(message string) (*nostr.Event, error) {
	sharedSecret, err := nip04.ComputeSharedSecret(s.localPubKey, s.localPrivKey)
	if err != nil {
		return nil, fmt.Errorf("compute shared secret: %w", err)
	}
	encrypted, err := nip04.Encrypt(message, sharedSecret)
	if err != nil {
		return nil, fmt.Errorf("encrypt DM: %w", err)
	}
	event := &nostr.Event{
		Kind:      4,
		Content:   encrypted,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"p", s.localPubKey}},
	}
	if err := event.Sign(s.localPrivKey); err != nil {
		return nil, fmt.Errorf("sign DM: %w", err)
	}
	return event, nil
}
