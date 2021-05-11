package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
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
	imdbRE    = regexp.MustCompile(`^https?://(?:www\.)?imdb\.com/title/([[:alnum:]]+)`)
	runtimeRE = regexp.MustCompile(`^PT(\d+)M$`)
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

	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "parsing response from %s", titleURL)
	}

	headEl := htree.FindEl(doc, func(n *html.Node) bool {
		return n.DataAtom == atom.Head
	})
	if headEl == nil {
		return nil, fmt.Errorf("no HEAD in response from %s", titleURL)
	}
	jsonEl := htree.FindEl(headEl, func(n *html.Node) bool {
		return n.DataAtom == atom.Script && htree.ElAttr(n, "type") == "application/ld+json"
	})
	if jsonEl == nil {
		return nil, fmt.Errorf("no info JSON in response from %s", titleURL)
	}

	jsonBuf := new(bytes.Buffer)
	for child := jsonEl.FirstChild; child != nil; child = child.NextSibling {
		jsonBuf.WriteString(child.Data)
	}

	var result imdbInfo
	err = json.Unmarshal(jsonBuf.Bytes(), &result)
	if err != nil {
		return nil, errors.Wrapf(err, "unmarshaling JSON in response from %s", titleURL)
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

	summaryEl := htree.FindEl(doc, func(n *html.Node) bool {
		return n.DataAtom == atom.Div && htree.ElClassContains(n, "summary_text")
	})
	if summaryEl != nil {
		summary, err := htree.Text(summaryEl)
		if err != nil {
			return nil, errors.Wrapf(err, "getting summary text for %s", titleURL)
		}
		result.Summary = strings.TrimSpace(summary)
	}

	runtimeEl := htree.FindEl(doc, func(n *html.Node) bool {
		return n.DataAtom == atom.Time
	})
	if runtimeEl != nil {
		attr := htree.ElAttr(runtimeEl, "datetime")
		if m := runtimeRE.FindStringSubmatch(attr); len(m) > 0 {
			runtime, err := strconv.Atoi(m[1])
			if err != nil {
				log.Printf("Warning: cannot parse runtime string %s", attr)
			} else {
				result.RuntimeMins = runtime
			}
		}
	}

	return &result, nil
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
