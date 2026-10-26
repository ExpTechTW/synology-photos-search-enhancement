package f16

import (
	"encoding/hex"
	"math"
	"testing"
)

// 期望值由 Python 的 struct.pack("<e", x) 產生
func TestMatchesPython(t *testing.T) {
	cases := map[float64]string{
		0: "0000", 1: "003c", -2: "00c0", 0.1: "662e", 0.333333: "5535", 65504: "ff7b", 6e-8: "0100",
		5.960464477539063e-08: "0100", 1e-9: "0000", 0.000123: "0808", -0.0731: "aeac", 1.0009765625: "013c",
		1.00048828125: "003c", 1.00146484375: "023c", 2049: "0068", 2051: "0268",
	}
	for x, want := range cases {
		if got := hex.EncodeToString(Encode([]float64{x})); got != want {
			t.Errorf("Encode(%v) = %s, want %s", x, got, want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, x := range []float64{0, 1, -1, 0.5, 0.0001, -0.73, 123.25, 65504} {
		got := float64(Decode(Encode([]float64{x}))[0])
		if math.Abs(got-x) > math.Abs(x)*1e-3+1e-7 {
			t.Errorf("round trip %v → %v", x, got)
		}
	}
	if !math.IsInf(float64(Decode(Encode([]float64{1e6}))[0]), 1) {
		t.Error("太大要變成 Inf")
	}
}
