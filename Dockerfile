FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /yt .

FROM denoland/deno:bin AS deno

FROM python:3.13-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir "yt-dlp[default]" \
 && useradd --system --uid 10001 --no-create-home yt \
 && mkdir /data && chown yt:yt /data
COPY --from=deno /deno /usr/local/bin/deno
COPY --from=build /yt /usr/local/bin/yt
ENV ADDR=:8080 DATA_DIR=/data HOME=/tmp DENO_DIR=/tmp/deno XDG_CACHE_HOME=/tmp PYTHONDONTWRITEBYTECODE=1
USER yt
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["python3", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz')"]
ENTRYPOINT ["/usr/local/bin/yt"]
