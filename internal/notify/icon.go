package notify

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"math"
)

// The tray icon is a cloud with a symbol inside, drawn here at whatever size
// the display asks for, so CloudSync needs no image files:
//
//	in sync    green cloud, tick
//	working    blue cloud, two circling arrows
//	attention  amber cloud, "!"   (offline, slowed down, retrying)
//	error      red cloud, cross
//	paused     grey cloud, two bars
//	stopped    hollow grey cloud, stop square
//
// A thin white edge keeps the coloured clouds readable on a dark taskbar.

// All shapes are laid out on a 32 x 32 grid and scaled to the icon size.
const grid = 32.0

type vec struct{ x, y float64 }

func (a vec) sub(b vec) vec { return vec{a.x - b.x, a.y - b.y} }
func (a vec) len() float64  { return math.Hypot(a.x, a.y) }

func circleSD(p, c vec, r float64) float64 { return p.sub(c).len() - r }

func boxSD(p, c, half vec) float64 {
	q := vec{math.Abs(p.x-c.x) - half.x, math.Abs(p.y-c.y) - half.y}
	return vec{math.Max(q.x, 0), math.Max(q.y, 0)}.len() + math.Min(math.Max(q.x, q.y), 0)
}

// segDist is the distance from p to the segment a-b.
func segDist(p, a, b vec) float64 {
	ab, ap := b.sub(a), p.sub(a)
	t := math.Max(0, math.Min(1, (ap.x*ab.x+ap.y*ab.y)/(ab.x*ab.x+ab.y*ab.y)))
	return p.sub(vec{a.x + ab.x*t, a.y + ab.y*t}).len()
}

// cloudSD is the signed distance to the cloud: negative inside.
func cloudSD(p vec) float64 {
	d := circleSD(p, vec{9, 20}, 6.5)
	d = math.Min(d, circleSD(p, vec{16, 14}, 8))
	d = math.Min(d, circleSD(p, vec{23.5, 19.5}, 7))
	return math.Min(d, boxSD(p, vec{16.25, 22.75}, vec{7.25, 3.75}))
}

// glyphCentre is where the symbol sits: a little below the cloud's middle,
// where it is widest.
var glyphCentre = vec{16.5, 18}

func inTriangle(p, a, b, c vec) bool {
	sign := func(p1, p2, p3 vec) float64 { return (p1.x-p3.x)*(p2.y-p3.y) - (p2.x-p3.x)*(p1.y-p3.y) }
	d1, d2, d3 := sign(p, a, b), sign(p, b, c), sign(p, c, a)
	neg := d1 < 0 || d2 < 0 || d3 < 0
	pos := d1 > 0 || d2 > 0 || d3 > 0
	return !(neg && pos)
}

// syncGlyph is two arrows chasing each other round a circle.
func syncGlyph(q vec) bool {
	const radius, half = 4.7, 1.25
	r := q.len()
	ang := math.Atan2(q.y, q.x)
	for k := 0; k < 2; k++ {
		start := -2.45 + float64(k)*math.Pi // 1.75 radians (about 100 degrees) of arc, then a gap
		end := start + 1.75
		// Angle of q measured from the arc's start, wrapped to 0..2pi.
		a := math.Mod(ang-start+4*math.Pi, 2*math.Pi)
		if math.Abs(r-radius) <= half && a <= end-start {
			return true
		}
		// The arrowhead at the end, pointing the way the arc runs.
		at := vec{radius * math.Cos(end), radius * math.Sin(end)}
		tangent := vec{-math.Sin(end), math.Cos(end)}
		normal := vec{math.Cos(end), math.Sin(end)}
		tip := vec{at.x + tangent.x*3.6, at.y + tangent.y*3.6}
		b1 := vec{at.x + normal.x*2.9, at.y + normal.y*2.9}
		b2 := vec{at.x - normal.x*2.9, at.y - normal.y*2.9}
		if inTriangle(q, b1, b2, tip) {
			return true
		}
	}
	return false
}

