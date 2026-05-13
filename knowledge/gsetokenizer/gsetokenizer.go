// Package gsetokenizer registers a Bleve tokenizer backed by go-ego/gse
// (Chinese word segmentation). The tokenizer is registered under the name
// "gse" via a package-level init() and can be referenced in custom Bleve
// index mappings by that name.
//
// Import this package for its side effects:
//
//	import _ "github.com/larryhou/llm-go/knowledge/gsetokenizer"
package gsetokenizer

import (
	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/registry"
	"github.com/go-ego/gse"
)

const Name = "gse"

var seg gse.Segmenter

func init() {
	// Load the embedded default dictionary. Errors are silently ignored so
	// the package can be imported safely even without a custom dict file.
	_ = seg.LoadDict()

	_ = registry.RegisterTokenizer(Name, func(config map[string]interface{}, cache *registry.Cache) (analysis.Tokenizer, error) {
		return &gseTokenizer{}, nil
	})
}

type gseTokenizer struct{}

// Tokenize splits input bytes using gse in search mode (shorter, more granular
// segments) and returns a Bleve TokenStream.
func (t *gseTokenizer) Tokenize(input []byte) analysis.TokenStream {
	segs := seg.ModeSegment(input, true)
	tokens := make(analysis.TokenStream, 0, len(segs))
	for pos, s := range segs {
		term := []byte(s.Token().Text())
		if len(term) == 0 {
			continue
		}
		tokens = append(tokens, &analysis.Token{
			Term:     term,
			Start:    s.Start(),
			End:      s.End(),
			Position: pos + 1,
			Type:     analysis.Ideographic,
		})
	}
	return tokens
}
