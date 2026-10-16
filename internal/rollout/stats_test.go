package rollout

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestMannWhitney(t *testing.T) {
	// Reference value (scipy.stats.mannwhitneyu, method="asymptotic"): U=0, p=0.01219.
	u, p := MannWhitney([]float64{1, 2, 3, 4, 5}, []float64{6, 7, 8, 9, 10})
	if u != 0 || math.Abs(p-0.01219) > 0.0001 {
		t.Errorf("U=%g p=%g", u, p)
	}
	// U equals the count of pairs with x > y (ties count 1/2), and the test is symmetric.
	r := rand.New(rand.NewPCG(1, 2))
	for range 50 {
		x, y := make([]float64, 3+r.IntN(20)), make([]float64, 3+r.IntN(20))
		for i := range x {
			x[i] = float64(r.IntN(6))
		}
		for i := range y {
			y[i] = float64(r.IntN(6))
		}
		want := 0.0
		for _, a := range x {
			for _, b := range y {
				switch {
				case a > b:
					want++
				case a == b:
					want += 0.5
				}
			}
		}
		u, p := MannWhitney(x, y)
		_, q := MannWhitney(y, x)
		if math.Abs(u-want) > 1e-9 || math.Abs(p-q) > 1e-12 || p < 0 || p > 1 {
			t.Fatalf("x=%v y=%v: U=%g atteso %g, p=%g/%g", x, y, u, want, p, q)
		}
	}
	// Identical samples: no difference.
	if _, p := MannWhitney([]float64{0, 0, 0}, []float64{0, 0, 0, 0}); p != 1 {
		t.Errorf("campioni identici: p=%g", p)
	}
	if ratio(0, 0) != 1 || !math.IsInf(ratio(1, 0), 1) || ratio(2, 4) != 0.5 {
		t.Error("ratio")
	}
}
