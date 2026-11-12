package notify

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"testing"
)

// TestDumpIcons writes a contact sheet of every icon, at 16, 24 and 32
// pixels, on a light and a dark taskbar, to $CLOUDSYNC_ICON_SHEET, for
// looking at. It does nothing unless that is set.
func TestDumpIcons(t *testing.T) {
	out := os.Getenv("CLOUDSYNC_ICON_SHEET")
	if out == "" {
		t.Skip("set CLOUDSYNC_ICON_SHEET to a .png path to draw the icons")
	}
	states := []State{StateOK, StateBusy, StateAttention, StateError, StatePaused, StateStopped}
	sizes := []int{16, 24, 32}
	const zoom, pad = 6, 8
	rowH := 32*zoom + pad
	colW := 32*zoom + pad
	sheet := image.NewNRGBA(image.Rect(0, 0, colW*len(states), rowH*len(sizes)*2))
	for bg := 0; bg < 2; bg++ {
		fill := color.NRGBA{243, 243, 243, 255}
		if bg == 1 {
			fill = color.NRGBA{32, 32, 32, 255}
		}
		for si, size := range sizes {
			y0 := (bg*len(sizes) + si) * rowH
			draw.Draw(sheet, image.Rect(0, y0, sheet.Bounds().Dx(), y0+rowH), image.NewUniform(fill), image.Point{}, draw.Src)
			for ci, st := range states {
				img := iconImage(st, size)
				x0 := ci * colW
				for y := 0; y < size*zoom*32/size; y++ {
					for x := 0; x < size*zoom*32/size; x++ {
						c := img.NRGBAAt(x/(zoom*32/size), y/(zoom*32/size))
						if c.A == 0 {
							continue
						}
						a := float64(c.A) / 255
						base := sheet.NRGBAAt(x0+x, y0+y)
						sheet.SetNRGBA(x0+x, y0+y, color.NRGBA{
							uint8(float64(c.R)*a + float64(base.R)*(1-a)),
							uint8(float64(c.G)*a + float64(base.G)*(1-a)),
							uint8(float64(c.B)*a + float64(base.B)*(1-a)), 255})
					}
				}
			}
		}
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, sheet); err != nil {
		t.Fatal(err)
	}
}
