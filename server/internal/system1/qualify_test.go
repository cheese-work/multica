package system1

import (
	"testing"
	"time"
)

func TestStatus(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name                string
		age                 time.Duration
		denominator, agreed int64
		demoted             bool
		want                string
	}{
		{"bar met", 7 * day, 20, 18, false, StatusQualified},
		{"just under 90%", 7 * day, 20, 17, false, StatusCollecting},
		{"too young", 6 * day, 20, 20, false, StatusCollecting},
		{"19 cases", 10 * day, 19, 19, false, StatusCollecting},
		{"one case from 19 abstentions", 10 * day, 1, 1, false, StatusCollecting},
		{"under 20 after 4 weeks", 28 * day, 19, 19, false, StatusInsufficientData},
		{"20 cases after 4 weeks but weak", 30 * day, 20, 10, false, StatusCollecting},
		{"demoted wins", 30 * day, 20, 20, true, StatusDemoted},
	}
	for _, c := range cases {
		if got := Status(c.age, c.denominator, c.agreed, c.demoted); got != c.want {
			t.Errorf("%s: Status = %s, want %s", c.name, got, c.want)
		}
	}
}
