package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/bobg/htree"
	"github.com/pkg/errors"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	// This regex parses an IMDb title URL,
	// creating a capture group for the title's "ID" (e.g. "tt0076759" for Star Wars).
	imdbRE = regexp.MustCompile(`^https?://(?:www\.)?imdb\.com/title/([[:alnum:]]+)`)

	// Here are five different regular expression patterns for parsing a title's running time.
	// They are tried one by one in getRuntimeMins (below)
	// until we find one that succeeds.
	runtimeRE1 = regexp.MustCompile(`^PT(\d+)M$`)
	runtimeRE2 = regexp.MustCompile(`(\d+)h\s+(\d+)m`)
	runtimeRE3 = regexp.MustCompile(`(\d+)min`)
	runtimeRE4 = regexp.MustCompile(`(\d+)\s*hours?\s*(\d+)\s*minute`)
	runtimeRE5 = regexp.MustCompile(`(\d+)\s*hour`)
)

func parseIMDbID(inp string) string {
	if m := imdbRE.FindStringSubmatch(inp); len(m) > 1 {
		return m[1]
	}
	return inp
}

type imdbInfo struct {
	Name          string          `json:"name"`
	Image         string          `json:"image"`
	RawGenre      json.RawMessage `json:"genre"`    // string or []string
	RawActor      json.RawMessage `json:"actor"`    // person or []person
	RawDirector   json.RawMessage `json:"director"` // person or []person
	Description   string          `json:"description"`
	DatePublished string          `json:"datePublished"`
	Duration      string          `json:"duration"`

	Genres    []string `json:"-"`
	Actors    []string `json:"-"`
	Directors []string `json:"-"`

	RuntimeMins int    `json:"-"`
	Summary     string `json:"-"`
}

func parseIMDbPage(cl *http.Client, id string) (*imdbInfo, error) {
	titleURL := fmt.Sprintf("https://www.imdb.com/title/%s/", id)

	req, err := http.NewRequest("GET", titleURL, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "building request to GET %s", titleURL)
	}

	resp, err := cl.Do(req)
	if err != nil {
		return nil, errors.Wrapf(err, "getting %s", titleURL)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("status %d (%s) getting %s", resp.StatusCode, http.StatusText(resp.StatusCode), titleURL)
	}

	return parseIMDbHTML(resp.Body)
}

func parseIMDbHTML(r io.Reader) (*imdbInfo, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, errors.Wrap(err, "parsing HTML")
	}

	headEl := htree.FindEl(doc, func(n *html.Node) bool {
		return n.DataAtom == atom.Head
	})
	if headEl == nil {
		return nil, fmt.Errorf("no HEAD in HTML")
	}
	jsonEl := htree.FindEl(headEl, func(n *html.Node) bool {
		return n.DataAtom == atom.Script && htree.ElAttr(n, "type") == "application/ld+json"
	})
	if jsonEl == nil {
		return nil, fmt.Errorf("no info JSON in HTML")
	}

	jsonBuf := new(bytes.Buffer)
	for child := jsonEl.FirstChild; child != nil; child = child.NextSibling {
		jsonBuf.WriteString(child.Data)
	}

	var result imdbInfo
	err = json.Unmarshal(jsonBuf.Bytes(), &result)
	if err != nil {
		return nil, errors.Wrapf(err, "unmarshaling JSON in HTML")
	}

	result.Actors, err = parsePersons(result.RawActor)
	if err != nil {
		return nil, errors.Wrap(err, "parsing actors")
	}
	result.Directors, err = parsePersons(result.RawDirector)
	if err != nil {
		return nil, errors.Wrap(err, "parsing directors")
	}

	var genre string
	err = json.Unmarshal(result.RawGenre, &genre)
	if err != nil {
		err = json.Unmarshal(result.RawGenre, &result.Genres)
		if err != nil {
			return nil, fmt.Errorf("could not parse genre %s", string(result.RawGenre))
		}
	} else {
		result.Genres = []string{genre}
	}

	summary, err := getSummary(doc)
	if err != nil {
		return nil, errors.Wrap(err, "getting summary text")
	}
	result.Summary = strings.TrimSpace(summary)
	if result.Summary == "" {
		result.Summary = result.Description
	}

	runtimeMins, err := getRuntimeMins(doc)
	if err != nil {
		return nil, errors.Wrap(err, "getting runtime")
	}
	if runtimeMins > 0 {
		result.RuntimeMins = runtimeMins
	}

	return &result, nil
}

