package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bobg/bib"
	"github.com/bobg/errors"
	"github.com/bobg/gcsobj"
	"github.com/bobg/go-generics/v4/set"
	"github.com/bobg/go-generics/v4/slices"
	"github.com/bobg/mid"
	"google.golang.org/api/iterator"
)

func (s *server) handle(w http.ResponseWriter, req *http.Request) error {
	if err := s.checkAuth(w, req); err != nil {
		return err
	}

	path := strings.Trim(req.URL.Path, "/")
	if path == "" {
		return s.handleDir(w, req, "")
	}

	ctx := req.Context()

	if path == "infomap" {
		err := s.ensureInfoMap(ctx)
		if err != nil {
			return errors.Wrap(err, "getting info map")
		}

		s.mu.RLock()
		defer s.mu.RUnlock()

		return mid.RespondJSON(w, s.infoMap)
	}

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

	err = s.serveObj(ctx, w, req, objname, path, s.verbose)
	return errors.Wrap(err, "serving object")
}

func (s *server) checkAuth(w http.ResponseWriter, req *http.Request) error {
	if s.username == "" || s.password == "" {
		return nil
	}

	username, password, ok := req.BasicAuth()
	if !ok {
		w.Header().Add("WWW-Authenticate", `Basic realm="Access to list and stream titles"`)
		return mid.CodeErr{C: http.StatusUnauthorized}
	}

	if username != s.username || password != s.password {
		log.Printf("Unauthorized access attempt from %s (username %s, password %s)", req.RemoteAddr, username, password)
		return mid.CodeErr{C: http.StatusUnauthorized}
	}

	return nil
}

func (s *server) serveObj(ctx context.Context, w http.ResponseWriter, req *http.Request, objname, path string, verbose bool) (err error) {
	if verbose {
		defer func() {
			if err == nil {
				return
			}
			log.Printf("Error serving %s: %s", objname, err)
		}()
	}

	obj := s.bucket.Object(objname)
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return errors.Wrapf(err, "getting attrs for object %s", objname)
	}
	objtime := attrs.Created
	if attrs.Updated.After(objtime) {
		objtime = attrs.Updated
	}

	r := gcsobj.NewReaderWithSize(ctx, obj, attrs.Size)
	defer r.Close()

	if verbose {
		log.Printf("Serving %s", objname)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		start := time.Now()
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					log.Printf("Finished serving %s: %d bytes in %s", objname, r.NRead(), time.Since(start))
					return

				case <-ticker.C:
					log.Printf("Still serving %s: %d bytes in %s", objname, r.NRead(), time.Since(start))
				}
			}
		}()
	}

	wrapper := &mid.ResponseWrapper{W: w}
	http.ServeContent(wrapper, req, path, objtime, r)
	if wrapper.Code < 200 || wrapper.Code >= 400 {
		return mid.CodeErr{C: wrapper.Code}
	}
	return nil
}

