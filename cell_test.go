package main

import (
	"fmt"
	"testing"
)

func TestCellName(t *testing.T) {
	cases := []struct {
		row, col int
		want     string
	}{
		{0, 0, "A1"},
		{0, 1, "B1"},
		{1, 0, "A2"},
		{0, 26, "AA1"},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("case_%02d", i+1), func(t *testing.T) {
			got := cellName(c.row, c.col)
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