// glyph reports whether the symbol for st covers q, a point relative to the
// symbol's centre.
func glyph(st State, q vec) bool {
	switch st {
	case StateOK:
		return math.Min(segDist(q, vec{-5, 0.5}, vec{-1.6, 3.9}), segDist(q, vec{-1.6, 3.9}, vec{5.6, -4.4})) <= 1.45
	case StateBusy:
		return syncGlyph(q)
	case StateAttention:
		return segDist(q, vec{0, -5.6}, vec{0, 1.2}) <= 1.5 || circleSD(q, vec{0, 5}, 1.8) <= 0
	case StateError:
		return math.Min(segDist(q, vec{-4.2, -4.2}, vec{4.2, 4.2}), segDist(q, vec{-4.2, 4.2}, vec{4.2, -4.2})) <= 1.4
	case StatePaused:
		return boxSD(q, vec{-3.2, 0}, vec{1.5, 5}) <= 0 || boxSD(q, vec{3.2, 0}, vec{1.5, 5}) <= 0
	default: // StateStopped
		return boxSD(q, vec{0, 0}, vec{4, 4}) <= 0
	}
}

func fillColour(st State) color.NRGBA {
	switch st {
	case StateOK:
		return color.NRGBA{38, 166, 91, 255}
	case StateBusy:
		return color.NRGBA{36, 120, 235, 255}
	case StateAttention:
		return color.NRGBA{232, 146, 0, 255}
	case StateError:
		return color.NRGBA{214, 48, 49, 255}
	case StatePaused:
		return color.NRGBA{120, 128, 140, 255}
	}
	return color.NRGBA{130, 138, 150, 255} // stopped
}

// sample is the colour of the icon at p (grid units), alpha 0 where nothing is.
func sample(st State, p vec) color.NRGBA {
	white := color.NRGBA{255, 255, 255, 255}
	sd := cloudSD(p)
	if st == StateStopped {
		// A hollow cloud: just its outline, with the square inside.
		if glyph(st, p.sub(glyphCentre)) || (sd <= 0 && sd >= -2.6) {
			return fillColour(st)
		}
		return color.NRGBA{}
	}
	switch {
	case sd <= 0 && glyph(st, p.sub(glyphCentre)):
		return white
	case sd <= 0:
		return fillColour(st)
	case sd <= 1.3:
		return white // the edge
	}
	return color.NRGBA{}
}

// iconImage draws the icon for st, size pixels square.
func iconImage(st State, size int) *image.NRGBA {
	const n = 4 // n x n samples per pixel, for smooth edges
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := grid / float64(size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for j := 0; j < n; j++ {
				for i := 0; i < n; i++ {
					p := vec{(float64(x) + (float64(i)+0.5)/n) * scale, (float64(y) + (float64(j)+0.5)/n) * scale}
					c := sample(st, p)
					al := float64(c.A) / 255
					r, g, b, a = r+float64(c.R)*al, g+float64(c.G)*al, b+float64(c.B)*al, a+al
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(a / (n * n) * 255)})
		}
	}
	return img
}

// iconBits returns the icon for st as an icon image resource (a bitmap
// header, 32-bit colour rows from the bottom up, then an all-clear AND
// mask), which Windows turns into an icon with CreateIconFromResourceEx.
func iconBits(st State, size int) []byte {
	img := iconImage(st, size)
	pixels := make([]byte, size*size*4)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			c := img.NRGBAAt(x, y)
			i := ((size-1-y)*size + x) * 4 // bitmaps are stored bottom row first
			pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = c.B, c.G, c.R, c.A
		}
	}
	maskRow := (size + 31) / 32 * 4 // 1 bit per pixel, rows padded to 4 bytes

	var buf bytes.Buffer
	w := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }
	w(uint32(40))          // BITMAPINFOHEADER size
	w(int32(size))         // width
	w(int32(size * 2))     // height: colour rows plus mask rows
	w(uint16(1))           // planes
	w(uint16(32))          // bits per pixel
	w(uint32(0))           // BI_RGB
	w(uint32(len(pixels))) // image size
	w([4]int32{})          // resolution and palette sizes
	buf.Write(pixels)
	buf.Write(make([]byte, maskRow*size)) // AND mask: all clear, the alpha channel decides
	return buf.Bytes()
}
