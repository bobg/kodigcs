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

	verbose bool

	mu           sync.RWMutex // protects all of the following
	objNames     []string
	objNamesTime time.Time
	infoMap      map[string]movieInfo
	infoMapTime  time.Time
}
