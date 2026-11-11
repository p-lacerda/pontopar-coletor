package desktop

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	"github.com/p-lacerda/pontopar-coletor/internal/assets"
)

// Tons do ícone da bandeja: o glifo PontoPar com um selo colorido de status.
const (
	toneIdle = "idle" // sem selo (carregando / indefinido)
	toneOK   = "ok"   // coletando normalmente
	toneWarn = "warn" // Thera fora / configuração pendente
	toneBad  = "bad"  // relógio fora / coletor parado
	toneBusy = "busy" // refill em andamento
)

var toneColor = map[string]color.NRGBA{
	toneOK:   {0x16, 0xb3, 0x6f, 0xff},
	toneWarn: {0xf5, 0x9e, 0x0b, 0xff},
	toneBad:  {0xe5, 0x48, 0x4d, 0xff},
	toneBusy: {0x3b, 0x82, 0xf6, 0xff},
}

// iconSizes cobre 100%–200% de escala (16, 20, 24, 32) e o Alt+Tab (48).
var iconSizes = []int{16, 20, 24, 32, 48}

// buildStatusICO monta um .ico (entradas PNG, Windows Vista+) com o glifo e,
// para tons diferentes de idle, um selo redondo no canto inferior direito.
func buildStatusICO(tone string) ([]byte, error) {
	pngs := make([][]byte, 0, len(iconSizes))
	for _, size := range iconSizes {
		src, err := assets.Decode(assets.GlyphForSize(size))
		if err != nil {
			return nil, err
		}
		img := image.NewNRGBA(image.Rect(0, 0, size, size))
		draw.Draw(img, img.Bounds(), src, src.Bounds().Min, draw.Over)
		if c, ok := toneColor[tone]; ok {
			drawBadge(img, c)
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		pngs = append(pngs, buf.Bytes())
	}
	return encodeICO(pngs, iconSizes), nil
}

// drawBadge pinta um círculo com anel branco no canto inferior direito, com
// antisserrilhamento por supersampling (fica nítido até em 16 px).
func drawBadge(img *image.NRGBA, fill color.NRGBA) {
	n := float64(img.Bounds().Dx())
	r := n * 0.19              // raio do selo
	ring := math.Max(1, n/20)  // espessura do anel branco
	cx, cy := n-r-0.5, n-r-0.5 // encostado no canto
	const ss = 4               // amostras por eixo
	for y := int(cy - r - ring - 1); y <= int(cy+r+ring+1); y++ {
		for x := int(cx - r - ring - 1); x <= int(cx+r+ring+1); x++ {
			if x < 0 || y < 0 || x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
				continue
			}
			var inFill, inRing float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					d := math.Hypot(px-cx, py-cy)
					switch {
					case d <= r:
						inFill++
					case d <= r+ring:
						inRing++
					}
				}
			}
			total := float64(ss * ss)
			if inRing > 0 {
				blend(img, x, y, color.NRGBA{0xff, 0xff, 0xff, 0xff}, inRing/total)
			}
			if inFill > 0 {
				blend(img, x, y, fill, inFill/total)
			}
		}
	}
}

// blend aplica c com cobertura a (0..1) sobre o pixel (operador "over").
func blend(img *image.NRGBA, x, y int, c color.NRGBA, a float64) {
	dst := img.NRGBAAt(x, y)
	sa := a * float64(c.A) / 255
	da := float64(dst.A) / 255
	oa := sa + da*(1-sa)
	if oa <= 0 {
		return
	}
	mix := func(s, d uint8) uint8 {
		v := (float64(s)*sa + float64(d)*da*(1-sa)) / oa
		return uint8(math.Round(math.Min(255, math.Max(0, v))))
	}
	img.SetNRGBA(x, y, color.NRGBA{mix(c.R, dst.R), mix(c.G, dst.G), mix(c.B, dst.B), uint8(math.Round(oa * 255))})
}

// encodeICO grava o contêiner .ico com imagens PNG.
func encodeICO(pngs [][]byte, sizes []int) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, uint16(len(pngs))})
	offset := 6 + 16*len(pngs)
	for i, p := range pngs {
		dim := byte(sizes[i])
		if sizes[i] >= 256 {
			dim = 0
		}
		buf.WriteByte(dim)
		buf.WriteByte(dim)
		buf.WriteByte(0)                                        // paleta
		buf.WriteByte(0)                                        // reservado
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // planos
		_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // bpp
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(p)))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += len(p)
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	return buf.Bytes()
}
