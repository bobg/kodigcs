# Kodigcs - Serve video files for Kodi from a Google Cloud Storage bucket

This is kodigcs,
a server that supplies an HTTP or HTTPS “web directory source” to Kodi
containing files that live in a Google Cloud Storage bucket.

In addition to presenting GCS objects as video files to Kodi,
kodigcs also synthesizes matching “.nfo” files for each object,
containing metadata such as title,
director and actor names,
release year,
and so on.
This data is supplied in a Google Drive spreadsheet.

To use kodigcs, you’ll need:

1. Video files in [a Google Cloud Storage bucket](https://cloud.google.com/storage/docs/introduction);
2. [“Service account” credentials](https://cloud.google.com/iam/docs/service-accounts) for accessing the bucket, in the form of a JSON file;
3. A server host with a public URL on which to run kodigcs;
4. (Optional) A spreadsheet in Google Drive containing metadata for the objects in your GCS bucket.

Note:
be mindful of [the costs involved](https://cloud.google.com/storage/pricing) in storing and serving data from Google Cloud Storage.

Note:
if you have a server host without a public URL,
you might be able to give it one by using [ngrok](https://ngrok.com/).

## Installing kodigcs

Run:

```sh
go get github.com/bobg/kodigcs
```

## Running kodigcs

```sh
kodigcs -bucket BUCKETNAME [-creds CREDS] [-sheet SHEET_ID] [-listen ADDR] [-cert CERT] [-key KEY] [-username USERNAME] [-password PASSWORD]
```

- BUCKETNAME is the name of the GCS bucket
- CREDS is the name of the JSON file containing credentials for accessing the bucket (default is `creds.json`)
- SHEET_ID is the Google Drive spreadsheet ID of the metadata spreadsheet (see below)
- ADDR is the address on which the server will listen for requests (default `:1549`)
- CERT is the name of the TLS certificate file, if operating in TLS (i.e., HTTPS) mode
- KEY is the name of the TLS private key file, if operating in TLS (i.e., HTTPS) mode
- USERNAME is a username string that requests must supply, if using HTTP “basic authentication”
- PASSWORD is a password string that requests must supply, if using HTTP “basic authentication”

## Adding your kodigcs source to Kodi

Under Settings,
go to Media,
then Videos.
Select “Add videos.”

In the “add video source” dialog,
select “Browse” to browse for video sources.

In the “Browse for new share” dialog,
select “Add network location.”

In the “Add network location” dialog,
use the following settings:

- for Protocol, use “Web server directory” (either HTTPS, if kodigcs is running with `-cert` and `-key`, otherwise HTTP);
- for Server address, use the IP address or hostname of the server’s public URL
- leave Remote path blank
- for Port, use the kodigcs default of 1549, or whatever other port number you chose with `-listen`
- for Username and Password, use strings that match the `-username` and `-password` arguments, if any, otherwise leave blank

After confirming this dialog,
you’ll be back in the “Browse for new share” dialog.
Select the newly added source and confirm _this_ dialog.
That will return you to the “add video source” dialog,
where you must now give this new source a name.

## The metadata spreadsheet

You may specify metadata for the files in your GCS bucket in a Google Drive spreadsheet.

Row 1 of the spreadsheet must have column headings.
One of these must be `Filename`.
Each value in this column is the name of an object in your GCS bucket.

Remaining columns may have these headings:

- `Title`: this is the title that will be shown for the object. (The default is to infer the title from `Filename`.)
- `Subdir`: related objects may be grouped in a synthesized subdirectory by giving them identical `Subdir` names. This feature is of limited usefulness, as the Kodi user interface does not present these groupings, and it may change in future versions.
- `Year`: this is the release year for the title.
- `Directors`: this is a semicolon-separated list of directors for the title.
- `Actors`: this is a semicolon-separated list of actors for the title.
- `Runtime`: this is the running time, in minutes, of the title.
- `Trailer`: this is a YouTube URL of a trailer for the title.
- `Poster`: this is the URL of poster art for the title.
- `Tagline`: this is a short line of text, the title’s tag line.
- `Outline`: this is a short line of text, a summary of the title.
- `Plot`: this is a longer description of the title’s plot.

You must make your spreadsheet readable to at least the “service account” whose credentials kodigcs is using (with `-creds`).
You must specify the ID of the spreadsheet to kodigcs with `-sheet`.
The ID is the portion of the URL after `docs.google.com/spreadsheets/d/` and before the next `/`.
