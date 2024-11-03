package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"cloud.google.com/go/storage"
	"github.com/bobg/certs"
	"github.com/bobg/errors"
	"github.com/bobg/mid"
	"github.com/bobg/subcmd/v2"
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
			"-sheet", subcmd.String, "", "ID of Google spreadsheet with title metadata",
			"-listen", subcmd.String, ":1549", "listen address",
			"-certcmd", subcmd.String, "", "command to produce a sequence of JSON-encoded TLS certificates",
			"-username", subcmd.String, "", "HTTP Basic Auth username",
			"-password", subcmd.String, "", "HTTP Basic Auth password", // TODO: move this to an env var so as not to reveal it via expvar
			"-subdirs", subcmd.Bool, true, "whether to serve subdirectories",
			"-verbose", subcmd.Bool, false, "log each chunk of content as it's served",
		),
		"ssupdate", c.ssupdate, subcmd.Params(
			"-htmldir", subcmd.String, "", "directory of IMDb *.iso.html files",
			"-sheet", subcmd.String, "", "ID of Google spreadsheet with title metadata",
		),
	)
}

func (c maincmd) serve(ctx context.Context, sheetID, listenAddr, certcmd, username, password string, subdirs, verbose bool, _ []string) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	s := &server{
		bucket:      c.bucket,
		dirTemplate: template.Must(template.New("").Parse(dirTemplate)),
		listenAddr:  listenAddr,
		password:    password,
		sheetID:     sheetID,
		ssvc:        c.ssvc,
		subdirs:     subdirs,
		tls:         certcmd != "",
		username:    username,
		verbose:     verbose,
	}

	return s.serveHelper(ctx, certcmd)
}

func (s *server) serveHelper(ctx context.Context, certcmd string) (err error) {
	if certcmd == "" {
		return s.serveWithCert(ctx, nil)
	}

	certCh, wait, err := certs.FromCommand(ctx, certcmd)
	if err != nil {
		return errors.Wrap(err, "launching cert command")
	}
	defer func() {
		err2 := wait()
		err = errors.Join(err, err2)
	}()

	var (
		cert tls.Certificate
		ok   bool
	)

	select {
	case <-ctx.Done():
		return ctx.Err()

	case cert, ok = <-certCh:
		if !ok {
			return fmt.Errorf("cert command exited before producing a certificate")
		}
	}

	for {
		newCertPtr, err := s.serveHelper2(ctx, certCh, cert)
		if err != nil {
			return errors.Wrap(err, "serving with certificate")
		}
		cert = *newCertPtr
	}
}

func (s *server) serveHelper2(outerCtx context.Context, certCh <-chan tls.Certificate, cert tls.Certificate) (*tls.Certificate, error) {
	ctx, cancel := context.WithCancel(outerCtx)
	defer cancel()

	errCh := make(chan error, 1)

	go func() {
		errCh <- s.serveWithCert(ctx, &cert)
		close(errCh)
	}()

	select {
	case <-outerCtx.Done():
		return nil, outerCtx.Err()

	case err := <-errCh: // TODO: can err be nil here?
		if errors.Is(err, context.Canceled) && outerCtx.Err() == nil {
			err = nil
		}
		return nil, errors.Wrap(err, "error from goroutine")

	case newCert, ok := <-certCh:
		if !ok {
			return nil, fmt.Errorf("cert command exited")
		}

		cancel()

		err := <-errCh
		if err != nil {
			if errors.Is(err, context.Canceled) && outerCtx.Err() == nil {
				err = nil
			}
		}

		return &newCert, errors.Wrap(err, "after canceling goroutine")
	}
}

func (s *server) serveWithCert(ctx context.Context, cert *tls.Certificate) error {
	var (
		mux    = http.NewServeMux()
		thumb  = mid.Err(s.handleThumb)
		handle = mid.Err(s.handle)
	)
	if s.verbose {
		thumb = mid.Log(thumb)
		handle = mid.Log(handle)
	}
	mux.Handle("/thumbs/", thumb)
	mux.Handle("/", handle)

	h := &http.Server{
		Addr:    s.listenAddr,
		Handler: mux,
	}
	if cert != nil {
		h.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*cert}}
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("Listening on %s", s.listenAddr)
		if cert != nil {
			errCh <- h.ListenAndServeTLS("", "")
		} else {
			errCh <- h.ListenAndServe()
		}
		close(errCh)
	}()

	ctxWithoutCancel := context.WithoutCancel(ctx)

	select {
	case <-ctx.Done():
		log.Printf("Context canceled, shutting down server")
		if err := h.Shutdown(ctxWithoutCancel); err != nil {
			return errors.Wrap(err, "in Shutdown")
		}
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.Wrap(err, "in ListenAndServe")

	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.Wrap(err, "in ListenAndServe")
	}
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