func (s *server) handleThumb(w http.ResponseWriter, req *http.Request) error {
	if err := s.checkAuth(w, req); err != nil {
		return err
	}

	path := strings.Trim(req.URL.Path, "/")
	path = strings.TrimPrefix(path, "thumbs/")

	var err error
	path, err = url.PathUnescape(path)
	if err != nil {
		return errors.Wrapf(err, "unescaping path %s", path)
	}

	ctx := req.Context()
	if err := s.ensureObjNames(ctx); err != nil {
		return errors.Wrap(err, "getting obj names")
	}

	if err := s.ensureInfoMap(ctx); err != nil {
		return errors.Wrap(err, "in ensureInfoMap")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.objNames.Has(path) {
		// Serve this thumb from the bucket.

		log.Printf("Serving local thumb %s", path)

		err := s.serveObj(ctx, w, req, path, "/thumbs/"+path, false)
		return errors.Wrap(err, "serving local thumb")
	}

	// Redirect to the thumb's actual URL.

	var (
		ext  = filepath.Ext(path)
		root = strings.TrimSuffix(path, ext)
	)

	entry, ok := s.infoMap[root]
	if !ok {
		return mid.CodeErr{
			C:   http.StatusNotFound,
			Err: fmt.Errorf("no infoMap entry for /thumbs/%s", path),
		}
	}
	matches := slices.Filter(entry.Thumbs, func(th thumb) bool { return th.Val == path })
	if len(matches) == 0 {
		return mid.CodeErr{
			C:   http.StatusNotFound,
			Err: fmt.Errorf("no redirect URL for /thumbs/%s", path),
		}
	}

	log.Printf("Redirecting /thumbs/%s to %s", path, matches[0].origVal)

	http.Redirect(w, req, matches[0].origVal, http.StatusFound)
	return nil
}

func (s *server) handleDir(w http.ResponseWriter, req *http.Request, subdir string) error {
	if !s.subdirs && subdir != "" {
		return mid.CodeErr{
			C:   http.StatusBadRequest,
			Err: fmt.Errorf("will not serve subdir \"%s\" in non-subdirs mode", subdir),
		}
	}

	log.Printf("serving directory \"%s\"", subdir)

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
	s.objNames.Each(func(objName string) {
		ext := filepath.Ext(objName)
		switch ext {
		case ".iso", ".m2ts", ".m4v", ".mp4":
			// ok
		default:
			return
		}

		if false && ext != ".iso" {
			return
		}

		rootName := strings.TrimSuffix(objName, ext)
		info, ok := s.infoMap[rootName]
		if ok && s.subdirs && info.subdir != subdir {
			return
		}
		if !ok && s.subdirs && subdir != "" {
			return
		}

		// We add a prefix to the entry names based on the rootname's hash.
		// This is because Kodi doesn't seem to be able to distinguish between two different entries
		// that are identical for the first N bytes, for some value of N.
		// E.g., "The Best of The Electric Company, Vol. 2, Disc 1" looks the same to Kodi as
		// "The Best of The Electric Company, Vol. 2, Disc 2".
		prefix := rootNamePrefix(rootName)
		items = append(items, template.URL(prefix+objName), template.URL(prefix+rootName+".nfo"))
	})

	if s.subdirs && subdir == "" {
		subdirs := make(map[string]struct{})
		for _, info := range s.infoMap {
			if info.subdir != "" {
				subdirs[info.subdir] = struct{}{}
			}
		}
		for sd := range subdirs {
			items = append(items, template.URL(sd+"/"))
		}
	}

	return s.dirTemplate.Execute(w, items)
}

func (s *server) handleNFO(w http.ResponseWriter, req *http.Request, path string) error {
	ctx := req.Context()
	err := s.ensureInfoMap(ctx)
	if err != nil {
		return errors.Wrap(err, "getting info map")
	}

	log.Printf("serving %s", path)

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

func (s *server) ensureObjNames(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.objNames != nil && s.objNames.Len() > 0 && !isStale(s.objNamesTime) {
		return nil
	}

	log.Print("loading bucket")

	s.objNames = set.New[string]()

	iter := s.bucket.Objects(ctx, nil)
	for {
		attrs, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return errors.Wrap(err, "iterating over bucket")
		}
		s.objNames.Add(attrs.Name)
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

	s.infoMap = make(map[string]movieInfo)

	err := handleSheet(s.ssvc, s.sheetID, func(_ int, headings []string, name string, row []interface{}) error {
		var info movieInfo

		var (
			ext      = filepath.Ext(name)
			rootName = strings.TrimSuffix(name, ext)
		)

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

			case "sort":
				info.SortTitle = strings.ToLower(val)

			case "year":
				year, err := strconv.Atoi(val)
				if err != nil {
					log.Printf("Cannot parse year %s for %s: %s", val, name, err)
					continue
				}
				info.Year = year

			case "banner", "clearart", "clearlogo", "discart", "landscape", "poster":
				origVal := val

				if len(info.Thumbs) == 0 {
					valExt := filepath.Ext(val)
					val = s.relURL("thumbs/" + url.PathEscape(rootName) + valExt) // "foo bar.iso" -> http://host:port/thumbs/foo%20bar.jpg
				}
				info.Thumbs = append(info.Thumbs, thumb{
					Aspect:  heading,
					Val:     val,
					origVal: origVal,
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
				info.imdbID = parseIMDbID(val)
			}
		}

		if info.Title == "" {
			info.Title = rootName
		}
		if info.SortTitle == "" {
			info.SortTitle = bib.Key(info.Title)
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

func (s *server) relURL(path string) string {
	scheme := "http"
	if s.tls {
		scheme = "https"
	}

	u := &url.URL{
		Scheme: scheme,
		Host:   s.listenAddr,
		Path:   path,
	}

	return u.String()
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
