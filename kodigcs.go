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

	"cloud.google.com/go/storage"
	"github.com/bobg/mid"
	"github.com/bobg/subcmd"
	"github.com/pkg/errors"
	"golang.org/x/time/rate"
	"google.golang.org/api/option"
)

func main() {
	credsFile := flag.String("creds", "creds.json", "path to service-account credentials JSON file")
	flag.Parse()

	c := maincmd{credsFile: *credsFile}
	err := subcmd.Run(context.Background(), c, flag.Args())
	if err != nil {
		log.Fatal(err)
	}
}

type maincmd struct {
	credsFile string
}

func (c maincmd) Subcmds() map[string]subcmd.Subcmd {
	return subcmd.Commands(
		"serve", c.serve, subcmd.Params(
			"bucket", subcmd.String, "", "Google Cloud Storage bucket name",
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
			"sheet", subcmd.String, "", "ID of Google spreadsheet with title metadata",
		),
	)
}

func (c maincmd) serve(ctx context.Context, bucketName, sheetID, listenAddr, certFile, keyFile, username, password string, subdirs, verbose bool, _ []string) error {
	client, err := storage.NewClient(ctx, option.WithCredentialsFile(c.credsFile))
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		bucket:      client.Bucket(bucketName),
		credsFile:   c.credsFile,
		sheetID:     sheetID,
		dirTemplate: template.Must(template.New("").Parse(dirTemplate)),
		username:    username,
		password:    password,
		subdirs:     subdirs,
		verbose:     verbose,
	}

	http.Handle("/", mid.Err(s.handle))

	log.Printf("Listening on %s", listenAddr)
	if certFile != "" && keyFile != "" {
		err = http.ListenAndServeTLS(listenAddr, certFile, keyFile, nil)
	} else {
		err = http.ListenAndServe(listenAddr, nil)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.Wrap(err, "in ListenAndServe")
}

func (c maincmd) ssupdate(ctx context.Context, sheetID string, _ []string) error {
	return updateSpreadsheet(ctx, c.credsFile, sheetID)
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
