package ap

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testPrivKey returns a freshly generated RSA private key for signing.
// 1024 bits is faster than 2048 and fine for test-only signers.
func testPrivKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestIsRetryableStatus(t *testing.T) {
	tests := []struct {
		code      int
		retryable bool
	}{
		{200, false}, // success — never reaches the function but check anyway: 2xx isn't 4xx/5xx
		{400, false}, // bad request — permanent
		{401, false}, // signature rejected — permanent
		{403, false}, // forbidden — permanent
		{404, false}, // inbox not found — permanent
		{408, true},  // request timeout — transient
		{410, false}, // gone — permanent
		{413, false}, // payload too large — permanent (won't fit on retry either)
		{422, false}, // semantic error — permanent
		{429, true},  // rate limited — wait then retry
		{500, true},  // server error — transient
		{502, true},  // bad gateway — transient
		{503, true},  // service unavailable — transient
		{504, true},  // gateway timeout — transient
	}
	for _, tc := range tests {
		if got := isRetryableStatus(tc.code); got != tc.retryable {
			t.Errorf("isRetryableStatus(%d) = %v, want %v", tc.code, got, tc.retryable)
		}
	}
}

// fakePerms generates a delivery target that returns a specific status code.
func fakeInbox(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/inbox"
}

func TestDeliverActivity_Permanent4xxWrapsErrPermanent(t *testing.T) {
	inbox := fakeInbox(t, 401)
	// Generate a throw-away RSA key for signing — we just need a valid signer;
	// the test server doesn't verify.
	err := DeliverActivity(context.Background(), inbox, map[string]interface{}{
		"@context": DefaultContext,
		"type":     "Create",
	}, "test#main-key", testPrivKey(t))
	if err == nil {
		t.Fatal("expected error from 401 response")
	}
	if !errors.Is(err, ErrPermanent) {
		t.Errorf("401 should wrap ErrPermanent; got: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status code; got: %v", err)
	}
}

func TestDeliverActivity_Retryable5xxNotPermanent(t *testing.T) {
	inbox := fakeInbox(t, 503)
	err := DeliverActivity(context.Background(), inbox, map[string]interface{}{
		"@context": DefaultContext,
		"type":     "Create",
	}, "test#main-key", testPrivKey(t))
	if err == nil {
		t.Fatal("expected error from 503 response")
	}
	if errors.Is(err, ErrPermanent) {
		t.Errorf("503 should NOT be permanent (transient infra issue): %v", err)
	}
}

func TestDeliverActivity_429NotPermanent(t *testing.T) {
	inbox := fakeInbox(t, 429)
	err := DeliverActivity(context.Background(), inbox, map[string]interface{}{
		"@context": DefaultContext,
		"type":     "Create",
	}, "test#main-key", testPrivKey(t))
	if err == nil {
		t.Fatal("expected error from 429 response")
	}
	if errors.Is(err, ErrPermanent) {
		t.Errorf("429 must NOT be permanent — wait and retry: %v", err)
	}
}
