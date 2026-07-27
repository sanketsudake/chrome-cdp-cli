package chrome

import (
	"testing"
	"time"
)

func TestClampConsentTimeout(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, DefaultConsentTimeout},
		{-1, DefaultConsentTimeout},
		{-time.Hour, DefaultConsentTimeout},
		{time.Millisecond, MinConsentTimeout},
		{MinConsentTimeout, MinConsentTimeout},
		{45 * time.Second, 45 * time.Second},
		{MaxConsentTimeout, MaxConsentTimeout},
		{8760 * time.Hour, MaxConsentTimeout},
	} {
		if got := ClampConsentTimeout(c.in); got != c.want {
			t.Errorf("ClampConsentTimeout(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
