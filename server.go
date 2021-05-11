package main

import (
	"html/template"
	"sync"
	"time"

	"cloud.google.com/go/storage"
)

type server struct {
	bucket *storage.BucketHandle

	credsFile string
	sheetID   string

	dirTemplate *template.Template

	username, password string

	imdb bool

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
