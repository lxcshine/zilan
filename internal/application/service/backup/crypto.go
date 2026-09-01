package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// Envelope encryption for metadata blobs (PRD §4.4): each snapshot gets
// a fresh random data-encrypting key (DEK); the DEK itself is sealed
// with the master key derived from SYSTEM_AES_KEY. Restore therefore
// only needs SYSTEM_AES_KEY — the sealed DEK travels with the blob.
//
// Encrypted blob layout (all big-endian where relevant):
//
//	"ZBK1"                         4 bytes   magic
//	sealed-DEK length              2 bytes   uint16
//	sealed DEK                    12+32+16   nonce ‖ DEK ‖ GCM tag (sealed by master key)
//	nonce                         12 bytes   blob nonce
//	ciphertext ‖ tag              N+16       AES-256-GCM over the plaintext
//
// Metadata blobs are gzip-compressed jsonl/sql streams buffered in
// memory before encryption, which is why a whole-buffer format is fine;
// the file tier is copied byte-for-byte and never passes through here.

const encryptionMagic = "ZBK1"

// masterKeySalt domain-separates the backup master key from other uses
// of SYSTEM_AES_KEY (API-key encryption etc.) so the raw 32-byte secret
// is never used verbatim in two subsystems.
func deriveMasterKey(secret []byte) []byte {
	sum := sha256.Sum256(append([]byte("zilan-backup-v1:"), secret...))
	return sum[:]
}

// newDEK generates a fresh 32-byte data key.
func newDEK() ([]byte, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("generate DEK: %w", err)
	}
	return dek, nil
}

// sealWithKey encrypts plaintext with AES-256-GCM under key, prefixing
// a fresh nonce.
func sealWithKey(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// openWithKey reverses sealWithKey.
func openWithKey(key, blob []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize()+gcm.Overhead() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encryptBlob applies the full envelope format to plaintext.
func encryptBlob(master, plaintext []byte) ([]byte, error) {
	dek, err := newDEK()
	if err != nil {
		return nil, err
	}
	sealedDEK, err := sealWithKey(master, dek)
	if err != nil {
		return nil, fmt.Errorf("seal DEK: %w", err)
	}
	if len(sealedDEK) > 0xFFFF {
		return nil, fmt.Errorf("sealed DEK too large")
	}
	body, err := sealWithKey(dek, plaintext)
	if err != nil {
		return nil, fmt.Errorf("encrypt payload: %w", err)
	}

	out := make([]byte, 0, len(encryptionMagic)+2+len(sealedDEK)+len(body))
	out = append(out, encryptionMagic...)
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(sealedDEK)))
	out = append(out, lenBuf[:]...)
	out = append(out, sealedDEK...)
	out = append(out, body...)
	return out, nil
}

// decryptBlob reverses encryptBlob. A wrong SYSTEM_AES_KEY surfaces as
// a GCM open failure with an actionable message.
func decryptBlob(master, blob []byte) ([]byte, error) {
	header := len(encryptionMagic) + 2
	if len(blob) < header {
		return nil, fmt.Errorf("not a zilan encrypted backup blob (truncated header)")
	}
	if string(blob[:len(encryptionMagic)]) != encryptionMagic {
		return nil, fmt.Errorf("not a zilan encrypted backup blob (bad magic)")
	}
	sealedLen := int(binary.BigEndian.Uint16(blob[len(encryptionMagic):header]))
	if len(blob) < header+sealedLen {
		return nil, fmt.Errorf("truncated sealed key section")
	}
	sealedDEK := blob[header : header+sealedLen]
	body := blob[header+sealedLen:]

	dek, err := openWithKey(master, sealedDEK)
	if err != nil {
		return nil, fmt.Errorf("decrypt backup data key: wrong SYSTEM_AES_KEY or corrupted blob")
	}
	plaintext, err := openWithKey(dek, body)
	if err != nil {
		return nil, fmt.Errorf("decrypt backup payload: %w", err)
	}
	return plaintext, nil
}

// sha256Hex streams a digest over r.
func sha256Hex(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hexEncode(h.Sum(nil)), n, nil
}

// sha256Bytes digests an in-memory buffer.
func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hexEncode(sum[:])
}

func hexEncode(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out)
}
