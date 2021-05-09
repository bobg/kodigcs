package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/time/rate"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

func handleSheet(sheetsSvc *sheets.SpreadsheetsService, sheetID string, f func(rownum int, headings []string, name string, row []interface{}) error) error {
	resp, err := sheetsSvc.Values.Get(sheetID, "Sheet1!A:Z").Do()
	if err != nil {
		return errors.Wrap(err, "reading spreadsheet data")
	}
	if len(resp.Values) < 2 {
		return fmt.Errorf("got %d spreadsheet row(s), want 2 or more", len(resp.Values))
	}

	var headings []string
	for _, rawheading := range resp.Values[0] {
		if heading, ok := rawheading.(string); ok {
			headings = append(headings, strings.ToLower(heading))
		} else {
			headings = append(headings, "")
		}
	}

	for i := 1; i < len(resp.Values); i++ {
		row := resp.Values[i]
		if len(row) < 2 {
			continue
		}

		name, ok := row[0].(string)
		if !ok {
			continue
		}

		err = f(i, headings, name, row)
		if err != nil {
			return errors.Wrapf(err, "processing spreadsheet row %d (%s)", i, name)
		}
	}

	return nil
}

func updateSpreadsheet(ctx context.Context, credsFile, sheetID string) error {
	svc, err := sheets.NewService(ctx, option.WithCredentialsFile(credsFile), option.WithScopes(sheets.SpreadsheetsScope))
	if err != nil {
		return errors.Wrap(err, "creating sheets service")
	}

	var (
		httpLimiter = rate.NewLimiter(rate.Every(time.Second), 1)
		ssLimiter   = rate.NewLimiter(rate.Every(time.Second), 1)
	)

	cl := &http.Client{
		Transport: &limitedTransport{
			limiter:   httpLimiter,
			transport: http.DefaultTransport,
		},
	}

	ssSet := func(cell, val string) error {
		err := ssLimiter.Wait(ctx)
		if err != nil {
			return errors.Wrap(err, "waiting for ssLimiter")
		}
		vr := &sheets.ValueRange{
			Range:  cell,
			Values: [][]interface{}{{val}},
		}
		_, err = svc.Spreadsheets.Values.Update(sheetID, cell, vr).Context(ctx).ValueInputOption("RAW").Do()
		return errors.Wrap(err, "updating cell %s in spreadsheet")
	}

	return handleSheet(svc.Spreadsheets, sheetID, func(rownum int, headings []string, name string, row []interface{}) error {
		var (
			id         string
			needLookup bool
		)
		for j, heading := range headings {
			if j >= len(row) {
				break
			}
			if heading != "imdbid" {
				continue
			}
			val, ok := row[j].(string)
			if !ok {
				return nil
			}
			id = parseIMDbID(val)
		}

		if id == "" {
			return nil
		}

		for j, heading := range headings {
			switch heading {
			case "actors", "directors", "genre", "poster", "year":
				if j >= len(row) {
					needLookup = true
				} else {
					rawval := row[j]
					val, ok := rawval.(string)
					if !ok {
						continue
					}
					val = strings.TrimSpace(val)
					if val == "" {
						needLookup = true
					}
				}
			}
		}

		if !needLookup {
			return nil
		}

		imdbInfo, err := parseIMDbPage(cl, id)
		if err != nil {
			return errors.Wrapf(err, "getting IMDb info for %s (id %s)", name, id)
		}

		for j, heading := range headings {
			if j == 0 {
				continue
			}
			var val string
			if j < len(row) {
				var ok bool
				val, ok = row[j].(string)
				if !ok {
					continue
				}
			}
			val = strings.TrimSpace(val)
			if val != "" {
				continue
			}

			cell := cellName(rownum, j)

			switch heading {
			case "actors":
				newval := strings.Join(imdbInfo.Actors, "; ")
				err = ssSet(cell, newval)
				if err != nil {
					return errors.Wrapf(err, "setting %s to %s", cell, newval)
				}

			case "directors":
				newval := strings.Join(imdbInfo.Directors, "; ")
				err = ssSet(cell, newval)
				if err != nil {
					return errors.Wrapf(err, "setting %s to %s", cell, newval)
				}

			case "genre":
				newval := strings.Join(imdbInfo.Genres, "; ")
				err = ssSet(cell, newval)
				if err != nil {
					return errors.Wrapf(err, "setting %s to %s", cell, newval)
				}

			case "poster":
				err = ssSet(cell, imdbInfo.Image)
				if err != nil {
					return errors.Wrapf(err, "setting %s to %s", cell, imdbInfo.Image)
				}

			case "year":
				parts := strings.Split(imdbInfo.DatePublished, "-")
				if len(parts) != 3 {
					continue
				}
				err = ssSet(cell, parts[0])
				if err != nil {
					return errors.Wrapf(err, "setting %s to %s", cell, parts[0])
				}
			}
		}

		return nil
	})
}

// Row and col are both zero-based.
func cellName(row, col int) string {
	return fmt.Sprintf("%s%d", colName(col), row+1)
}

func colName(col int) string {
	if col < 26 {
		return string(byte(col) + 'A')
	}
	return colName(col/26-1) + colName(col%26)
}
