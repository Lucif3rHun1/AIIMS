package bot

import (
	"testing"
	"time"
)

func TestBookingClosed(t *testing.T) {
	day := time.Date(2026, 10, 7, 6, 0, 0, 0, istLoc)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 7, h, m, 0, 0, istLoc) }

	cases := []struct {
		now  time.Time
		want bool
	}{
		{at(5, 0), false},
		{at(9, 30), false}, // after 06:00 but before noon: still bookable
		{at(11, 59), false},
		{at(12, 0), true},
		{day.AddDate(0, 0, 1), true}, // yesterday's slot
	}
	for _, c := range cases {
		if got := bookingClosed(day, c.now); got != c.want {
			t.Errorf("bookingClosed(%v) = %v, want %v", c.now.Format("02 15:04"), got, c.want)
		}
	}
}
