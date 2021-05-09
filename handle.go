package main

import (
	"encoding/xml"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/bobg/gcsobj"
	"github.com/bobg/mid"
	"github.com/pkg/errors"
)

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
