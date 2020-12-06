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
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
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
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return errors.Wrapf(err, "getting attrs of %s", path)
	}

	r := &objReader{obj: obj, ctx: ctx, size: attrs.Size}
	defer r.Close()

	http.ServeContent(w, req, path, time.Time{}, r)
	// TODO: is it necessary to wrap w in order to detect an error here and propagate it out?

	s.bytesRead.Add(r.nread)
	log.Printf("served %s (%d bytes)", path, r.nread)

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
	if s.infoMap == nil {
		items = s.objNames
	} else {
		for _, objName := range s.objNames {
			items = append(items, objName)
			rootName := strings.TrimSuffix(objName, ".iso")
			if _, ok := s.infoMap[rootName]; ok {
				items = append(items, rootName+".nfo")
			}
		}
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
	resp, err := sheetsSvc.Spreadsheets.Values.Get(s.sheetID, "Sheet1!A:C").Do()
	if err != nil {
		return errors.Wrap(err, "reading spreadsheet data")
	}
	if len(resp.Values) < 2 {
		return fmt.Errorf("got %d spreadsheet row(s), wanted 2 or more", len(resp.Values))
	}
	for i, row := range resp.Values {
		if i == 0 {
			// TODO: parse column headings
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
		if title, ok := row[1].(string); ok {
			info.Title = title
		}
		if len(row) >= 3 {
			if imdbID, ok := row[2].(string); ok {
				info.IMDbID.Type = "imdb"
				info.IMDbID.Val = imdbID
			}
		}
		if info == (movieInfo{}) {
			continue
		}
		rootName := strings.TrimSuffix(name, ".iso")
		s.infoMap[rootName] = info
	}

	s.infoMapTime = time.Now()
	return nil
}

const staleTime = 5 * time.Minute

func isStale(t time.Time) bool {
	return t.Before(time.Now().Add(-staleTime))
}

type movieInfo struct {
	XMLName xml.Name `xml:"movie"`
	Title   string   `xml:"title,omitempty"`
	IMDbID  imdbID   `xml:"uniqueid,omitempty"`
}

type imdbID struct {
	XMLName xml.Name `xml:"uniqueid"`
	Type    string   `xml:"type,attr"`
	Val     string   `xml:",chardata"`
}

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
		return mid.CodeErr{C: http.StatusNotFound}
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	err = enc.Encode(info)
	if err != nil {
		return errors.Wrap(err, "writing XML")
	}

	if imdbID := info.IMDbID.Val; imdbID != "" {
		fmt.Fprintf(w, "\nhttps://www.imdb.com/title/%s\n", imdbID)
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
