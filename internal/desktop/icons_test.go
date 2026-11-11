package desktop

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

func TestBuildStatusICOHasAllSizesAndBadge(t *testing.T) {
	for _, tone := range []string{toneIdle, toneOK, toneWarn, toneBad, toneBusy} {
		ico, err := buildStatusICO(tone)
		if err != nil {
			t.Fatalf("%s: %v", tone, err)
		}
		var hdr [3]uint16
		if err := binary.Read(bytes.NewReader(ico[:6]), binary.LittleEndian, &hdr); err != nil {
			t.Fatal(err)
		}
		if hdr[1] != 1 || int(hdr[2]) != len(iconSizes) {
			t.Fatalf("%s: cabeçalho ico inválido %v", tone, hdr)
		}
		// A última entrada (48 px) tem de decodificar como PNG do tamanho certo.
		e := ico[6+16*(len(iconSizes)-1):]
		size := binary.LittleEndian.Uint32(e[8:12])
		off := binary.LittleEndian.Uint32(e[12:16])
		img, err := png.Decode(bytes.NewReader(ico[off : off+size]))
		if err != nil {
			t.Fatalf("%s: png inválido: %v", tone, err)
		}
		if img.Bounds().Dx() != 48 {
			t.Fatalf("%s: tamanho %d", tone, img.Bounds().Dx())
		}
		// O canto inferior direito tem o selo (cor do tom) — exceto idle.
		r, g, b, a := img.At(41, 41).RGBA()
		c, has := toneColor[tone]
		if has && (a == 0 || uint8(r>>8) != c.R || uint8(g>>8) != c.G || uint8(b>>8) != c.B) {
			t.Errorf("%s: selo ausente no canto (rgba %d %d %d %d)", tone, r>>8, g>>8, b>>8, a>>8)
		}
	}
}
