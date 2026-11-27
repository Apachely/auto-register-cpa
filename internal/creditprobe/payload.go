package creditprobe

import (
	"github.com/MoeForge/moego/ojson"
)

// buildPayload renders the smallest request that still carries a completion
// ceiling: one prompt token plus the max_tokens reservation. The document is
// written through ojson so serialization happens exactly once.
func buildPayload(model string, maxTokens int) []byte {
	doc := ojson.NewDocString(`{"messages":[{"role":"user","content":"x"}],"stream":false}`)
	doc.Set("model", model)
	doc.Set("max_tokens", maxTokens)
	return doc.Bytes()
}
