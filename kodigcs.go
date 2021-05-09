package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/bobg/mid"
	"github.com/pkg/errors"
	"golang.org/x/time/rate"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

var imdbRE = regexp.MustCompile(`^https?://(?:www\.)?imdb\.com/title/([[:alnum:]]+)`)

func main() {
	var (
		credsFile  = flag.String("creds", "creds.json", "path to service-account credentials JSON file")
		bucketName = flag.String("bucket", "", "Google Cloud Storage bucket name")
		sheetID    = flag.String("sheet", "", "ID of Google spreadsheet with title metadata")
		listenAddr = flag.String("listen", ":1549", "listen address")
		certFile   = flag.String("cert", "", "path to cert file")
		keyFile    = flag.String("key", "", "path to key file")
		username   = flag.String("username", "", "HTTP Basic Auth username")
		password   = flag.String("password", "", "HTTP Basic Auth password") // TODO: move this to an env var so as not to reveal it via expvar
		imdb       = flag.Bool("imdb", false, "lookup titles by their IMDb ID and fill in missing spreadsheet fields")
	)
	flag.Parse()

	ctx := context.Background()

	client, err := storage.NewClient(ctx, option.WithCredentialsFile(*credsFile))
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		bucket:      client.Bucket(*bucketName),
		credsFile:   *credsFile,
		sheetID:     *sheetID,
		dirTemplate: template.Must(template.New("").Parse(dirTemplate)),
		username:    *username,
		password:    *password,

		limiter: rate.NewLimiter(rate.Every(time.Second), 1),

		// dirRequests:      expvar.NewInt("dirRequests"),
		// mediaRequests:    expvar.NewInt("mediaRequests"),
		// nfoRequests:      expvar.NewInt("nfoRequests"),
		// bytesRead:        expvar.NewInt("bytesRead"),
		// spreadsheetLoads: expvar.NewInt("spreadsheetLoads"),
		// bucketLoads:      expvar.NewInt("bucketLoads"),
	}

	if *imdb {
		log.Print("Updating spreadsheet")
		err = s.updateSpreadsheet(ctx)
		if err != nil {
			log.Fatal(err)
		}
	}

	http.Handle("/", mid.Err(s.handle))

	log.Printf("Listening on %s", *listenAddr)
	if *certFile != "" && *keyFile != "" {
		err = http.ListenAndServeTLS(*listenAddr, *certFile, *keyFile, nil)
	} else {
		err = http.ListenAndServe(*listenAddr, nil)
	}

	if errors.Is(err, http.ErrServerClosed) {
		// ok
	} else if err != nil {
		log.Fatal(err)
	}
}

func (s *server) parsePath(ctx context.Context, path string) (subdir, objname string, err error) {
	err = s.ensureInfoMap(ctx)
	if err != nil {
		return "", "", err
	}

	ext := filepath.Ext(path)
	pathRoot := strings.TrimSuffix(path, ext)

	s.mu.RLock()
	defer s.mu.RUnlock()

	for rootName, info := range s.infoMap {
		if path == info.subdir {
			return path, "", nil
		}
		prefix := rootNamePrefix(rootName)
		if pathRoot == info.subdir+"/"+prefix+rootName {
			return info.subdir, strings.TrimPrefix(path, info.subdir+"/"), nil
		}
		if pathRoot == prefix+rootName {
			return "", path, nil
		}
	}

	return "", path, nil
}

func rootNamePrefix(rootName string) string {
	hash := sha256.Sum256([]byte(rootName))
	hash64 := base64.URLEncoding.EncodeToString(hash[:])
	return hash64[:7] + "-"
}

func (s *server) ensureObjNames(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.objNames) > 0 && !isStale(s.objNamesTime) {
		return nil
	}

	log.Print("loading bucket")
	// s.bucketLoads.Add(1)

	s.objNames = nil
	iter := s.bucket.Objects(ctx, nil)
	for {
		attrs, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return errors.Wrap(err, "iterating over bucket")
		}
		s.objNames = append(s.objNames, attrs.Name)
	}

	s.objNamesTime = time.Now()
	return nil
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

