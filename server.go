package main

import (
	"html/template"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/bobg/go-generics/v2/set"
	"google.golang.org/api/sheets/v4"
)

type server struct {
	ssvc   *sheets.SpreadsheetsService
	bucket *storage.BucketHandle

	sheetID string

	dirTemplate *template.Template

	username, password string

	subdirs bool
	verbose bool

	mu           sync.RWMutex // protects all of the following
	objNames     set.Of[string]
	objNamesTime time.Time
	infoMap      map[string]movieInfo
	infoMapTime  time.Time
}
