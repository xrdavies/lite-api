package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
)

// Converted protocols have no server-side response history. Retain store=true histories,
// encrypted and authenticated to the same tenant, response ID and upstream source.
func (a *App) chatHistoryCipher() cipher.AEAD {
	key := sha256.Sum256(append([]byte("lite-api/chat-history/v1\x00"), a.secret...))
	block, _ := aes.NewCipher(key[:])
	sealed, _ := cipher.NewGCM(block)
	return sealed
}

func (a *App) chatHistory(g *gatewayIdentity, id string, binding *responseBinding) ([]convertedChatMessage, error) {
	if binding == nil || binding.History == "" {
		return nil, nil
	}
	sealed := a.chatHistoryCipher()
	raw, err := base64.RawStdEncoding.DecodeString(binding.History)
	if err != nil || len(raw) < sealed.NonceSize() || len(raw) > (2<<20)+64 {
		return nil, &apiError{503, "invalid saved response history"}
	}
	plain, err := sealed.Open(nil, raw[:sealed.NonceSize()], raw[sealed.NonceSize():], []byte(responseBindingKey(g, id)+"\n"+binding.Target))
	if err != nil {
		return nil, &apiError{503, "response history could not be decrypted"}
	}
	var messages []convertedChatMessage
	if json.Unmarshal(plain, &messages) != nil || len(messages) > 256 {
		return nil, &apiError{503, "invalid saved response history"}
	}
	return messages, nil
}

func (a *App) bindChatResponse(ctx context.Context, g *gatewayIdentity, u *upstreamAccount, id string, messages []convertedChatMessage) error {
	raw, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	if len(raw) > 2<<20 || len(messages) > 256 {
		return &apiError{502, "response history exceeds limit; resend full input with store=false"}
	}
	sealed := a.chatHistoryCipher()
	nonce := make([]byte, sealed.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	binding := responseBinding{AccountID: u.ID, Target: responseTarget(u)}
	value := sealed.Seal(nonce, nonce, raw, []byte(responseBindingKey(g, id)+"\n"+binding.Target))
	binding.History = base64.RawStdEncoding.EncodeToString(value)
	return a.storeResponseBinding(ctx, g, id, binding)
}
