package backup

import (
	"bytes"
	"testing"
)

func TestEncryptBlobRoundTrip(t *testing.T) {
	master := deriveMasterKey([]byte("0123456789abcdef0123456789abcdef"))
	plaintext := []byte(`{"table":"tenants","row":{"id":1,"name":"alpha"}}`)

	blob, err := encryptBlob(master, plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(blob, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}
	if string(blob[:len(encryptionMagic)]) != encryptionMagic {
		t.Fatalf("missing magic header: %q", blob[:8])
	}

	got, err := decryptBlob(master, blob)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("roundtrip mismatch: %q != %q", got, plaintext)
	}
}

func TestDecryptBlobWrongKey(t *testing.T) {
	master := deriveMasterKey([]byte("0123456789abcdef0123456789abcdef"))
	wrong := deriveMasterKey([]byte("fedcba9876543210fedcba9876543210"))
	blob, err := encryptBlob(master, []byte("secret metadata"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := decryptBlob(wrong, blob); err == nil {
		t.Fatal("expected decryption failure with wrong master key")
	}
}

func TestDecryptBlobTampered(t *testing.T) {
	master := deriveMasterKey([]byte("0123456789abcdef0123456789abcdef"))
	blob, err := encryptBlob(master, []byte("secret metadata"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Flip a byte inside the ciphertext body (past header + sealed DEK).
	blob[len(blob)-1] ^= 0xFF
	if _, err := decryptBlob(master, blob); err == nil {
		t.Fatal("expected GCM integrity failure on tampered blob")
	}
}

func TestDecryptBlobRejectsGarbage(t *testing.T) {
	master := deriveMasterKey([]byte("0123456789abcdef0123456789abcdef"))
	for _, garbage := range [][]byte{nil, []byte("ZBK"), []byte("XXXX-truncated-data")} {
		if _, err := decryptBlob(master, garbage); err == nil {
			t.Fatalf("expected rejection of garbage input %q", garbage)
		}
	}
}

func TestSHA256Hex(t *testing.T) {
	// SHA-256 of the empty string.
	got := sha256Bytes(nil)
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Fatalf("empty-string digest mismatch: %s", got)
	}
}
