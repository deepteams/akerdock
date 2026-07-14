package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

const nonceSize = 12

// aad builds the additional authenticated data binding a ciphertext to its
// row: table || column || row uuid concatenated (data-dictionary §2.7). This
// prevents replaying a ciphertext from one row or column into another.
func aad(table, column, rowUUID string) []byte {
	return []byte(table + column + rowUUID)
}

// Encrypt seals plaintext for a given table/column/row with the active key
// version. Output layout (data-dictionary §2.7):
//
//	key_version (4 bytes big-endian) || nonce (12 bytes) || AES-256-GCM ciphertext (tag included)
func (k *Keyring) Encrypt(table, column, rowUUID string, plaintext []byte) ([]byte, error) {
	gcm, err := k.gcm(k.active)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4+nonceSize, 4+nonceSize+len(plaintext)+gcm.Overhead())
	binary.BigEndian.PutUint32(out[:4], k.active)
	if _, err := rand.Read(out[4 : 4+nonceSize]); err != nil {
		return nil, fmt.Errorf("envelope: nonce generation: %w", err)
	}
	return gcm.Seal(out, out[4:4+nonceSize], plaintext, aad(table, column, rowUUID)), nil
}

// Decrypt opens a *_enc value. A key version present in the data but absent
// from the key file yields an explicit error naming the missing version —
// never an empty value (instance-config §3.4.3).
func (k *Keyring) Decrypt(table, column, rowUUID string, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 4+nonceSize {
		return nil, fmt.Errorf("envelope: ciphertext too short (%d bytes)", len(ciphertext))
	}
	version := binary.BigEndian.Uint32(ciphertext[:4])
	if _, ok := k.keys[version]; !ok {
		return nil, fmt.Errorf("envelope: key version %d referenced by %s.%s is missing from the master key file — restore that version, do not remove keys still referenced by data (runbook key-rotation.md)", version, table, column)
	}
	gcm, err := k.gcm(version)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, ciphertext[4:4+nonceSize], ciphertext[4+nonceSize:], aad(table, column, rowUUID))
	if err != nil {
		return nil, fmt.Errorf("envelope: decryption of %s.%s failed (key version %d): %w", table, column, version, err)
	}
	return plaintext, nil
}

func (k *Keyring) gcm(version uint32) (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.keys[version])
	if err != nil {
		return nil, fmt.Errorf("envelope: %w", err)
	}
	return cipher.NewGCM(block)
}