func getSummary(doc *html.Node) (string, error) {
	summaryEl := htree.FindEl(doc, func(n *html.Node) bool {
		return n.DataAtom == atom.Div && htree.ElClassContains(n, "summary_text")
	})
	if summaryEl != nil {
		return htree.Text(summaryEl)
	}

	summaryEl = htree.FindEl(doc, func(n *html.Node) bool {
		return n.DataAtom == atom.Div && htree.ElAttr(n, "data-testid") == "storyline-plot-summary"
	})
	if summaryEl != nil {
		return htree.Text(summaryEl)
	}

	return "", nil
}

func getRuntimeMins(doc *html.Node) (int, error) {
	runtimeEl := htree.FindEl(doc, func(n *html.Node) bool {
		return n.DataAtom == atom.Time
	})
	if runtimeEl != nil {
		attr := htree.ElAttr(runtimeEl, "datetime")
		if m := runtimeRE1.FindStringSubmatch(attr); len(m) > 0 {
			runtime, err := strconv.Atoi(m[1])
			if err == nil {
				// Ignore errors.
				return runtime, nil
			}
		}
	}

	runtimeEl = htree.FindEl(doc, func(n *html.Node) bool {
		return n.DataAtom == atom.Li && htree.ElAttr(n, "data-testid") == "title-techspec_runtime"
	})
	if runtimeEl != nil {
		subEl := htree.FindEl(runtimeEl, func(n *html.Node) bool {
			return n.DataAtom == atom.Span && htree.ElClassContains(n, "ipc-metadata-list-item__list-content-item")
		})
		if subEl != nil {
			text, err := htree.Text(subEl)
			if err != nil {
				return 0, errors.Wrap(err, "getting runtime text (1)")
			}
			if m := runtimeRE2.FindStringSubmatch(text); len(m) > 0 {
				hrs, err := strconv.Atoi(m[1])
				if err != nil {
					return 0, errors.Wrapf(err, "parsing runtime %s (1)", text)
				}
				mins, err := strconv.Atoi(m[2])
				if err != nil {
					return 0, errors.Wrapf(err, "parsing runtime %s (2)", text)
				}
				return 60*hrs + mins, nil
			}
			if m := runtimeRE3.FindStringSubmatch(text); len(m) > 0 {
				return strconv.Atoi(m[1])
			}
		}
		subEl = htree.FindEl(runtimeEl, func(n *html.Node) bool {
			return n.DataAtom == atom.Div && htree.ElClassContains(n, "ipc-metadata-list-item__content-container")
		})
		if subEl != nil {
			text, err := htree.Text(subEl)
			if err != nil {
				return 0, errors.Wrap(err, "getting runtime text (2)")
			}
			if m := runtimeRE4.FindStringSubmatch(text); len(m) > 0 {
				hrs, err := strconv.Atoi(m[1])
				if err != nil {
					return 0, errors.Wrapf(err, "parsing runtime %s (3)", text)
				}
				mins, err := strconv.Atoi(m[2])
				if err != nil {
					return 0, errors.Wrapf(err, "parsing runtime %s (4)", text)
				}
				return 60*hrs + mins, nil
			}
			if m := runtimeRE5.FindStringSubmatch(text); len(m) > 0 {
				hrs, err := strconv.Atoi(m[1])
				if err != nil {
					return 0, errors.Wrapf(err, "parsing runtime %s (5)", text)
				}
				return 60 * hrs, nil
			}
		}
	}

	return 0, nil
}

func parsePersons(inp []byte) ([]string, error) {
	if len(inp) == 0 {
		return nil, nil
	}

	type person struct {
		Name string `json:"name"`
	}

	var (
		p  person
		ps []person
	)
	err := json.Unmarshal(inp, &p)
	if err != nil {
		err = json.Unmarshal(inp, &ps)
		if err != nil {
			return nil, fmt.Errorf("could not parse %s", string(inp))
		}
	} else {
		ps = []person{p}
	}

	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.Name)
	}
	return names, nil
}
