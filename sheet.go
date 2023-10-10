package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/pkg/errors"
	"golang.org/x/time/rate"
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

func updateSpreadsheet(ctx context.Context, ssvc *sheets.SpreadsheetsService, bucket *storage.BucketHandle, htmldir, sheetID string) error {
	var (
		httpLimiter = rate.NewLimiter(rate.Every(10*time.Second), 1)
		ssLimiter   = rate.NewLimiter(rate.Every(time.Second), 1)
	)

	cl := &http.Client{
		Transport: &limitedTransport{
			limiter:   httpLimiter,
			transport: http.DefaultTransport,
		},
	}

	ssSet := func(cell, val string) error {
		if err := ssLimiter.Wait(ctx); err != nil {
			return errors.Wrap(err, "waiting for ssLimiter")
		}
		vr := &sheets.ValueRange{
			Range:  cell,
			Values: [][]interface{}{{val}},
		}
		_, err := ssvc.Values.Update(sheetID, cell, vr).Context(ctx).ValueInputOption("RAW").Do()
		return errors.Wrap(err, "updating cell %s in spreadsheet")
	}

	return handleSheet(ssvc, sheetID, func(rownum int, headings []string, name string, row []interface{}) error {
		var needLookup bool
		for j, heading := range headings {
			switch heading {
			case "actors", "directors", "genre", "poster", "year", "plot", "runtime":
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

		var (
			info *imdbInfo
			err  error
		)

		if htmldir != "" {
			filename := filepath.Join(htmldir, name+".html")
			f, err := os.Open(filename)
			if errors.Is(err, fs.ErrNotExist) {
				// ok
			} else if err != nil {
				return errors.Wrapf(err, "opening %s", filename)
			} else {
				defer f.Close()

				log.Printf("Getting IMDb info for %s from %s...\n", name, filename)

				info, err = parseIMDbHTML(f)
				if err != nil {
					return errors.Wrapf(err, "parsing %s", filename)
				}
			}
		}

		if info == nil {
			var id string
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

			log.Printf("Getting IMDb info for %s...", name)

			info, err = parseIMDbPage(cl, id)
			if err != nil {
				return errors.Wrapf(err, "getting IMDb info for %s (id %s)", name, id)
			}
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
				newval := strings.Join(info.Actors, "; ")
				err = ssSet(cell, newval)
				if err != nil {
					return errors.Wrapf(err, "setting %s to %s", cell, newval)
				}

			case "directors":
				newval := strings.Join(info.Directors, "; ")
				err = ssSet(cell, newval)
				if err != nil {
					return errors.Wrapf(err, "setting %s to %s", cell, newval)
				}

			case "genre":
				newval := strings.Join(info.Genres, "; ")
				err = ssSet(cell, newval)
				if err != nil {
					return errors.Wrapf(err, "setting %s to %s", cell, newval)
				}

			case "poster":
				if info.Image == "" {
					continue
				}
				if err = ssSet(cell, info.Image); err != nil {
					return errors.Wrapf(err, "setting %s to %s", cell, info.Image)
				}
				if err = uploadPoster(ctx, bucket, cl, info.Image, name, false); err != nil {
					return errors.Wrapf(err, "uploading poster for %s", name)
				}

			case "year":
				parts := strings.Split(info.DatePublished, "-")
				if len(parts) != 3 {
					continue
				}
				err = ssSet(cell, parts[0])
				if err != nil {
					return errors.Wrapf(err, "setting %s to %s", cell, parts[0])
				}

			case "plot":
				err = ssSet(cell, info.Summary)
				if err != nil {
					return errors.Wrapf(err, "setting %s to plot summary", cell)
				}

			case "runtime":
				if info.RuntimeMins > 0 {
					err = ssSet(cell, strconv.Itoa(info.RuntimeMins))
					if err != nil {
						return errors.Wrapf(err, "setting %s to runtime of %d", cell, info.RuntimeMins)
					}
				}
			}
		}

		return nil
	})
}

func uploadPoster(ctx context.Context, bucket *storage.BucketHandle, cl *http.Client, url, name string, force bool) error {
	var (
		urlExt   = filepath.Ext(url)
		nameExt  = filepath.Ext(name)
		rootName = strings.TrimSuffix(name, nameExt)
		objName  = rootName + urlExt
		obj      = bucket.Object(objName)
	)

	if urlExt == nameExt {
		log.Printf("  Skipping poster for %s; URL has identical extension", name)
		return nil
	}

	if !force {
		// Does obj already exist?
		_, err := obj.Attrs(ctx)
		if err == nil {
			log.Printf("  object %s already exists", objName)
			return nil
		}
		if !errors.Is(err, storage.ErrObjectNotExist) {
			return errors.Wrapf(err, "getting attrs for %s", objName)
		}
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return errors.Wrapf(err, "creating request for %s", url)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return errors.Wrapf(err, "getting %s", url)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("getting %s: %s", url, resp.Status)
	}

	log.Printf("Uploading poster for %s...", name)

	w := obj.NewWriter(ctx)
	defer w.Close()

	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err == nil { // sic
		w.ContentType = contentType
	}

	_, err = io.Copy(w, resp.Body)
	if err != nil {
		return errors.Wrapf(err, "copying %s to GCS", url)
	}

	err = w.Close()
	return errors.Wrap(err, "closing GCS writer")
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
