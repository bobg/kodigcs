package main

import (
	"fmt"
	"testing"
)

func TestSortTitle(t *testing.T) {
	cases := []struct {
		inp, want string
	}{{
		inp:  "The Gumball Rally",
		want: "gumball rally",
	}, {
		inp:  "1917",
		want: "nineteen seventeen",
	}, {
		inp:  "9 to 5",
		want: "nine to 5",
	}, {
		inp: "It's Garry Shandling's Show",
		want: "its garry shandlings show",
	}}

	for i, tc := range cases {
		t.Run(fmt.Sprintf("%02d", i+1), func(t *testing.T) {
			got := sortTitle(tc.inp)
			if got != tc.want {
				t.Errorf(`input "%s", got "%s", want "%s"`, tc.inp, got, tc.want)
			}
		})
	}
}
