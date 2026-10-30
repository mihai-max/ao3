# ao3 archive reader

Search, read and download the EPUBs inside the "things" AO3 torrent
(`magnet:?xt=urn:btih:62ff22c20350695cbac69062b9b9567d05310be1`) without
extracting a single zip. One self-contained Docker image, published to
`ghcr.io/mihai-max/ao3`, with an Unraid template in [`unraid/ao3.xml`](unraid/ao3.xml).

- Indexes every EPUB's title, author, fandom, rating, tags, stats and summary
  into one SQLite file with full-text search (SQLite is built into the binary).
- Reads chapters in the browser, sanitized, with images, light and dark mode.
- Downloads the original `.epub` byte for byte.
- Nothing is stored in the image or a Docker volume: the index is a file in a
  host folder you choose.

## What is in the torrent

Read from the torrent's metadata (hybrid v1/v2 torrent, created 2026-08-30 by
`go.torrent`, 32 MiB pieces):

| | |
|---|---|
| Files | 107 |
| Total size | 801,443,431,445 bytes = **746.4 GiB** |
| `.zip` files | 105, holding all of the data |
| `.txt` files | 2 tiny notes: `zip_order.txt`, `extract_from_zips.txt` |

The zips are `queue01.zip` … `queue94.zip` (0.4 to 26 GiB each) plus
`sept21_update.zip`, `sept21_2update.zip`, `july2021feb192022.zip`,
`dec2021jan2022update.zip`, `feb2022_update.zip`, `oct_dec_2022.zip`,
`jan_may_2023.zip`, `gap_filled.zip`, `extra_stories.zip`, `login.zip` and
`pic_stories.zip`.

