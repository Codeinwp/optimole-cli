# optimole

`optimole` uploads local files into an [Optimole](https://optimole.com) media library, into the folders you
choose, exports the library back as ZIP archives, builds the CDN URLs of its images, resized or
converted, and deletes images. It is a single binary with no dependencies.

```sh
optimole login                                   # paste your API key once
optimole upload -r ./photos --folder Campaigns   # photos/2024/a.jpg -> Campaigns/2024
optimole export start                            # export the library, download ./optimole-export-ID
optimole url KEY --width 800 --format webp       # the image's CDN URL, 800 px wide, as WebP
optimole delete --folder Campaigns/2025          # delete every image of a folder (asks first)
```

## Install

**macOS and Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/Codeinwp/optimole-cli/main/install.sh | sh
```

**Windows** (PowerShell)

```powershell
irm https://raw.githubusercontent.com/Codeinwp/optimole-cli/main/install.ps1 | iex
```

Then run `optimole login` and paste an API key from the Optimole dashboard.

The installer downloads the latest [release](https://github.com/Codeinwp/optimole-cli/releases) for your
system and CPU, checks it against the release's `checksums.txt`, and needs no administrator rights:

| System | Installs to | PATH |
|---|---|---|
| macOS, Linux | `/usr/local/bin` when it is writable, else `~/.local/bin` | Adds `~/.local/bin` to your shell profile (`.zshrc`, `.bashrc`/`.bash_profile`, fish config or `.profile`) when it is missing |
| Windows | `%LOCALAPPDATA%\Programs\optimole` | Adds the folder to your user PATH |

Run the same command again to update. Optional settings (environment variables): `OPTIMOLE_VERSION=v0.1.0`
for a specific release, `OPTIMOLE_INSTALL_DIR` for another folder, and `OPTIMOLE_NO_MODIFY_PATH=1` to leave
PATH and shell profiles alone. To uninstall, delete the `optimole` binary (and `optimole logout` first to
remove the saved key).

**Other ways**

- Download the archive for your system from the [releases page](https://github.com/Codeinwp/optimole-cli/releases),
  check it, and put `optimole` on your PATH:

  ```sh
  shasum -a 256 -c checksums.txt --ignore-missing
  tar -xzf optimole_darwin_arm64.tar.gz && mv optimole ~/.local/bin/
  ```

- With Go 1.23 or newer: `go install github.com/Codeinwp/optimole-cli/cmd/optimole@latest`

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
granted (and their subfolders): always pass `--folder` (or `--folder-id`). They cannot create folders
(`folder_creation_forbidden`), upload into the library root (`folder_required`), list folders, export, or
delete.

The API base URL defaults to `https://dashboard.optimole.com/api`; override it with `--api-url` or
`OPTIMOLE_API_URL` (for example to try another environment). Only `https` URLs are
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
straight to storage with the presigned `PUT` (never loading it into memory, and sending exactly the headers
the server signed: `Content-Type`, plus `Content-Disposition: attachment` for SVG), then completes it
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
  trimmed; leading and trailing slashes in `--folder` are ignored (the server trims them too). A path has at most 20 levels and each name
  at most 255 characters.
- Hidden entries (names starting with `.`) inside directories are ignored unless `--include-hidden`; a file
  you name explicitly is always considered.
- Symbolic links are never followed, including one named on the command line (with or without a trailing
  `/`); they are reported as skipped. Symlinks earlier in a path you type are resolved the way the shell
  resolves them (`link/..` is the parent of the link's target), and the directory found that way is the one
  that is walked.
- Each file is checked again right before it is sent: it must still be the same regular file, with the same
  size, that was listed. A file or directory swapped for a symlink (or anything else) in the meantime fails
  instead of uploading whatever the path now points to.
- Nothing is uploaded twice: files are compared by identity, so the same file named twice, a case alias
  (`a.jpg` and `A.JPG` on a case-insensitive disk) or a path through a symlinked parent counts once. Hard
  links to one file count once as well.

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

If the server refuses a destination (a missing folder or one not granted to a scoped key, `folder_required`,
`folder_creation_forbidden`), the other files for it are marked failed without more requests. An invalid key,
a full account (`upload_limit_reached`) or any other `403` (such as an inactive plan) stops the run; the files
not yet sent are reported as not attempted.

#### Retries

All waits use capped exponential backoff with jitter (or the `Retry-After` delay), up to 4 attempts.

- **Reserving an asset** (`POST /api/assets`) is repeated only when the server cannot have reserved one: on
  `429`, on `503 upload_service_unavailable`, and when the connection failed before the request was sent. A
  `502`/`504`, another `503` or a connection lost after sending fails the file, because a second reservation
  could end up published as a duplicate.
- **The storage `PUT`** is repeated to the *same* URL on a network error or a `5xx` while the URL is
  unexpired (the signature pins the asset, size and type, so this is harmless). Only a `403` (expired or
  mismatched signature) or an expired URL leads to one new reservation. Whenever an earlier `PUT` may have
  stored the bytes (its answer was lost), the CLI asks the server to complete the original asset before
  reserving another or giving up. Because storage may still be finishing that `PUT`, a "not uploaded"
  answer is asked again after about 2 s and 5 s; only after three such answers is a new asset reserved.
  One small window remains: if storage completes the original `PUT` more than about 7 s after the CLI
  stopped waiting for it, the server's hourly cleanup publishes that copy as well, next to the new one.
  Redirects from storage are refused, never followed.
- **Completion and folder listing** are repeated on `429`, `502`, `503`, `504` and network errors.
- Validation errors (other `4xx`) are never retried; the server's message, field errors and code are shown.

Ctrl-C stops the run, including while files are being listed or a dry run is printing: uploads in flight
are cancelled and the summary is still printed. Press it again to quit immediately.

If the output itself cannot be written (a full disk, for example), no new uploads start and the command exits
1: results that cannot be recorded are lost results.

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

### `optimole export <command>`

Exports the original files of the whole library and downloads them. An export covers every whitelisted
domain of the account, plus `directUpload` for the files uploaded straight to the library; the server zips
each domain into archives of about 1 GB. Exports need a full-access API key.

| Command | What it does |
|---|---|
| `optimole export start [flags]` | Starts an export, follows it and downloads each archive as soon as it is ready. If an export is already running, follows that one instead. `--no-wait` only starts it and prints its ID. |
| `optimole export download [ID\|latest] [flags]` | Downloads an existing export (default: the latest), waiting while it runs. Run it again to resume. |
| `optimole export status [ID\|latest] [--json]` | Shows the export's status and each domain's status, archives and size. |
| `optimole export list [--json]` | Lists the account's exports, newest first. |
| `optimole export delete ID` | Deletes a finished export and its archives on the server (local files are not touched). |

`start` and `download` take:

| Flag | Meaning |
|---|---|
| `-o`, `--output DIR` | Where the export folder goes (default: the current folder). |
| `--domain NAME` | Only download this domain's archives (repeatable, case-insensitive). The export itself always covers every domain. |
| `-c`, `--concurrency N` | Archives downloaded in parallel, 1-8 (default 2). |
| `--timeout DURATION` | Give up after this long, e.g. `90m` or `2h` (default: no limit). |
| `--json` | One JSON object per event and a final summary object. |

```sh
optimole export start                          # start, wait, download everything
optimole export start --no-wait                # start it now, download later
optimole export download                       # the latest export, into ./optimole-export-ID
optimole export download 42 -o /backups --domain example.com
```

#### Where the files go

```text
optimole-export-42/
  example.com/images-1.zip
  example.com/images-2.zip
  directUpload/images-1.zip
  export.json
```

Each domain gets a folder named after it. Domain names come from the server, so the CLI keeps only letters,
digits, `.`, `-` and `_` (anything else becomes `_`, so `*.example.com` becomes `_.example.com`), drops leading
and trailing dots, avoids Windows device names, and adds `-2`, `-3`... when two names differ only in case.
`export.json` maps each domain to its folder and lists its archives with their size and whether they are
downloaded. It holds no download URLs. The archives are not unpacked.

#### How downloads work

- While the export runs, the CLI checks it every 5 seconds (less often after a few minutes) and downloads each
  archive as soon as the server lists it.
- An archive is written to `images-N.zip.<size>.part` and renamed into place only when its size matches what
  the server listed and every file in it reads back with its checksum (files in a compression format Go's ZIP
  reader lacks are not read; Optimole's archives use deflate). A download that gets cut is resumed
  where it stopped; one that stalls for 2 minutes is restarted. An archive that fails the check (a resume that
  joined two versions of a rewritten archive, for example) is downloaded once more from scratch, then reported
  as failed. An archive whose size neither the server nor storage gives is not downloaded.
- One command at a time downloads into an export folder: it holds a lock on `.optimole-export.lock`, and a
  second command into the same folder stops with an error. Links planted inside the export folder are not
  followed.
- Running `download` again skips the archives already in place and resumes the partial ones. If the server
  listed an archive with a new size (it rewrote it), that archive is downloaded again.
- Download URLs are valid for a day; when storage refuses one, the CLI asks the server for a fresh one.
  Download URLs never get the API key and are never printed, and redirects are refused.
- Ctrl-C and `--timeout` stop only the CLI, never the export on the server, and the command prints how to
  resume (`optimole export download ID`). There is no way to cancel an export from the API.

#### Output

```text
Started export 42.
  example.com: running
  directUpload: running
Export 42: running
downloading optimole-export-42/example.com/images-1.zip (1.0 GB)
downloaded  optimole-export-42/example.com/images-1.zip (1.0 GB in 41s)
  directUpload: completed, no files
...
Export 42: completed

Export 42 completed: 2 downloaded, 0 already there, 0 failed (1.8 GB written) in optimole-export-42.
```

With `--json`:

```json
{"event":"started","id":42,"created":true}
{"event":"domain","domain":"example.com","status":"running","archives":0}
{"event":"export","id":42,"status":"running"}
{"event":"downloading","domain":"example.com","part":1,"path":"optimole-export-42/example.com/images-1.zip","size":1073741824}
{"event":"archive","domain":"example.com","part":1,"path":"optimole-export-42/example.com/images-1.zip","status":"downloaded","size":1073741824,"seconds":41}
{"summary":{"id":42,"status":"completed","dir":"optimole-export-42","downloaded":2,"present":0,"failed":0,"bytes":1932735283,"failed_domains":[],"interrupted":false,"timed_out":false}}
```

Each look at the export reports its domains first, then the export's own status. An archive's `status` is
`downloaded`, `present` (already on disk) or `failed` (with `error`). `created` is `false` when the command
follows an export that was already running.

An export is `queued`, `running`, `completed`, `failed` or `cancelled`. Once every domain has finished, it is
`failed` if one of them failed; the other domains' archives are still downloaded, and the command exits 1. An account keeps at most 5
exports (`export_limit_reached`); delete one with `optimole export delete ID`, or wait: finished exports are
deleted automatically 10 days after they finish.

### `optimole url [flags] KEY|URL...`

Prints the CDN URL of each asset key or Optimole URL, changed as the flags ask. Without arguments it reads one
key or URL per line from stdin, and also the JSON lines of `optimole upload --json` and `optimole url --json`,
so commands can be piped together.

```sh
# An asset's URL, 800 px wide, as WebP
optimole url 42cf6621995747f69a30d32f6a6c4f82 --width 800 --format webp

# The URLs of everything just uploaded, 1200 px wide
optimole upload -r ./photos --json | optimole url --width 1200

# Change a URL you already have (no API call, no key needed)
optimole url 'https://abc.i.optimole.com/w:auto/h:auto/q:auto/id:42cf…/directUpload/photo.jpg' --quality 60

# Chain changes
optimole url KEY --width 800 | optimole url --resize fill --height 400 --gravity top
```

| Flag | URL option | Meaning |
|---|---|---|
| `--width N\|auto`, `--height N\|auto` | `w:`, `h:` | Size in pixels. |
| `--resize TYPE` | `rt:` | `fit` (the default: keeps the aspect ratio and fits the size), `fill` (keeps the aspect ratio, fills the size and cuts what sticks out), `fill-down` (like `fill`, but an image smaller than the size is only cropped to its aspect ratio), `force` (ignores the aspect ratio) or `auto` (`fill` when the image and the size have the same orientation, else `fit`). |
| `--gravity POS` | `g:` | The part `fill` keeps: `center`, `top`, `bottom`, `left`, `right`, `top-left`, `top-right`, `bottom-left`, `bottom-right` or `smart`. |
| `--focus X:Y` | `g:fp:X:Y` | The point `fill` keeps, as fractions: `0:0` is the top left, `1:1` the bottom right. |
| `--quality Q` | `q:` | `auto`, `mauto`, `eco` or 1–100. |
| `--format F` | `f:` | `auto` removes `f:`, and the CDN serves each browser AVIF or WebP when it accepts them; or force `webp`, `jpg`, `png`, `gif`, `ico`, `jxl` or `heic`. |
| `--dpr N` | `dpr:` | Pixel density, 1–5. |
| `--download` | `att:1` | Serve the image as a download. |
| `--set NAME:VALUE` | any | Any other option the CDN reads, e.g. `--set bl:5` (blur) or `--set c:300:200:ce` (crop); repeatable. Unknown names are refused, because the CDN drops them silently. |
| `--unset NAME` | | Remove an option from the URL; repeatable. |
| `--reset` | | Start from `w:auto/h:auto/q:auto`, dropping the URL's options. |
| `-c`, `--concurrency N` | | Keys looked up in parallel, 1–16 (default 4). |
| `--json` | | One JSON object per input. |
| `--api-key`, `--api-url` | | See [Authentication](#authentication). |

#### How URLs are changed

- A key is looked up with `GET /api/assets/{key}`, which gives its URL on the account's delivery domain
  (custom domains included). A URL is changed locally: no API call and no key are needed.
- An option already in the URL is replaced, never added a second time: the CDN uses only the first copy of an
  option, so a second `w:` would do nothing. Options the flags do not name are kept.
- Options are written in the order the CDN applies them, so the same changes always give the same URL.
- Two flags that change the same option (`--width` and `--set w:…`, `--gravity` and `--focus`) are an error.
- Values are checked the way the CDN reads them. The CDN answers a bad `w`, `h` or `q` with an empty image,
  and ignores a cache buster (`cb:`) unless it looks like `abc.1` (3 or more characters, a dot, then more), so
  those are refused.
- `--format avif` is refused: the CDN drops `f:avif` and picks the format from the browser instead, which is
  what `--format auto` asks for.
- Files that are not images (their URLs have `/files/`, or CSS and JS assets) are printed as they are; image
  flags are an error for them.
- A URL the CDN would misread is refused: one with spaces or control characters (a newline would also print
  two URLs), or one whose origin path holds a segment the CDN takes for an option, such as `w:9` when the URL
  sets no `w:` of its own.

#### Output

One URL per line, in input order; a failed input is reported on stderr and the others are still printed. With
`--json`, each input gives one object: `input`, `url` and, when known, `key`, `mime_type`, `original_width`
and `original_height`; a failed input gives `input`, `error` and `code` (`invalid_input`, `not_found`,
`no_url` or `not_an_image`). A key the dashboard does not know and a key outside a folder-scoped key's folders
both answer `not_found`. A rejected API key stops the run. Messages never show the API keys in use (flag,
environment or saved key), and a line that looks like an API key is not echoed at all.

### `optimole delete`

Deletes images from the library for good: the CDN stops serving them, so pages that still use them show broken
images. Name the images with exactly one of:

| Flag | Deletes |
|---|---|
| `--keys FILE` | The images whose asset keys are in `FILE`, one per line (`-` reads stdin). Blank lines and spaces around a key are ignored, and a key listed twice counts once. |
| `--folder PATH` | Every image in this folder and all its subfolders, e.g. `Campaigns/2026` (the paths `optimole folders` prints; case does not matter). For a website's folder, also that website's images that are in no folder. |
| `--folder-id N` | The same, naming the folder by its ID. |
| `--site DOMAIN` | Every image of this website of the account, in any folder. `https://www.example.com/blog` means `example.com`. An internationalized domain must be given in its `xn--` form. |

| Flag | Meaning |
|---|---|
| `--yes` | Delete without asking. |
| `--json` | Print the result as one JSON object. |
| `--api-key`, `--api-url` | See [Authentication](#authentication). |

```sh
optimole delete --folder Campaigns/2025
optimole delete --site example.com
optimole delete --keys old-keys.txt
printf '%s\n' KEY1 KEY2 | optimole delete --keys - --yes   # in a script: no question
```

Deleting needs a full-access API key; folder-scoped keys cannot delete. There are no approval codes: before
deleting, the command says what it will delete and asks you to type the folder's path (its name, for a path
longer than a terminal line holds), the website's domain, or `delete` for keys, then Enter. Any other answer,
Ctrl-C or Ctrl-D deletes nothing. `--yes` skips the question; without a terminal to ask on (in a script, or with
`--keys -`, where stdin carries the keys) the command refuses to run without `--yes`.

Folders and websites are kept. A folder is looked up first (`GET /api/folders`), so an unknown folder, or a path
two folders share, is refused before anything is deleted; name the folder with `--folder-id` then. A path is only
matched against the whole folder list: if the list does not come back whole, the command stops.

- **Keys** are sent 100 per request (`DELETE /api/assets` with `{"keys": [...]}`) and deleted at once. The server
  checks every key of a request first: if one is not the account's, or is deleted already, it refuses the whole
  request (`422`, naming the key) and deletes none of it. The run stops at the first request that fails; the
  images of the requests before it stay deleted, and the command says how many keys to leave out to finish.
- **A folder or a website** (`{"folder": ID}` or `{"domain": "example.com"}`) is emptied in the background: the
  server answers at once with how many images go (`202`), and they leave the library within a few minutes.
  Images added afterwards are kept.
- **Retries.** A delete request is repeated only when the server cannot have received it (a `429`, or a
  connection that failed before sending). A repeat after a lost answer would start a second background deletion,
  or be refused for the keys it deleted. When an answer is lost (or is a `5xx`), the command says the images may
  be deleted, or the deletion may have started.

Output:

```text
Deleted 250 images.
Deleting 1520 images in the background. They leave the library within a few minutes.
Nothing to delete: the folder "Campaigns/2025" has no images.
```

With `--json`, one object: `{"scope":"keys","requested":250,"deleted":250}` for keys (after a failure also
`error`, and `maybe_deleted` when the outcome of the failed request is unknown), and
`{"scope":"folder","folder_id":15,"folder":"Campaigns/2025","deleting":1520}` or
`{"scope":"site","domain":"example.com","deleting":1520}` for a folder or a website.

### `optimole login`, `optimole logout`

`login` reads the key from stdin: type or paste it at the prompt, or pipe it (`optimole login < key.txt`;
the first non-blank line is used). It never takes the key as an argument, and does not repeat one given that
way.
It checks the key with a cheap authenticated call before saving it; a folder-scoped key is accepted even though
it cannot list folders. `logout` deletes the saved key.

### `optimole version`

Prints the version, commit and build date.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Every selected file was uploaded (skipped files do not count). For `export start` and `export download`: the export completed and every selected archive is on disk (`export start --no-wait`: the export was started, or one was already running). For `url`: every input was printed. For `delete`: the images were deleted (a folder or a website: are being deleted). |
| 1 | At least one file failed, there was nothing to upload, or the output could not be written. For exports: the export failed or was cancelled, an archive failed, `--timeout` passed, or the account keeps the most exports it may. For `url`: an input failed (the others are still printed), or the API key was rejected. For `delete`: the folder was not found, the API refused or failed, or the typed answer did not match. |
| 2 | Usage or configuration error (bad flags, a directory without `-r`, an unknown `--domain`, missing key, non-https URL, `delete` without `--yes` and without a terminal). Nothing was uploaded, downloaded or deleted. For `url`, a missing key stops the run at the first asset key. |
| 130 | Interrupted with Ctrl-C. |

## Limits

- 100 MB per file; the account's plan sets an upload limit (`upload_limit_reached` once it is reached).
  Reservations count towards it until they are completed or settled by the server's hourly cleanup, which
  is another reason the CLI avoids reserving an asset twice for one file.
- The API allows 300 requests per minute per account; each file takes 2 API requests plus the storage upload,
  and each key given to `optimole url` one, so very large batches may see `429` answers, which the CLI waits
  out. URLs given to `optimole url` take none.
- A presigned upload URL is valid for 15 minutes.
- `optimole delete --keys` sends 100 keys per request.
- An account runs one export at a time and keeps at most 5. Export archives are about 1 GB each (a part can be
  larger when its first files are), and their download URLs are valid for a day.

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

## License

Copyright (C) 2026 Vertigo Studio SA.

`optimole` is free software: you can redistribute it and/or modify it under the terms of the GNU General
Public License as published by the Free Software Foundation, either version 3 of the License, or (at your
option) any later version. It is distributed in the hope that it will be useful, but WITHOUT ANY WARRANTY;
see the [LICENSE](LICENSE) file for details.

