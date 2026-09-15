# yt

small http api over [yt-dlp](https://github.com/yt-dlp/yt-dlp). go stdlib only, no deps.

- metadata for any site yt-dlp supports
- download jobs with live progress over sse (or polling)
- quality, codec, container, audio extraction, trimming, subtitles, sponsorblock, raw yt-dlp args
- jobs persisted to disk, survive restarts, expire after a ttl

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
| `GET /healthz` | health + yt-dlp version |

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
| `JOB_TTL` | `1h` | finished job lifetime |
| `JOB_TIMEOUT` | `30m` | per job |
| `MAX_FILESIZE` | | passed to `--max-filesize` |
| `ALLOW_ARGS` | `true` | accept raw `args` |
| `ALLOW_PRIVATE` | `false` | allow urls resolving to private ips |
| `COOKIES_FILE` | | `--cookies` |
| `PROXY` | | `--proxy` |
| `YTDLP_ARGS` | | extra args for every call |
| `YTDLP_BIN` | `yt-dlp` | |

## sandbox

the container runs as a non-root user on a read-only rootfs with all capabilities dropped, `no-new-privileges`, pid/memory/cpu limits and its own bridge. `deploy/firewall.sh` drops egress from that bridge to private ranges and the host, so a compromised extractor can only reach the internet. `deploy/yt-update.timer` rebuilds daily to keep yt-dlp current.

## license

mit
