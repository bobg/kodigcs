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
		inp:  "It's Garry Shandling's Show",
		want: "its garry shandlings show",
	}, {
		inp:  "The 40-Year-Old Virgin",
		want: "forty year old virgin",
	}, {
		inp:  "42nd Street",
		want: "forty-second street",
	}, {
		inp:  "The 30th Floor",
		want: "thirtieth floor",
	}, {
		inp:  "The 501st Legion",
		want: "five hundred first legion",
	}, {
		inp:  "The 600th Floor",
		want: "six hundredth floor",
	}, {
		inp:  "350000000 Years of Solitude",
		want: "three hundred fifty million years of solitude",
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
