package main

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/bobg/go-generics/slices"
)

func sortTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "&", " and ")
	s = strings.ReplaceAll(s, "-", " ")

	// Keep only letters, digits, and whitespace.
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, s)

	f := strings.Fields(s)
	switch f[0] {
	case "a", "the", "an":
		if len(f) == 1 {
			// Unlikely case.
			return f[0]
		}
		f = f[1:]
	}

	n, err := strconv.ParseInt(f[0], 10, 64)
	if err == nil {
		f = slices.ReplaceN(f, 0, 1, intToWords(n)...)
	}

	return strings.Join(f, " ")
}

func intToWords(n int64) []string {
	switch n {
	case 0:
		return []string{"zero"}
	case 1:
		return []string{"one"}
	case 2:
		return []string{"two"}
	case 3:
		return []string{"three"}
	case 4:
		return []string{"four"}
	case 5:
		return []string{"five"}
	case 6:
		return []string{"six"}
	case 7:
		return []string{"seven"}
	case 8:
		return []string{"eight"}
	case 9:
		return []string{"nine"}
	case 10:
		return []string{"ten"}
	case 11:
		return []string{"eleven"}
	case 12:
		return []string{"twelve"}
	case 13:
		return []string{"thirteen"}
	case 14:
		return []string{"fourteen"}
	case 15:
		return []string{"fifteen"}
	case 16:
		return []string{"sixteen"}
	case 17:
		return []string{"seventeen"}
	case 18:
		return []string{"eighteen"}
	case 19:
		return []string{"nineteen"}
	case 20:
		return []string{"twenty"}
	}

	if n < 100 {
		var s string
		switch {
		case n < 30:
			s = "twenty"
		case n < 40:
			s = "thirty"
		case n < 50:
			s = "forty"
		case n < 60:
			s = "fifty"
		case n < 70:
			s = "sixty"
		case n < 80:
			s = "seventy"
		case n < 90:
			s = "eighty"
		default:
			s = "ninety"
		}
		if r := n % 10; r > 0 {
			w := intToWords(r)
			s += "-" + w[0]
		}
		return []string{s}
	}

	if n < 1000 {
		q, r := n/100, n%100 // quotient, remainder
		w := intToWords(q)
		w = append(w, "hundred")
		if r > 0 {
			ww := intToWords(r)
			w = append(w, ww...)
		}
		return w
	}

	// Years.
	if n >= 1100 && n < 3000 && !((n >= 2000) && (n < 2010)) {
		q, r := n/100, n%100
		w := intToWords(q)
		if r < 10 {
			w = append(w, "hundred")
		}
		if r > 0 {
			ww := intToWords(r)
			w = append(w, ww...)
		}
		return w
	}

	if n < 1000000 {
		q, r := n/1000, n%1000
		w := intToWords(q)
		w = append(w, "thousand")
		if r > 0 {
			ww := intToWords(r)
			w = append(w, ww...)
		}
		return w
	}

	if n < 1000000000 {
		q, r := n/1000000, n%1000000
		w := intToWords(q)
		w = append(w, "million")
		if r > 0 {
			ww := intToWords(r)
			w = append(w, ww...)
		}
		return w
	}

	q, r := n/1000000000, n%1000000000
	w := intToWords(q)
	w = append(w, "billion")
	if r > 0 {
		ww := intToWords(r)
		w = append(w, ww...)
	}
	return w
}
