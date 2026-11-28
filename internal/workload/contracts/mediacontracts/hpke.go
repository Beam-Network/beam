package mediacontracts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

const (
	HPKEEncBytes = 32
	hpkeTagBytes = 16
)

var ErrHPKEOpen = errors.New("mediacontracts: hpke: cannot open")

var (
	hpkeKEMSuite = []byte("KEM\x00\x20")
	hpkeSuite    = []byte("HPKE\x00\x20\x00\x01\x00\x01")
)

func hpkeExtract(salt, ikm []byte) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

func hpkeExpand(prk, info []byte, length int) []byte {
	var out, previous []byte
	for counter := byte(1); len(out) < length; counter++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(previous)
		mac.Write(info)
		mac.Write([]byte{counter})
		previous = mac.Sum(nil)
		out = append(out, previous...)
	}
	return out[:length]
}

func hpkeConcat(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func hpkeLabeledExtract(suite, salt []byte, label string, ikm []byte) []byte {
	return hpkeExtract(salt, hpkeConcat([]byte("HPKE-v1"), suite, []byte(label), ikm))
}

func hpkeLabeledExpand(suite, prk []byte, label string, info []byte, length int) []byte {
	return hpkeExpand(prk, hpkeConcat([]byte{byte(length >> 8), byte(length)}, []byte("HPKE-v1"), suite, []byte(label), info), length)
}

func hpkeAEAD(dh, enc, recipient, info []byte) (cipher.AEAD, []byte, error) {
	prk := hpkeLabeledExtract(hpkeKEMSuite, nil, "eae_prk", dh)
	shared := hpkeLabeledExpand(hpkeKEMSuite, prk, "shared_secret", hpkeConcat(enc, recipient), 32)
	context := hpkeConcat([]byte{0}, hpkeLabeledExtract(hpkeSuite, nil, "psk_id_hash", nil), hpkeLabeledExtract(hpkeSuite, nil, "info_hash", info))
	secret := hpkeLabeledExtract(hpkeSuite, shared, "secret", nil)
	block, err := aes.NewCipher(hpkeLabeledExpand(hpkeSuite, secret, "key", context, 16))
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	return aead, hpkeLabeledExpand(hpkeSuite, secret, "base_nonce", context, 12), err
}

func HPKESeal(recipient, info, aad, plaintext []byte) ([]byte, error) {
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return hpkeSealWith(ephemeral, recipient, info, aad, plaintext)
}

func HPKESealDeterministic(ephemeral, recipient, info, aad, plaintext []byte) ([]byte, error) {
	key, err := ecdh.X25519().NewPrivateKey(ephemeral)
	if err != nil {
		return nil, err
	}
	return hpkeSealWith(key, recipient, info, aad, plaintext)
}

func hpkeSealWith(ephemeral *ecdh.PrivateKey, recipient, info, aad, plaintext []byte) ([]byte, error) {
	public, err := ecdh.X25519().NewPublicKey(recipient)
	if err != nil {
		return nil, err
	}
	dh, err := ephemeral.ECDH(public)
	if err != nil {
		return nil, err
	}
	enc := ephemeral.PublicKey().Bytes()
	aead, nonce, err := hpkeAEAD(dh, enc, recipient, info)
	if err != nil {
		return nil, err
	}
	return aead.Seal(enc, nonce, plaintext, aad), nil
}

func HPKEOpen(privateKey, info, aad, sealed []byte) ([]byte, error) {
	if len(sealed) < HPKEEncBytes+hpkeTagBytes {
		return nil, ErrHPKEOpen
	}
	key, err := ecdh.X25519().NewPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	enc, err := ecdh.X25519().NewPublicKey(sealed[:HPKEEncBytes])
	if err != nil {
		return nil, ErrHPKEOpen
	}
	dh, err := key.ECDH(enc)
	if err != nil {
		return nil, ErrHPKEOpen
	}
	aead, nonce, err := hpkeAEAD(dh, sealed[:HPKEEncBytes], key.PublicKey().Bytes(), info)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, sealed[HPKEEncBytes:], aad)
	if err != nil {
		return nil, ErrHPKEOpen
	}
	return plaintext, nil
}
