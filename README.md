# yt

small http api over [yt-dlp](https://github.com/yt-dlp/yt-dlp). go stdlib only, no deps.

- metadata for any site yt-dlp supports
- download jobs with live progress over sse (or polling)
- quality, codec, container, audio extraction, trimming, subtitles, sponsorblock, raw yt-dlp args
- x, bluesky, mastodon, streamable, twitch clips and direct files are resolved natively in go, no yt-dlp
- embed anything: put a link after the slash on x.mnl.rocks and paste it in discord, telegram, slack…
- jobs persisted to disk, survive restarts; files are deleted 5 minutes after they are ready

live at https://yt.mnl.rocks · docs for agents at [/llms.txt](https://yt.mnl.rocks/llms.txt) · spec at [/openapi.json](https://yt.mnl.rocks/openapi.json)

## usage

```bash
# one shot
curl -OJ 'https://yt.mnl.rocks/v1/download?url=https://youtu.be/jNQXAC9IVRw&quality=1080'

# job + progress
curl -X POST https://yt.mnl.rocks/v1/jobs -H 'content-type: application/json' \
  -d '{"url":"https://youtu.be/jNQXAC9IVRw","codec":"h264","container":"mp4"}'
curl -N https://yt.mnl.rocks/v1/jobs/{id}/events
curl -OJ https://yt.mnl.rocks/v1/jobs/{id}/file
```

| route | |
| --- | --- |
| `GET /v1/info?url=` | metadata, formats, playlist entries |
| `GET /v1/extractors` | supported sites |
| `POST /v1/jobs` | start a download |
| `GET /v1/jobs/{id}` | job state |
| `GET /v1/jobs/{id}/events` | job state over sse |
| `GET /v1/jobs/{id}/file` | result, range requests ok |
| `DELETE /v1/jobs/{id}` | cancel / delete |
| `GET /v1/download?url=` | sync download |
| `GET /v1/embed?url=` | embed metadata + player urls |
| `GET /healthz` | health + yt-dlp version |

## embed

```
https://x.mnl.rocks/https://x.com/jack/status/20
https://x.mnl.rocks/youtu.be/jNQXAC9IVRw
```

the page carries `og:video` / `twitter:player` tags, so chat apps unfurl it into a player. youtube reuses youtube's own player; everything else gets a progressive mp4 served from `x.mnl.rocks/media?url=`, proxied straight from the origin when a native extractor knows the site and produced by a job otherwise. `/oembed?url=` on the same host, `/embed/<url>` on the main host does the same.

## run

```bash
docker compose up -d --build   # listens on 127.0.0.1:8190
```

or locally with `yt-dlp`, `ffmpeg` and `deno` on the path:

```bash
go run .
```

## config

| env | default | |
| --- | --- | --- |
| `ADDR` | `:8080` | listen address |
| `API_KEY` | | require `Authorization: Bearer <key>` on `/v1` |
| `CORS_ORIGIN` | `*` | |
| `DATA_DIR` | `$TMPDIR/yt` | jobs and files |
| `MAX_JOBS` | `3` | concurrent downloads |
| `MAX_QUEUE` | `64` | queued jobs before 429 |
| `JOB_TTL` | `5m` | how long finished files are kept |
| `JOB_TIMEOUT` | `30m` | per job |
| `MAX_FILESIZE` | | passed to `--max-filesize` |
| `ALLOW_ARGS` | `true` | accept raw `args` |
| `ALLOW_PRIVATE` | `false` | allow urls resolving to private ips |
| `COOKIES_FILE` | | `--cookies` |
| `PROXY` | | `--proxy` |
| `YTDLP_ARGS` | | extra args for every call |
| `YTDLP_BIN` | `yt-dlp` | |
| `PUBLIC_URL` | request host | absolute base for links, e.g. `https://yt.mnl.rocks` |
| `EMBED_HOST` | | host that serves embed pages at `/<url>`, e.g. `x.mnl.rocks` |

## license

mit
