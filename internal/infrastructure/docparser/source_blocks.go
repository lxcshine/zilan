package docparser

import (
	"encoding/json"

	"github.com/Tencent/WeKnora/docreader/proto"
	"github.com/Tencent/WeKnora/internal/types"
)

// decodeSourceBlock decodes one parser-reported source block. A block with an
// empty range or without a decodable locator type is dropped; citations
// degrade to the chunk without a position.
func decodeSourceBlock(b *proto.SourceBlock) (types.SourceBlock, bool) {
	if b == nil || b.End <= b.Start {
		return types.SourceBlock{}, false
	}
	var loc types.SourceLocator
	if err := json.Unmarshal([]byte(b.LocatorJson), &loc); err != nil || loc.Type == "" {
		return types.SourceBlock{}, false
	}
	return types.SourceBlock{Start: int(b.Start), End: int(b.End), Locator: loc}, true
}

// decodeSourceBlocks decodes the parser-reported blocks, keeping the given
// content length for bounds clamping.
func decodeSourceBlocks(blocks []*proto.SourceBlock) []types.SourceBlock {
	out := make([]types.SourceBlock, 0, len(blocks))
	for _, b := range blocks {
		if sb, ok := decodeSourceBlock(b); ok {
			out = append(out, sb)
		}
	}
	return out
}