func (s *server) ensureInfoMap(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sheetID == "" {
		return nil
	}
	if len(s.infoMap) > 0 && !isStale(s.infoMapTime) {
		return nil
	}

	log.Print("loading spreadsheet")
	// s.spreadsheetLoads.Add(1)

	s.infoMap = make(map[string]movieInfo)
	svc, err := sheets.NewService(ctx, option.WithCredentialsFile(s.credsFile), option.WithScopes(sheets.SpreadsheetsReadonlyScope))
	if err != nil {
		return errors.Wrap(err, "creating sheets service")
	}

	err = handleSheet(svc.Spreadsheets, s.sheetID, func(_ int, headings []string, name string, row []interface{}) error {
		var info movieInfo

		for j, rawval := range row {
			if j == 0 {
				continue
			}
			if j >= len(headings) {
				break
			}
			val, ok := rawval.(string)
			if !ok || val == "" {
				continue
			}

			heading := headings[j]
			switch heading {
			case "title":
				info.Title = val

			case "year":
				year, err := strconv.Atoi(val)
				if err != nil {
					log.Printf("Cannot parse year %s for %s: %s", val, name, err)
					continue
				}
				info.Year = year

			case "banner", "clearart", "clearlogo", "discart", "landscape", "poster":
				info.Thumbs = append(info.Thumbs, thumb{
					Aspect: heading,
					Val:    val,
				})

			case "directors":
				directors := splitsemi(val)
				info.Directors = append(info.Directors, directors...)

			case "actors":
				actors := splitsemi(val)
				for _, a := range actors {
					info.Actors = append(info.Actors, actor{
						Name:  a,
						Order: len(info.Actors),
					})
				}

			case "runtime":
				mins, err := strconv.Atoi(val)
				if err != nil {
					log.Printf("Cannot parse runtime %s for %s: %s", val, name, err)
					continue
				}
				info.Runtime = mins

			case "trailer":
				u, err := url.Parse(val)
				if err != nil {
					log.Printf("Cannot parse trailer URL %s for %s: %s", val, name, err)
					continue
				}

				var ytid string
				switch u.Host {
				case "www.youtube.com": // /watch?v=...
					path := strings.TrimPrefix(u.Path, "/")
					if path != "watch" {
						log.Printf("Cannot parse trailer URL %s for %s: not a watch link", val, name)
						continue
					}
					qvals, err := url.ParseQuery(u.RawQuery)
					if err != nil {
						log.Printf("Cannot parse query in trailer URL %s for %s: %s", val, name, err)
						continue
					}
					if v, ok := qvals["v"]; ok && len(v) > 0 {
						ytid = v[0]
					}

				case "youtu.be": // /...
					ytid = strings.TrimPrefix(u.Path, "/")

				default:
					log.Printf("Cannot parse trailer URL %s for %s: not a YouTube link", val, name)
					continue
				}

				if ytid == "" {
					log.Printf("Cannot parse YouTube ID out of trailer URL %s for %s", val, name)
					continue
				}

				info.Trailer = fmt.Sprintf("plugin://plugin.video.youtube/?action=play_video&videoid=%s", ytid)

			case "outline":
				info.Outline = val

			case "plot":
				info.Plot = val

			case "tagline":
				info.Tagline = val

			case "genre":
				info.Genre = val

			case "subdir":
				info.subdir = val

			case "imdbid":
				if m := imdbRE.FindStringSubmatch(val); len(m) > 1 {
					info.imdbID = m[1]
				} else {
					info.imdbID = val
				}
			}
		}

		var (
			ext      = filepath.Ext(name)
			rootName = strings.TrimSuffix(name, ext)
		)
		if info.Title == "" {
			info.Title = rootName
		}

		s.infoMap[rootName] = info

		return nil
	})
	if err != nil {
		return errors.Wrap(err, "processing spreadsheet")
	}

	s.infoMapTime = time.Now()
	return nil
}

func splitsemi(s string) []string {
	fields := strings.Split(s, ";")
	var result []string
	for _, f := range fields {
		trimmed := strings.TrimSpace(f)
		if trimmed == "" {
			continue
		}
		result = append(result, trimmed)
	}
	return result
}

const staleTime = 5 * time.Minute

func isStale(t time.Time) bool {
	return t.Before(time.Now().Add(-staleTime))
}

type (
	movieInfo struct {
		XMLName   xml.Name `xml:"movie"`
		Title     string   `xml:"title,omitempty"`
		Year      int      `xml:"year,omitempty"`
		Thumbs    []thumb  `xml:"thumb,omitempty"`
		Directors []string `xml:"director,omitempty"`
		Actors    []actor  `xml:"actor,omitempty"`
		Runtime   int      `xml:"runtime,omitempty"`
		Trailer   string   `xml:"trailer,omitempty"`
		Outline   string   `xml:"outline,omitempty"`
		Plot      string   `xml:"plot,omitempty"`
		Tagline   string   `xml:"tagline,omitempty"`
		Genre     string   `xml:"genre,omitempty"`
		subdir    string
		imdbID    string
	}

	thumb struct {
		XMLName xml.Name `xml:"thumb"`
		Aspect  string   `xml:"aspect,attr"`
		Val     string   `xml:",chardata"`
	}

	actor struct {
		XMLName xml.Name `xml:"actor"`
		Name    string   `xml:"name"`
		Role    string   `xml:"role,omitempty"`
		Order   int      `xml:"order"`
		Thumb   thumb    `xml:"thumb,omitempty"`
	}
)

type limitedTransport struct {
	limiter   *rate.Limiter
	transport http.RoundTripper
}

func (lt *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	err := lt.limiter.Wait(req.Context())
	if err != nil {
		return nil, err
	}
	return lt.transport.RoundTrip(req)
}

type base26 struct{}

func (base26) N() int64                         { return 26 }
func (base26) Encode(n int64) ([]byte, error)   { return []byte{'A' + byte(n)}, nil }
func (base26) Decode(inp []byte) (int64, error) { return int64(inp[0] - 'A'), nil }

const dirTemplate = `
<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 3.2 Final//EN">
<html>
 <head>
  <title>Index</title>
 </head>
 <body>
  <h1>Index</h1>
  <ul>
   {{ range . }}
    <li>
     <a href="{{ . }}">{{ . }}</a>
    </li>
   {{ end }}
  </ul>
 </body>
</html>
`
