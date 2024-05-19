package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"flag"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"cloud.google.com/go/storage"
	"github.com/bobg/errors"
	"github.com/bobg/mid"
	"github.com/bobg/subcmd"
	"golang.org/x/time/rate"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

func main() {
	var (
		credsFile = flag.String("creds", "creds.json", "path to service-account credentials JSON file")
		bucket    = flag.String("bucket", "", "Google Cloud Storage bucket name")
	)
	flag.Parse()

	if *bucket == "" {
		log.Fatal("Must specify -bucket")
	}

	ctx := context.Background()

	gcs, err := storage.NewClient(ctx, option.WithCredentialsFile(*credsFile))
	if err != nil {
		log.Fatalf("Error creating GCS client: %s", err)
	}

	// TODO: For the serve subcommand we only need sheets.SpreadsheetsReadonlyScope.
	ssvc, err := sheets.NewService(ctx, option.WithCredentialsFile(*credsFile), option.WithScopes(sheets.SpreadsheetsScope))
	if err != nil {
		log.Fatalf("Error creating sheets service: %s", err)
	}

	c := maincmd{
		ssvc:   ssvc.Spreadsheets,
		bucket: gcs.Bucket(*bucket),
	}
	if err := subcmd.Run(ctx, c, flag.Args()); err != nil {
		log.Fatal(err)
	}
}

type maincmd struct {
	ssvc   *sheets.SpreadsheetsService
	bucket *storage.BucketHandle
}

func (c maincmd) Subcmds() map[string]subcmd.Subcmd {
	return subcmd.Commands(
		"serve", c.serve, subcmd.Params(
			"sheet", subcmd.String, "", "ID of Google spreadsheet with title metadata",
			"listen", subcmd.String, ":1549", "listen address",
			"cert", subcmd.String, "", "path to cert file",
			"key", subcmd.String, "", "path to key file",
			"username", subcmd.String, "", "HTTP Basic Auth username",
			"password", subcmd.String, "", "HTTP Basic Auth password", // TODO: move this to an env var so as not to reveal it via expvar
			"subdirs", subcmd.Bool, true, "whether to serve subdirectories",
			"verbose", subcmd.Bool, false, "log each chunk of content as it's served",
		),
		"ssupdate", c.ssupdate, subcmd.Params(
			"htmldir", subcmd.String, "", "directory of IMDb *.iso.html files",
			"sheet", subcmd.String, "", "ID of Google spreadsheet with title metadata",
		),
	)
}

func (c maincmd) serve(outerCtx context.Context, sheetID, listenAddr, certFile, keyFile, username, password string, subdirs, verbose bool, _ []string) error {
	ctx, cancel := signal.NotifyContext(outerCtx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	s := &server{
		ssvc:        c.ssvc,
		bucket:      c.bucket,
		sheetID:     sheetID,
		dirTemplate: template.Must(template.New("").Parse(dirTemplate)),
		listenAddr:  listenAddr,
		username:    username,
		password:    password,
		subdirs:     subdirs,
		verbose:     verbose,
	}

	var (
		mux    = http.NewServeMux()
		thumb  = mid.Err(s.handleThumb)
		handle = mid.Err(s.handle)
	)
	if verbose {
		thumb = mid.Log(thumb)
		handle = mid.Log(handle)
	}
	mux.Handle("/thumbs/", thumb)
	mux.Handle("/", handle)

	h := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		log.Printf("Signal received, shutting down server")
		h.Shutdown(outerCtx)
	}()

	log.Printf("Listening on %s", listenAddr)

	var err error

	if certFile != "" && keyFile != "" {
		s.tls = true
		err = h.ListenAndServeTLS(certFile, keyFile)
	} else {
		err = h.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.Wrap(err, "in ListenAndServe")
}

func (c maincmd) ssupdate(ctx context.Context, htmldir, sheetID string, _ []string) error {
	return updateSpreadsheet(ctx, c.ssvc, c.bucket, htmldir, sheetID)
}

func rootNamePrefix(rootName string) string {
	hash := sha256.Sum256([]byte(rootName))
	hash64 := base64.URLEncoding.EncodeToString(hash[:])
	return hash64[:7] + "-"
}

type (
	movieInfo struct {
		XMLName   xml.Name `xml:"movie"`
		Title     string   `xml:"title,omitempty"`
		SortTitle string   `xml:"sorttitle,omitempty"`
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
		origVal string
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
