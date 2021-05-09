package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bobg/basexx"
	"github.com/bobg/htree"
	"github.com/pkg/errors"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
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

func (s *server) updateSpreadsheet(ctx context.Context) error {
	svc, err := sheets.NewService(ctx, option.WithCredentialsFile(s.credsFile), option.WithScopes(sheets.SpreadsheetsScope))
	if err != nil {
		return errors.Wrap(err, "creating sheets service")
	}

	cl := http.Client{
		Transport: &limitedTransport{
			limiter:   rate.NewLimiter(rate.Every(time.Second), 1),
			transport: http.DefaultTransport,
		},
	}

	return handleSheet(svc.Spreadsheets, s.sheetID, func(rownum int, headings []string, name string, row []interface{}) error {
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
			if m := imdbRE.FindStringSubmatch(val); len(m) > 1 {
				id = m[1]
			} else {
				id = val
			}
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
		return func() error { // introduces new defer context
			titleURL := fmt.Sprintf("https://www.imdb.com/title/%s/", id)

			req, err := http.NewRequest("GET", titleURL, nil)
			if err != nil {
				return errors.Wrapf(err, "building request to GET %s", titleURL)
			}

			log.Printf("Getting %s for %s (row %d)", titleURL, name, rownum+1)
			resp, err := cl.Do(req)
			if err != nil {
				return errors.Wrapf(err, "getting %s", titleURL)
			}
			defer resp.Body.Close()

			doc, err := html.Parse(resp.Body)
			if err != nil {
				return errors.Wrapf(err, "parsing response from %s", titleURL)
			}

			headEl := htree.FindEl(doc, func(n *html.Node) bool {
				return n.DataAtom == atom.Head
			})
			if headEl == nil {
				return fmt.Errorf("no HEAD in response from %s", titleURL)
			}
			jsonEl := htree.FindEl(headEl, func(n *html.Node) bool {
				return n.DataAtom == atom.Script && htree.ElAttr(n, "type") == "application/ld+json"
			})
			if jsonEl == nil {
				return fmt.Errorf("no info JSON in response from %s", titleURL)
			}

			var parsed struct {
				Name          string          `json:"name"`
				Image         string          `json:"image"`
				Genre         json.RawMessage `json:"genre"`    // string or []string
				Actor         json.RawMessage `json:"actor"`    // person or []person
				Director      json.RawMessage `json:"director"` // person or []person
				Description   string          `json:"description"`
				DatePublished string          `json:"datePublished"`
				Duration      string          `json:"duration"`
			}

			jsonBuf := new(bytes.Buffer)
			for child := jsonEl.FirstChild; child != nil; child = child.NextSibling {
				jsonBuf.WriteString(child.Data)
			}

			err = json.Unmarshal(jsonBuf.Bytes(), &parsed)
			if err != nil {
				return errors.Wrapf(err, "unmarshaling JSON in response from %s", titleURL)
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
					names, err := parsePersons(parsed.Actor)
					if err != nil {
						return errors.Wrap(err, "parsing actor")
					}
					newval := strings.Join(names, "; ")
					err = s.ssSet(ctx, svc.Spreadsheets, cell, newval)
					if err != nil {
						return errors.Wrapf(err, "setting %s to %s", cell, newval)
					}

				case "directors":
					names, err := parsePersons(parsed.Director)
					if err != nil {
						return errors.Wrap(err, "parsing director")
					}
					newval := strings.Join(names, "; ")
					err = s.ssSet(ctx, svc.Spreadsheets, cell, newval)
					if err != nil {
						return errors.Wrapf(err, "setting %s to %s", cell, newval)
					}

				case "genre":
					var (
						genre  string
						genres []string
					)
					err = json.Unmarshal(parsed.Genre, &genre)
					if err != nil {
						err = json.Unmarshal(parsed.Genre, &genres)
						if err != nil {
							return fmt.Errorf("could not parse genre %s", string(parsed.Genre))
						}
					} else {
						genres = []string{genre}
					}
					newval := strings.Join(genres, "; ")
					err = s.ssSet(ctx, svc.Spreadsheets, cell, newval)
					if err != nil {
						return errors.Wrapf(err, "setting %s to %s", cell, newval)
					}

				case "poster":
					err = s.ssSet(ctx, svc.Spreadsheets, cell, parsed.Image)
					if err != nil {
						return errors.Wrapf(err, "setting %s to %s", cell, parsed.Image)
					}

				case "year":
					parts := strings.Split(parsed.DatePublished, "-")
					if len(parts) != 3 {
						continue
					}
					err = s.ssSet(ctx, svc.Spreadsheets, cell, parts[0])
					if err != nil {
						return errors.Wrapf(err, "setting %s to %s", cell, parts[0])
					}
				}
			}

			return nil
		}()
	})
}

func (s *server) ssSet(ctx context.Context, sheetsSvc *sheets.SpreadsheetsService, cell, val string) error {
	err := s.limiter.Wait(ctx)
	if err != nil {
		return err
	}
	vr := &sheets.ValueRange{
		Range:  cell,
		Values: [][]interface{}{{val}},
	}
	_, err = sheetsSvc.Values.Update(s.sheetID, cell, vr).Context(ctx).ValueInputOption("RAW").Do()
	return err
}

// Row and col are both zero-based.
func cellName(row, col int) string {
	colstr := strconv.Itoa(col)
	src := basexx.NewBuffer([]byte(colstr), basexx.Alnum(10))
	dest := basexx.NewBuffer(make([]byte, basexx.Length(10, 26, len(colstr))), base26{})
	basexx.Convert(dest, src) // ignore errors

	return fmt.Sprintf("%s%d", string(dest.Written()), row+1)
}
