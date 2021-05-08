package main

import (
	"bytes"
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
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/bobg/basexx"
	"github.com/bobg/gcsobj"
	"github.com/bobg/htree"
	"github.com/bobg/mid"
	"github.com/pkg/errors"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
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

type server struct {
	bucket *storage.BucketHandle

	credsFile string
	sheetID   string

	dirTemplate *template.Template

	username, password string

	imdb    bool
	limiter *rate.Limiter // for use in ssSet

	// dirRequests      *expvar.Int
	// mediaRequests    *expvar.Int
	// nfoRequests      *expvar.Int
	// bytesRead        *expvar.Int
	// spreadsheetLoads *expvar.Int
	// bucketLoads      *expvar.Int

	mu           sync.RWMutex // protects all of the following
	objNames     []string
	objNamesTime time.Time
	infoMap      map[string]movieInfo
	infoMapTime  time.Time
}

func (s *server) handle(w http.ResponseWriter, req *http.Request) error {
	if s.username != "" && s.password != "" {
		username, password, ok := req.BasicAuth()
		if !ok || username != s.username || password != s.password {
			w.Header().Add("WWW-Authenticate", `Basic realm="Access to list and stream titles"`)
			return mid.CodeErr{C: http.StatusUnauthorized}
		}
	}

	path := strings.Trim(req.URL.Path, "/")
	if path == "" {
		return s.handleDir(w, req, "")
	}

	ctx := req.Context()

	subdir, objname, err := s.parsePath(ctx, path)
	if err != nil {
		return errors.Wrapf(err, "parsing path %s", path)
	}

	if objname == "" {
		return s.handleDir(w, req, subdir)
	}

	objname = objname[8:] // remove 7-byte hash prefix plus "-"

	if strings.HasSuffix(objname, ".nfo") {
		return s.handleNFO(w, req, objname)
	}

	// s.mediaRequests.Add(1)

	obj := s.bucket.Object(objname)
	r, err := gcsobj.NewReader(ctx, obj)
	if err != nil {
		return errors.Wrapf(err, "creating reader for object %s", objname)
	}
	defer r.Close()

	http.ServeContent(w, req, path, time.Time{}, r)
	// TODO: is it necessary to wrap w in order to detect an error here and propagate it out?

	// s.bytesRead.Add(r.NRead())

	return nil
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

func (s *server) handleDir(w http.ResponseWriter, req *http.Request, subdir string) error {
	log.Printf("serving directory \"%s\"", subdir)
	// s.dirRequests.Add(1)

	ctx := req.Context()

	err := s.ensureObjNames(ctx)
	if err != nil {
		return errors.Wrap(err, "getting obj names")
	}

	err = s.ensureInfoMap(ctx)
	if err != nil {
		return errors.Wrap(err, "getting info map")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var items []template.URL
	for _, objName := range s.objNames {
		if ext := filepath.Ext(objName); ext != ".nfo" {
			rootName := strings.TrimSuffix(objName, ext)
			info, ok := s.infoMap[rootName]
			if ok && info.subdir != subdir {
				continue
			}
			if !ok && subdir != "" {
				continue
			}

			// We add a prefix to the entry names based on the rootname's hash.
			// This is because Kodi doesn't seem to be able to distinguish between two different entries
			// that are identical for the first N bytes, for some value of N.
			// E.g., "The Best of The Electric Company, Vol. 2, Disc 1" looks the same to Kodi as
			// "The Best of The Electric Company, Vol. 2, Disc 2".
			prefix := rootNamePrefix(rootName)
			items = append(items, template.URL(prefix+objName), template.URL(prefix+rootName+".nfo"))
		}
	}

	if subdir == "" {
		subdirs := make(map[string]struct{})
		for _, info := range s.infoMap {
			if info.subdir != "" {
				subdirs[info.subdir] = struct{}{}
			}
		}
		for s := range subdirs {
			items = append(items, template.URL(s+"/"))
		}
	}

	return s.dirTemplate.Execute(w, items)
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

func (s *server) handleNFO(w http.ResponseWriter, req *http.Request, path string) error {
	ctx := req.Context()
	err := s.ensureInfoMap(ctx)
	if err != nil {
		return errors.Wrap(err, "getting info map")
	}

	log.Printf("serving %s", path)
	// s.nfoRequests.Add(1)

	s.mu.RLock()
	defer s.mu.RUnlock()

	path = strings.TrimSuffix(path, ".nfo")
	info, ok := s.infoMap[path]
	if !ok {
		info = movieInfo{Title: path}
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	err = enc.Encode(info)
	if err != nil {
		return errors.Wrap(err, "writing XML")
	}

	if info.imdbID != "" {
		fmt.Fprintf(w, "\nhttps://www.imdb.com/title/%s\n", info.imdbID)
	}
	return nil
}

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
