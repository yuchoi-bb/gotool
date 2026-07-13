// genicon 은 gotool 아이콘(icon.ico, icon-preview.png)을 생성한다.
// 디자인: 보라→핫핑크 대각 그라데이션의 둥근 사각형 + 광택 + 흰 번개.
//
//	go run ./tools/genicon
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

type vec struct{ x, y float64 }

// 번개 폴리곤(0..1 좌표)
var bolt = []vec{
	{0.60, 0.085},
	{0.26, 0.585},
	{0.445, 0.585},
	{0.395, 0.915},
	{0.75, 0.405},
	{0.545, 0.405},
}

func inPoly(p vec, poly []vec) bool {
	in := false
	j := len(poly) - 1
	for i := 0; i < len(poly); i++ {
		a, b := poly[i], poly[j]
		if (a.y > p.y) != (b.y > p.y) &&
			p.x < (b.x-a.x)*(p.y-a.y)/(b.y-a.y)+a.x {
			in = !in
		}
		j = i
	}
	return in
}

// 둥근 사각형(0..1 좌표, margin/radius 포함) 내부 여부
func inRoundRect(p vec, margin, radius float64) bool {
	lo, hi := margin, 1-margin
	if p.x < lo || p.x > hi || p.y < lo || p.y > hi {
		return false
	}
	rx := math.Max(math.Abs(p.x-0.5)-(0.5-margin-radius), 0)
	ry := math.Max(math.Abs(p.y-0.5)-(0.5-margin-radius), 0)
	return rx*rx+ry*ry <= radius*radius
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

type fcolor struct{ r, g, b, a float64 }

func over(dst, src fcolor) fcolor {
	oa := src.a + dst.a*(1-src.a)
	if oa == 0 {
		return fcolor{}
	}
	return fcolor{
		r: (src.r*src.a + dst.r*dst.a*(1-src.a)) / oa,
		g: (src.g*src.a + dst.g*dst.a*(1-src.a)) / oa,
		b: (src.b*src.a + dst.b*dst.a*(1-src.a)) / oa,
		a: oa,
	}
}

// sample 은 (0..1) 좌표의 색을 계산한다.
func sample(p vec) fcolor {
	const margin, radius = 0.02, 0.21
	if !inRoundRect(p, margin, radius) {
		return fcolor{}
	}

	// 대각 그라데이션: 보라(#7C3AED) → 핫핑크(#EC4899) → 살짝 오렌지 기운(#FB5F3C 방향)
	t := clamp01((p.x + p.y) / 2)
	var r, g, b float64
	if t < 0.62 {
		u := t / 0.62
		r = lerp(124, 236, u)
		g = lerp(58, 72, u)
		b = lerp(237, 153, u)
	} else {
		u := (t - 0.62) / 0.38
		r = lerp(236, 249, u)
		g = lerp(72, 92, u)
		b = lerp(153, 98, u)
	}
	c := fcolor{r / 255, g / 255, b / 255, 1}

	// 상단 광택
	if p.y < 0.5 {
		gl := 0.20 * math.Pow(1-p.y/0.5, 1.6)
		c.r = lerp(c.r, 1, gl)
		c.g = lerp(c.g, 1, gl)
		c.b = lerp(c.b, 1, gl)
	}
	// 하단 깊이
	if p.y > 0.72 {
		dk := 0.14 * clamp01((p.y-0.72)/0.28)
		c.r = lerp(c.r, 0, dk)
		c.g = lerp(c.g, 0, dk)
		c.b = lerp(c.b, 0, dk)
	}

	// 번개 그림자
	if inPoly(vec{p.x - 0.012, p.y - 0.022}, bolt) {
		c = over(c, fcolor{0, 0, 0, 0.30})
	}
	// 번개(흰색, 아래로 갈수록 살짝 핑크빛)
	if inPoly(p, bolt) {
		w := clamp01((p.y - 0.1) / 0.85)
		c = over(c, fcolor{1, lerp(1, 0.93, w), lerp(1, 0.96, w), 1})
	}
	return c
}

// render 는 size×size RGBA 이미지를 슈퍼샘플링으로 그린다.
func render(size int) *image.RGBA {
	ss := 5
	if size >= 128 {
		ss = 3
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					p := vec{
						(float64(x) + (float64(sx)+0.5)/float64(ss)) / float64(size),
						(float64(y) + (float64(sy)+0.5)/float64(ss)) / float64(size),
					}
					c := sample(p)
					r += c.r * c.a
					g += c.g * c.a
					b += c.b * c.a
					a += c.a
				}
			}
			n := float64(ss * ss)
			if a > 0 {
				img.SetRGBA(x, y, color.RGBA{
					R: uint8(r / a * 255),
					G: uint8(g / a * 255),
					B: uint8(b / a * 255),
					A: uint8(a / n * 255),
				})
			}
		}
	}
	return img
}

// bmpEntry 는 ICO 안에 넣을 32bpp BMP(DIB) 바이트를 만든다.
func bmpEntry(img *image.RGBA) []byte {
	w := img.Rect.Dx()
	h := img.Rect.Dy()
	var buf bytes.Buffer
	hdr := struct {
		Size         uint32
		Width        int32
		Height       int32 // 이미지+AND 마스크라 2배
		Planes       uint16
		BitCount     uint16
		Compression  uint32
		SizeImage    uint32
		XPels, YPels int32
		ClrUsed      uint32
		ClrImportant uint32
	}{40, int32(w), int32(h * 2), 1, 32, 0, uint32(w * h * 4), 0, 0, 0, 0}
	binary.Write(&buf, binary.LittleEndian, hdr)
	// 픽셀(BGRA, 아래에서 위로)
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.RGBAAt(x, y)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	// AND 마스크(전부 0 = 알파 사용), 행은 32비트 정렬
	rowBytes := ((w + 31) / 32) * 4
	mask := make([]byte, rowBytes*h)
	buf.Write(mask)
	return buf.Bytes()
}

func main() {
	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	type img struct {
		size int
		data []byte
	}
	var imgs []img
	for _, s := range sizes {
		m := render(s)
		if s == 256 {
			var b bytes.Buffer
			png.Encode(&b, m)
			imgs = append(imgs, img{s, b.Bytes()})
		} else {
			imgs = append(imgs, img{s, bmpEntry(m)})
		}
	}

	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&out, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&out, binary.LittleEndian, uint16(len(imgs)))
	offset := 6 + 16*len(imgs)
	for _, im := range imgs {
		b := byte(im.size)
		if im.size >= 256 {
			b = 0
		}
		out.WriteByte(b)                                    // width
		out.WriteByte(b)                                    // height
		out.WriteByte(0)                                    // colors
		out.WriteByte(0)                                    // reserved
		binary.Write(&out, binary.LittleEndian, uint16(1))  // planes
		binary.Write(&out, binary.LittleEndian, uint16(32)) // bitcount
		binary.Write(&out, binary.LittleEndian, uint32(len(im.data)))
		binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(im.data)
	}
	for _, im := range imgs {
		out.Write(im.data)
	}

	if err := os.WriteFile("icon.ico", out.Bytes(), 0o644); err != nil {
		panic(err)
	}
	// 미리보기 PNG(256px)
	preview := render(256)
	var pb bytes.Buffer
	png.Encode(&pb, preview)
	if err := os.WriteFile("icon-preview.png", pb.Bytes(), 0o644); err != nil {
		panic(err)
	}
}
