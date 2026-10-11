package labels

import "testing"

func TestSelettori(t *testing.T) {
	eu1a := map[string]string{"region": "eu-1", "az": "eu-1a", "canary": "true"}
	us1b := map[string]string{"region": "us-1", "az": "us-1b"}
	for _, tt := range []struct {
		sel        string
		eu1a, us1b bool
	}{
		{"region=eu-1", true, false},
		{"region = eu-1, az=eu-1a, canary=true", true, false},
		{"region in (eu-2, us-1)", false, true},
		{"region notin (eu-2, us-1)", true, false},
		{"canary!=true", false, true},
		{"region in (eu-1,us-1), az!=eu-1a", false, true},
	} {
		s, err := Parse(tt.sel)
		if err != nil {
			t.Fatalf("%s: %v", tt.sel, err)
		}
		if s.Matches(eu1a) != tt.eu1a || s.Matches(us1b) != tt.us1b {
			t.Errorf("%s: eu1a=%v us1b=%v", tt.sel, s.Matches(eu1a), s.Matches(us1b))
		}
	}
	for _, bad := range []string{"", "region", "region in eu-1", "region in ()", "re gion=x", "region=a b"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accettato", bad)
		}
	}
	if ValidLabels(map[string]string{"region": "eu 1"}) == nil {
		t.Error("etichetta con spazio accettata")
	}
}