The torrent metadata does not show what is inside each zip. The app assumes
the zips hold `.epub` files in AO3's download format, which is what the
names suggest. Check that on your copy before indexing (see
[First check your data](#first-check-your-data)).

## Do you need to decompress anything?

**No.** A zip has a directory at its end that says where every file starts.
The indexer reads that directory once per zip, and the reader later jumps
straight to one EPUB and inflates only that one. The zips stay exactly as the
torrent client wrote them, mounted read-only, and keep seeding.

If you extracted them anyway you would need roughly another 750 to 800 GiB.
For around 20 million EPUBs that is also 20 million small files, which is
slow to create, back up or scan on any filesystem, and on Unraid's array
especially.

## How big will it be?

For about **20 million EPUBs**:

| What | Size |
|---|---|
| The torrent's zips, kept as they are | 746 GiB |
| Search index, default settings | **about 28 GB** |
| Search index with searchable summaries | about 35 GB |
| Extra space for extraction | none |
| Memory while serving | well under 1 GB |

The index figures are measured, not guessed. A benchmark wrote 300,000 works
with AO3-like field lengths and word frequencies and got 1,426 bytes per work
by default and 1,738 with summary search. Reproduce it with:

```bash
AO3_SIZE_BENCH=300000 go test ./internal/index -run SizeEstimate -v
```

Indexing is a one-time job. It reads all 746 GiB once, so on an Unraid array
expect a few hours, mostly limited by disk read speed. It is resumable. If
the container stops, finished zips are kept and an interrupted zip is redone
from its start.

## Where things are stored

| Inside the container | What | Unraid default | Docker Compose default |
|---|---|---|---|
| `/data` (read-only) | the torrent's 105 zips and 2 txt files | `/mnt/user/data/things` | `./zips` |
| `/index` (read-write) | `ao3.db` plus `ao3.db-wal` and `ao3.db-shm` | `/mnt/user/appdata/ao3` | `./index` |

So on Unraid the index is **`/mnt/user/appdata/ao3/ao3.db`**. If your appdata
lives on a small cache SSD, point the Index folder at a disk share instead,
for example `/mnt/user/data/ao3-index`. An SSD makes search faster, so use one
if it has about 35 GB free.

## Install on Unraid

1. Download the torrent to a share, for example `/mnt/user/data/things`.
2. Open the Unraid terminal and save the template into your user templates:

   ```bash
   wget -O /boot/config/plugins/dockerMan/templates-user/my-ao3-archive.xml https://raw.githubusercontent.com/mihai-max/ao3/HEAD/unraid/ao3.xml
   ```

3. In the web UI, open **Docker**, click **Add Container**, and pick
   **ao3-archive** from the template list.
4. Set **Data folder** to where the zips are and **Index folder** to where the
   index should go, then click **Apply**.
5. Open the WebUI. The status page shows indexing progress per zip. Search
   works while indexing runs and fills in as it goes.

The container starts as root only long enough to give the Index folder to
`PUID:PGID` (99:100 by default, Unraid's nobody:users), then runs as that
user.

## Run with Docker or Docker Compose

```bash
docker run -d --name ao3 -p 8080:8080 \
  -e PUID=1000 -e PGID=1000 \
  -v /path/to/things:/data:ro \
  -v /path/to/ao3-index:/index \
  ghcr.io/mihai-max/ao3:latest
```

Or put the zips in `./zips` next to [`docker-compose.yml`](docker-compose.yml) and run:

```bash
docker compose up -d
```

Then open `http://<server>:8080/`.

## First check your data

Before the first full index, confirm the zips look as expected:

```bash
docker run --rm -v /mnt/user/data/things:/data:ro ghcr.io/mihai-max/ao3:latest inspect /data
```

```bash
docker run --rm -v /mnt/user/data/things:/data:ro ghcr.io/mihai-max/ao3:latest inspect -n 3 /data/queue85.zip
```

The first command prints both txt notes and the zip order. The second prints
the member count, file extensions, compression method and the parsed metadata
of the first EPUBs. `queue85.zip` is the smallest zip at 437 MiB. If the
extensions are not `.epub`, or `ao3 preface found` is false, open an issue
with that output.

## Searching

| Type | Finds |
|---|---|
| `coffee shop` | works with both words |
| `"coffee shop"` | the exact phrase |
| `angst -fluff` | angst but not fluff |
| `drarr*` | words starting with drarr |
| `12345` | AO3 work 12345 directly |

The field menu limits a search to title, author, fandom, tags or summary.
Tags cover relationships, characters, additional tags, warnings, category,
rating and series. Tag and author links on a work page run those searches.
Summary search only works when `AO3_FTS_SUMMARY=true` was set before the
index was first built.

When the same work appears in several zips, for example in a queue zip and
again in a later update zip, the copy with the newest AO3 "Updated" date wins.
On a tie, the zip listed later in `zip_order.txt` wins.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `PUID`, `PGID` | unset | run as this user and group, and own the index folder |
| `UMASK` | unset | file creation mask after switching user |
| `AO3_DATA` | `/data` | folder with the zips |
| `AO3_INDEX` | `/index/ao3.db` | index file |
| `AO3_ADDR` | `:8080` | listen address |
| `AO3_AUTO_INDEX` | `true` | index new or unfinished zips at start |
| `AO3_INDEX_WORKERS` | `0` | EPUB parser threads, 0 means all CPUs |
| `AO3_FTS_SUMMARY` | `false` | make summaries searchable, applies to a new index only |

Commands: `serve` (default), `index [--only queue85.zip]`, `inspect`,
`healthcheck`, `version`. To rebuild the index from scratch, stop the
container, delete the files in the Index folder and start it again.

## Publishing to ghcr.io

[`.github/workflows/ghcr.yml`](.github/workflows/ghcr.yml) runs the tests and
pushes a multi-arch image (amd64 and arm64) on every push.

| Push to | Image tag |
|---|---|
| the default branch | `latest` |
| any branch | the branch name, with `/` turned into `-` |
| a `v1.2.3` tag | `1.2.3` |
| any commit | `sha-<short sha>` |

A new GHCR package is private. Either make it public under the repository's
**Packages** settings, or run `docker login ghcr.io` on the Unraid server
with a token that has `read:packages`.

## Security

There is no login. Keep it on your LAN or behind a VPN or an authenticating
reverse proxy. EPUB content is treated as untrusted. Chapter HTML goes through
an allowlist sanitizer, the pages send a Content Security Policy that blocks
all scripts, and only images are served from inside an EPUB.

## Development

```bash
go test -race ./...
```

```bash
go run ./cmd/mkfixture ./sample && go run ./cmd/ao3 serve --data ./sample --index ./index/ao3.db --addr :8080
```

`mkfixture` writes two small zips of synthetic AO3-style EPUBs, including a
work that appears in both, so the whole app can be tried without the torrent.

Layout:

| Path | Purpose |
|---|---|
| `cmd/ao3` | the single binary |
| `internal/archive` | zip discovery, ordering, direct reads by offset |
| `internal/epub` | EPUB parsing and AO3 preface metadata |
| `internal/index` | SQLite schema, FTS5 search, the parallel resumable indexer |
| `internal/web` | HTTP handlers, chapter rendering and sanitizing, templates |
| `internal/privdrop` | PUID and PGID handling |
| `unraid/` | Unraid template and icon |
