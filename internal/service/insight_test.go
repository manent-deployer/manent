package service

import (
	"testing"
	"time"
)

func TestParseSinceGiorniAcavalloDellOraLegale(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skip("time zone database not available")
	}
	// Daylight saving time ended on 2026-10-25: 30 calendar days would be 721h.
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, rome)
	got, err := ParseSince("30d", now)
	if err != nil {
		t.Fatal(err)
	}
	if d := now.Sub(got); d != 720*time.Hour {
		t.Errorf("30d = %v, attese 720h", d)
	}
	for _, v := range []string{"0d", "-1d", "ieri", "-5m"} {
		if _, err := ParseSince(v, now); err == nil {
			t.Errorf("%q accettato", v)
		}
	}
}
