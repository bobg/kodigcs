package main

import (
	"context"
	"encoding/xml"
	"expvar"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/bobg/gcsobj"
	"github.com/bobg/mid"
	"github.com/pkg/errors"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

func main() {
	var (
		credsFile  = flag.String("creds", "creds.json", "path to service-account credentials JSON file")
		bucketName = flag.String("bucket", "", "Google Cloud Storage bucket name")
		sheetID    = flag.String("sheet", "", "ID of Google spreadsheet with title metadata")
		listenAddr = flag.String("listen", ":1549", "listen address")
		certFile   = flag.String("cert", "", "path to cert file")
		keyFile    = flag.String("key", "", "path to key file")
		username   = flag.String("username", "", "HTTP Basic Auth username")
		password   = flag.String("password", "", "HTTP Basic Auth password")
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

		dirRequests:      expvar.NewInt("dirRequests"),
		isoRequests:      expvar.NewInt("isoRequests"),
		nfoRequests:      expvar.NewInt("nfoRequests"),
		bytesRead:        expvar.NewInt("bytesRead"),
		spreadsheetLoads: expvar.NewInt("spreadsheetLoads"),
		bucketLoads:      expvar.NewInt("bucketLoads"),
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

	dirRequests      *expvar.Int
	isoRequests      *expvar.Int
	nfoRequests      *expvar.Int
	bytesRead        *expvar.Int
	spreadsheetLoads *expvar.Int
	bucketLoads      *expvar.Int

	mu           sync.RWMutex // protects all of the following
	objNames     []string
	objNamesTime time.Time
	infoMap      map[string]movieInfo
	infoMapTime  time.Time
}

func (s *server) handle(w http.ResponseWriter, req *http.Request) error {
	if s.username != "" && s.password != "" {
		username, password, ok := req.BasicAuth()
		if !ok {
			return mid.CodeErr{C: http.StatusUnauthorized}
		}
		if username != s.username || password != s.password {
			return mid.CodeErr{C: http.StatusUnauthorized}
		}
	}

	path := strings.Trim(req.URL.Path, "/")
	if path == "" {
		return s.handleDir(w, req)
	}

	if strings.HasSuffix(path, ".nfo") {
		return s.handleNFO(w, req, path)
	}

	s.isoRequests.Add(1)

	ctx := req.Context()
	obj := s.bucket.Object(path)
	r, err := gcsobj.NewReader(ctx, obj)
	if err != nil {
		return errors.Wrapf(err, "creating reader for object %s", path)
	}
	defer r.Close()

	http.ServeContent(w, req, path, time.Time{}, r)
	// TODO: is it necessary to wrap w in order to detect an error here and propagate it out?

	s.bytesRead.Add(r.NRead())
	log.Printf("served %s (%d bytes)", path, r.NRead())

	return nil
}

func (s *server) handleDir(w http.ResponseWriter, req *http.Request) error {
	log.Print("serving directory")
	s.dirRequests.Add(1)

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

	var items []string
	for _, objName := range s.objNames {
		rootName := strings.TrimSuffix(objName, ".iso")
		items = append(items, objName, rootName+".nfo")
	}

	return s.dirTemplate.Execute(w, items)
}

func (s *server) ensureObjNames(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.objNames) > 0 && !isStale(s.objNamesTime) {
		return nil
	}

	log.Print("loading bucket")
	s.bucketLoads.Add(1)

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
	s.spreadsheetLoads.Add(1)

	s.infoMap = make(map[string]movieInfo)
	sheetsSvc, err := sheets.NewService(ctx, option.WithCredentialsFile(s.credsFile), option.WithScopes(sheets.SpreadsheetsReadonlyScope))
	if err != nil {
		return errors.Wrap(err, "creating sheets service")
	}
	resp, err := sheetsSvc.Spreadsheets.Values.Get(s.sheetID, "Sheet1!A:Z").Do()
	if err != nil {
		return errors.Wrap(err, "reading spreadsheet data")
	}
	if len(resp.Values) < 2 {
		return fmt.Errorf("got %d spreadsheet row(s), wanted 2 or more", len(resp.Values))
	}

	var headings []string

	for i, row := range resp.Values {
		if i == 0 {
			for _, rawheading := range row {
				if heading, ok := rawheading.(string); ok {
					headings = append(headings, strings.ToLower(heading))
				} else {
					headings = append(headings, "")
				}
			}
			continue
		}

		if len(row) < 2 {
			continue
		}

		name, ok := row[0].(string)
		if !ok {
			continue
		}

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

			case "imdbid":
				info.imdbID = val
			}
		}

		rootName := strings.TrimSuffix(name, ".iso")
		if info.Title == "" {
			info.Title = rootName
		}

		s.infoMap[rootName] = info
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
	s.nfoRequests.Add(1)

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
