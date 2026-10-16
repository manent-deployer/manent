package rollout

import (
	"math"
	"slices"
)

// MannWhitney runs the two-sided Mann-Whitney U test on the samples x and y with the normal
// approximation, the tie correction and the continuity correction (as scipy's mannwhitneyu
// with method="asymptotic"). It returns U of x and the p-value; with identical samples (no
// variance) the p-value is 1.
func MannWhitney(x, y []float64) (u, p float64) {
	n1, n2 := float64(len(x)), float64(len(y))
	if n1 == 0 || n2 == 0 {
		return 0, 1
	}
	type obs struct {
		v float64
		x bool
	}
	all := make([]obs, 0, len(x)+len(y))
	for _, v := range x {
		all = append(all, obs{v, true})
	}
	for _, v := range y {
		all = append(all, obs{v, false})
	}
	slices.SortFunc(all, func(a, b obs) int {
		switch {
		case a.v < b.v:
			return -1
		case a.v > b.v:
			return 1
		}
		return 0
	})
	// Average ranks of the ties; ties accumulates t³-t for the variance correction.
	var r1, ties float64
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].v == all[i].v {
			j++
		}
		rank := float64(i+j+1) / 2
		for k := i; k < j; k++ {
			if all[k].x {
				r1 += rank
			}
		}
		t := float64(j - i)
		ties += t*t*t - t
		i = j
	}
	u = r1 - n1*(n1+1)/2
	n := n1 + n2
	mean := n1 * n2 / 2
	variance := n1 * n2 / 12 * ((n + 1) - ties/(n*(n-1)))
	if variance <= 0 {
		return u, 1
	}
	d := math.Abs(u-mean) - 0.5
	if d < 0 {
		d = 0
	}
	z := d / math.Sqrt(variance)
	return u, math.Min(1, math.Erfc(z/math.Sqrt2))
}

// mean returns the arithmetic mean of v.
func mean(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// ratio returns candidate/stable; with a zero stable value 1 if both are zero, +Inf otherwise.
func ratio(candidate, stable float64) float64 {
	switch {
	case stable != 0:
		return candidate / stable
	case candidate == 0:
		return 1
	}
	return math.Inf(int(math.Copysign(1, candidate)))
}
