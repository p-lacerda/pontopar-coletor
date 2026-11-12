// Package assets embute a marca PontoPar (o glifo "Pd" sem fundo) usada no
// ícone da bandeja, no favicon/logo do painel e na janela do app.
package assets

import (
	"bytes"
	_ "embed"
	"image"
	"image/png"
)

//go:embed glyph-16.png
var Glyph16 []byte

//go:embed glyph-20.png
var Glyph20 []byte

//go:embed glyph-24.png
var Glyph24 []byte

//go:embed glyph-32.png
var Glyph32 []byte

//go:embed glyph-48.png
var Glyph48 []byte

//go:embed glyph-64.png
var Glyph64 []byte

//go:embed glyph-256.png
var Glyph256 []byte

// GlyphForSize devolve o PNG pronto mais próximo (>=) do tamanho pedido.
func GlyphForSize(px int) []byte {
	switch {
	case px <= 16:
		return Glyph16
	case px <= 20:
		return Glyph20
	case px <= 24:
		return Glyph24
	case px <= 32:
		return Glyph32
	case px <= 48:
		return Glyph48
	case px <= 64:
		return Glyph64
	default:
		return Glyph256
	}
}

// Decode decodifica um dos PNGs embutidos.
func Decode(b []byte) (image.Image, error) { return png.Decode(bytes.NewReader(b)) }

// TheraMais256 é o "T" da TheraMais (sem fundo), mostrado no painel como
// "uma solução TheraMais". Extraído de thera-saude/apps/mobile/assets/icon.png.
//
//go:embed theramais-256.png
var TheraMais256 []byte
