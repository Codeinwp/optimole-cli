# optimole

`optimole` uploads local files into an [Optimole](https://optimole.com) media library, into the folders you
choose. It is a single binary with no dependencies.

```sh
optimole login                                   # paste your API key once
optimole upload -r ./photos --folder Campaigns   # photos/2024/a.jpg -> Campaigns/2024
```

> **Server requirement:** the CLI uses the dashboard's unified upload API (`POST /api/assets` →
> presigned `PUT` → `POST /api/assets/{key}/completion`). It works against production once that API is
> released on dashboard.optimole.com; until then, point it at an environment that has it with `--api-url`.

## Install

With Go 1.23 or newer:

```sh
go install github.com/Codeinwp/optimole-cli/cmd/optimole@latest
```

Prebuilt binaries for macOS, Linux and Windows (amd64 and arm64) will be attached to each
[GitHub release](https://github.com/Codeinwp/optimole-cli/releases) once releases are published, with a
`checksums.txt` to verify them:

```sh
shasum -a 256 -c checksums.txt --ignore-missing
tar -xzf optimole_*_darwin_arm64.tar.gz && sudo mv optimole /usr/local/bin/
```

## Authentication

Create an API key in the Optimole dashboard, then pick one of:

| Source | Example | Notes |
|---|---|---|
| `optimole login` | `optimole login` | Reads the key from stdin (hidden while you type), checks it against the API and saves it to `<config dir>/optimole/config.json` (directory `0700`, file `0600`). The config dir is `~/Library/Application Support` on macOS, `$XDG_CONFIG_HOME` or `~/.config` on Linux and `%AppData%` on Windows. |
| Environment | `export OPTIMOLE_API_KEY=...` | Good for CI. |
| Flag | `--api-key ...` | Visible in your shell history and process list; prefer the other two. |

The first one found wins, in this order: `--api-key`, `OPTIMOLE_API_KEY`, the saved key. `optimole logout`
deletes the saved key.

The key is never printed: it is removed from every error message, and it is only sent to the API, never to
the storage URL the file bytes go to.

**Folder-scoped keys** (the ones starting with `optml-s`) work for uploads into the folders they were
granted (and their subfolders): always pass `--folder` (or `--folder-id`). They cannot create folders, upload
into the library root, or list folders.

The API base URL defaults to `https://dashboard.optimole.com/api`; override it with `--api-url` or
`OPTIMOLE_API_URL` (for example `https://staging-dashboard.optimole.com/api`). Only `https` URLs are
accepted, except `http://localhost` and `http://127.0.0.1`. TLS certificates are always verified.

## Commands

### `optimole upload [flags] <path>...`

Uploads files, and with `-r` the contents of directories.

| Flag | Meaning |
|---|---|
| `--folder PATH` | Destination folder path from the library root, e.g. `Campaigns/2026/Spring`. |
| `--folder-id N` | Destination folder ID instead of a path (see `optimole folders`). Cannot be combined with `--folder` or with subdirectories. |
| `--create-folders` | Create missing folders (full-access keys only). Without it, a missing folder is an error. |
| `-r`, `--recursive` | Upload directory contents; subdirectories become subfolders. |
| `--include-hidden` | Include dot-prefixed files and directories found inside directories. |
| `--exclude GLOB` | Skip matching paths; repeatable. Tried against the path relative to the argument and against the base name (`*.psd`, `raw`, `2024/drafts/*`). `**` is not supported. |
| `-c`, `--concurrency N` | Files uploaded in parallel, 1–16 (default 4). |
| `--dry-run` | Show what would go where; contacts nothing and needs no key. |
| `--json` | One JSON object per file, then a summary object. |
| `--api-key`, `--api-url` | See [Authentication](#authentication). |

Examples:

```sh
# One file into the library root
optimole upload logo.png

# Several files into an existing folder
optimole upload --folder "Brand/Logos" logo.png logo-dark.png

# A whole tree, creating folders as needed, skipping Photoshop files
optimole upload -r ./site-assets --folder Website --create-folders --exclude '*.psd'

# Check the mapping first
optimole upload -r ./photos --folder Campaigns --dry-run

# Machine-readable output
optimole upload -r ./photos --folder Campaigns --json | jq -r 'select(.status=="uploaded") | .url'
```

Each file is uploaded in three steps: the CLI reserves an asset (`POST /api/assets`), streams the file
straight to storage with the presigned `PUT` (never loading it into memory), then completes it
(`POST /api/assets/{key}/completion`, with the image width and height for JPEG, PNG and GIF). The CDN URL
from the completion answer is printed.

#### Folder mapping

- A file argument goes into `--folder` (or the library root).
- A directory argument needs `-r`. Its **contents** go into `--folder` (or the root), and every
  subdirectory becomes a subfolder below it:

  ```text
  optimole upload -r ./photos --folder Campaigns
    ./photos/a.jpg            -> Campaigns
    ./photos/2024/b.jpg       -> Campaigns/2024
    ./photos/2024/raw/c.jpg   -> Campaigns/2024/raw
  ```

- Folder names match case-insensitively (`campaigns` finds `Campaigns`); spaces around each name are
  trimmed; leading and trailing slashes in `--folder` are ignored. A path has at most 20 levels and each name
  at most 255 characters.
- Hidden entries (names starting with `.`) inside directories are ignored unless `--include-hidden`; a file
  you name explicitly is always considered.
- Symbolic links are never followed, including one named on the command line; they are reported as skipped.
- Nothing is uploaded twice when the same file is named twice.

#### What gets skipped

Files the server would refuse are reported as `skipped` without contacting it (the server still has the
final say):

- **Size:** empty files and files over **100 MB** (104,857,600 bytes).
- **Names** longer than 255 characters, and folder paths over the limits above.
- **Types:** the extension (case-insensitive) must be one of:
  - Images: gif, jpg, jpeg, jpe, png, webp, avif, svg, svgz
  - Video: asf, asx, wmv, wmx, wm, avi, divx, flv, mov, qt, mpeg, mpg, mpe, mp4, m4v, ogv, webm, mkv, 3gp,
    3gpp, 3g2, 3gp2
  - Audio: mp3, m4a, m4b, aac, ra, ram, wav, ogg, oga, flac, mid, midi, wma, wax, mka
  - Text: txt, asc, c, cc, h, srt, csv, tsv, ics, rtx, css, vtt
  - Documents and archives: rtf, pdf, json, tar, zip, gz, gzip, rar, 7z, psd, xcf, doc, pot, pps, ppt, wri,
    xla, xls, xlt, xlw, mdb, mpp, docx, docm, dotx, dotm, xlsx, xlsm, xlsb, xltx, xltm, xlam, pptx, pptm,
    ppsx, ppsm, potx, potm, ppam, sldx, sldm, onetoc, onetoc2, onetmp, onepkg, oxps, xps, odt, odp, ods, odg,
    odc, odb, odf, wp, wpd, key, numbers, pages
  - Always refused: htm, html, js, swf, exe, class, dfxp

#### Parallel uploads and new folders

Up to `-c` files are in flight at once. The first file bound for each destination folder is uploaded on its
own; once it is done, the rest of that folder's files go in parallel and name the folder by the ID the server
returned. With `--create-folders`, destinations that would create the same missing parent (`New/a` and
`New/b` both create `New`) also start one after the other until that parent exists. This keeps parallel
requests from creating the same folder twice.

If the server says a destination folder is invalid (missing, or not granted to a scoped key), the other files
for that folder are marked failed without more requests. An invalid key or a full account
(`upload_limit_reached`) stops the run; the files not yet sent are reported as not attempted.

#### Retries

- API calls are retried on `429` (waiting as long as `Retry-After` says), `502`, `503`, `504` and network
  errors, with capped exponential backoff and jitter, up to 4 attempts.
- If the storage `PUT` fails with `403` (expired or mismatched signature), a `5xx` or a network error, the file
  is retried once from the start, with a new asset. Abandoned assets are cleaned up by the server.
- Validation errors (`4xx`) are never retried; the server's message, field errors and code are shown.

Ctrl-C stops the run: uploads in flight are cancelled and the summary is still printed. Press it again to quit
immediately.

#### Output

Default output is one line per file and a summary:

```text
skipped   photos/notes.bmp: unsupported file type .bmp
uploaded  photos/a.jpg -> Campaigns  https://xyz.i.optimole.com/w:auto/h:auto/q:auto/id:4f.../directUpload/a.jpg
failed    photos/b.jpg -> Campaigns/2024: create asset: The selected folder is invalid. (HTTP 422); if the folder does not exist yet, add --create-folders to create it (full-access keys only)

1 uploaded, 1 skipped, 1 failed (3 files; 2 hidden or excluded entries ignored).
```

With `--json`, each file is one object (`folder` is the destination path, `""` for the root; `folder_id` is
set when known; `key`, `url`, `error` and `code` appear when they apply), followed by a summary object:

```json
{"path":"photos/a.jpg","folder":"Campaigns","folder_id":12,"status":"uploaded","key":"4f0c...","url":"https://..."}
{"path":"photos/b.jpg","folder":"Campaigns","status":"failed","error":"create asset: ... (HTTP 403; upload_limit_reached; limit 50000, used 50000)","code":"upload_limit_reached"}
{"summary":{"total":2,"uploaded":1,"skipped":0,"failed":1,"ignored":0}}
```

`status` is `uploaded`, `skipped` or `failed` (`planned` in a dry run).

### `optimole folders [--parent root|ID] [--json]`

Lists the library's active folders. Without `--parent` it prints every folder with its full path (what
`--folder` expects); `--parent root` lists the top-level folders and `--parent 12` the children of folder 12.

```text
$ optimole folders
ID  PATH
12  Campaigns
15  Campaigns/2026
16  Campaigns/2026/Spring
```

Folder-scoped keys cannot list folders (the API answers 403); `upload --folder PATH` still works with them.

### `optimole login`, `optimole logout`

`login` reads the key from stdin: type or paste it at the prompt, or pipe it (`optimole login < key.txt`).
It checks the key with a cheap authenticated call before saving it; a folder-scoped key is accepted even though
it cannot list folders. `logout` deletes the saved key.

### `optimole version`

Prints the version, commit and build date.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Every selected file was uploaded (skipped files do not count). |
| 1 | At least one file failed, or there was nothing to upload. |
| 2 | Usage or configuration error (bad flags, a directory without `-r`, missing key, non-https URL). Nothing was uploaded. |
| 130 | Interrupted with Ctrl-C. |

## Limits

- 100 MB per file; the account's plan sets an upload limit (`upload_limit_reached` once it is reached).
- The API allows 300 requests per minute per account; each file takes 2 API requests plus the storage upload,
  so very large batches may see `429` answers, which the CLI waits out.
- A presigned upload URL is valid for 15 minutes.

## Development

```sh
gofmt -l .            # must print nothing
go vet ./...
go test -race ./...
go build -o optimole ./cmd/optimole
```

Standard library only. The tests run against an in-process fake of the API and of the storage endpoint
(`internal/testserver`); they never contact a real service. Releases are built by GoReleaser when a `v*` tag
is pushed (as a draft release to review and publish).
