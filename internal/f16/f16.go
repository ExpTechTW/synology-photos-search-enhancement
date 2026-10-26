// Package f16：向量在資料庫裡存成 little-endian 的 IEEE 754 半精度（跟 Python 的 struct "<e" 相同）。
package f16

import (
	"encoding/binary"
	"math"
)

// Encode 把向量轉成 float16 bytes（四捨六入五成雙，跟 Python 相同）。
func Encode(v []float64) []byte {
	out := make([]byte, 2*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint16(out[2*i:], fromFloat64(x))
	}
	return out
}

// Decode 把 float16 bytes 轉回 float32。
func Decode(b []byte) []float32 {
	out := make([]float32, len(b)/2)
	for i := range out {
		out[i] = toFloat32(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}

// DecodeInto 解碼前 len(dst) 個值到 dst（不另外配置記憶體）。
func DecodeInto(dst []float32, b []byte) {
	for i := range dst {
		dst[i] = toFloat32(binary.LittleEndian.Uint16(b[2*i:]))
	}
}

// fromFloat64 直接從 float64 四捨六入五成雙到 float16（不經過 float32，避免兩次捨入跟 Python 不同）。
func fromFloat64(x float64) uint16 {
	bits := math.Float64bits(x)
	sign := uint16(bits>>48) & 0x8000
	exp := int((bits >> 52) & 0x7ff)
	mant := bits & (1<<52 - 1)
	switch {
	case exp == 0x7ff: // Inf / NaN
		if mant != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	case exp == 0:
		return sign // float64 的次正規數遠小於 float16 能表示的最小值
	}
	e := exp - 1023 + 15
	if e >= 0x1f {
		return sign | 0x7c00 // 太大
	}
	var half, rem, mid uint64
	if e <= 0 {
		// float16 的次正規數：單位是 2^-24
		shift := uint(43 - e)
		if shift > 63 {
			return sign
		}
		m := mant | 1<<52
		half, rem, mid = m>>shift, m&(1<<shift-1), 1<<(shift-1)
	} else {
		half, rem, mid = uint64(e)<<10|mant>>42, mant&(1<<42-1), 1<<41
	}
	if rem > mid || (rem == mid && half&1 == 1) {
		half++ // 進位可能讓指數 +1，甚至變成 Inf，跟 IEEE 一致
	}
	return sign | uint16(half)
}

func toFloat32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x3ff)
	switch exp {
	case 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		// 次正規數
		e := uint32(127 - 15 + 1)
		for mant&0x400 == 0 {
			mant <<= 1
			e--
		}
		mant &= 0x3ff
		return math.Float32frombits(sign | e<<23 | mant<<13)
	case 0x1f:
		return math.Float32frombits(sign | 0x7f800000 | mant<<13)
	}
	return math.Float32frombits(sign | (exp+127-15)<<23 | mant<<13)
}
