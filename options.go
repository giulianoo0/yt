package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Options struct {
	URL            string   `json:"url"`
	Format         string   `json:"format,omitempty"`
	Sort           string   `json:"sort,omitempty"`
	Quality        string   `json:"quality,omitempty"`
	Codec          string   `json:"codec,omitempty"`
	Container      string   `json:"container,omitempty"`
	AudioOnly      bool     `json:"audio_only,omitempty"`
	AudioFormat    string   `json:"audio_format,omitempty"`
	Start          string   `json:"start,omitempty"`
	End            string   `json:"end,omitempty"`
	Subtitles      []string `json:"subtitles,omitempty"`
	EmbedMetadata  bool     `json:"embed_metadata,omitempty"`
	EmbedThumbnail bool     `json:"embed_thumbnail,omitempty"`
	EmbedChapters  bool     `json:"embed_chapters,omitempty"`
	SponsorBlock   []string `json:"sponsorblock_remove,omitempty"`
	PlaylistItem   int      `json:"playlist_item,omitempty"`
	Args           []string `json:"args,omitempty"`
}

func optionsFromQuery(q url.Values) Options {
	list := func(k string) []string {
		var out []string
		for _, v := range q[k] {
			for _, s := range strings.Split(v, ",") {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	flag := func(k string) bool { b, _ := strconv.ParseBool(q.Get(k)); return b }
	item, _ := strconv.Atoi(q.Get("playlist_item"))
	return Options{
		URL:            q.Get("url"),
		Format:         q.Get("format"),
		Sort:           q.Get("sort"),
		Quality:        q.Get("quality"),
		Codec:          q.Get("codec"),
		Container:      q.Get("container"),
		AudioOnly:      flag("audio_only"),
		AudioFormat:    q.Get("audio_format"),
		Start:          q.Get("start"),
		End:            q.Get("end"),
		Subtitles:      list("subtitles"),
		EmbedMetadata:  flag("embed_metadata"),
		EmbedThumbnail: flag("embed_thumbnail"),
		EmbedChapters:  flag("embed_chapters"),
		SponsorBlock:   list("sponsorblock_remove"),
		PlaylistItem:   item,
		Args:           q["arg"],
	}
}

var (
	codecs       = map[string][2]string{"h264": {"[vcodec^=avc1]", "[acodec^=mp4a]"}, "vp9": {"[vcodec~='^vp0?9']", ""}, "av1": {"[vcodec^=av01]", ""}, "hevc": {"[vcodec~='^(hev|hvc)']", ""}}
	containers   = map[string]bool{"mp4": true, "webm": true, "mkv": true, "mov": true}
	audioFormats = map[string]bool{"best": true, "mp3": true, "m4a": true, "aac": true, "opus": true, "vorbis": true, "flac": true, "wav": true, "alac": true}
	timeRe       = regexp.MustCompile(`^\d+(:\d{1,2}){0,2}(\.\d+)?$`)
	langRe       = regexp.MustCompile(`^[\w.\-*]+$`)
)

var deniedArgs = map[string]bool{
	"-o": true, "--output": true, "-P": true, "--paths": true, "--exec": true, "--config-location": true, "--config-locations": true,
	"-a": true, "--batch-file": true, "--load-info-json": true, "--cookies": true, "--cookies-from-browser": true,
	"--download-archive": true, "--cache-dir": true, "--rm-cache-dir": true, "--plugin-dirs": true, "--ffmpeg-location": true,
	"--external-downloader": true, "--downloader": true, "--external-downloader-args": true, "--downloader-args": true,
	"--postprocessor-args": true, "--ppa": true, "--use-postprocessor": true, "--alias": true, "--enable-file-urls": true,
	"-U": true, "--update": true, "--update-to": true, "--print-to-file": true, "--netrc": true, "--netrc-location": true,
	"--netrc-cmd": true, "--print": true, "-O": true, "-s": true, "--simulate": true, "--skip-download": true,
	"-j": true, "-J": true, "--dump-json": true, "--dump-single-json": true, "-F": true, "--list-formats": true,
	"--batch": true, "--load-pages": true, "--write-pages": true, "--js-runtimes": true, "--remote-components": true,
}

func (o *Options) validate(cfg config) error {
	if o.Quality = strings.TrimSuffix(strings.ToLower(o.Quality), "p"); o.Quality != "" && o.Quality != "best" && o.Quality != "audio" {
		if n, err := strconv.Atoi(o.Quality); err != nil || n < 100 || n > 4320 {
			return errors.New("quality must be best, audio or a height like 1080")
		}
	}
	if o.Codec = strings.ToLower(o.Codec); o.Codec != "" && o.Codec != "any" {
		if _, ok := codecs[o.Codec]; !ok {
			return errors.New("codec must be any, h264, hevc, vp9 or av1")
		}
	}
	if o.Container = strings.ToLower(o.Container); o.Container != "" && !containers[o.Container] {
		return errors.New("container must be mp4, webm, mkv or mov")
	}
	if o.AudioFormat = strings.ToLower(o.AudioFormat); o.AudioFormat != "" && !audioFormats[o.AudioFormat] {
		return errors.New("audio_format must be best, mp3, m4a, aac, opus, vorbis, flac, wav or alac")
	}
	for _, t := range []string{o.Start, o.End} {
		if t != "" && !timeRe.MatchString(t) {
			return errors.New("start/end must be seconds or [hh:]mm:ss")
		}
	}
	for _, l := range append(append([]string{}, o.Subtitles...), o.SponsorBlock...) {
		if !langRe.MatchString(l) {
			return fmt.Errorf("invalid list value %q", l)
		}
	}
	if len(o.Args) > 0 {
		if !cfg.allowArgs {
			return errors.New("raw args are disabled on this instance")
		}
		for _, a := range o.Args {
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 2 && !isNumber(a) {
				return fmt.Errorf("combined short flags are not allowed: %s", a)
			}
			if deniedArgs[strings.SplitN(a, "=", 2)[0]] {
				return fmt.Errorf("arg not allowed: %s", a)
			}
		}
	}
	return checkURL(o.URL, cfg.allowPrivate)
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func checkURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be an absolute http(s) url")
	}
	if allowPrivate {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return fmt.Errorf("cannot resolve %s", u.Hostname())
	}
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() || ip.IP.IsLinkLocalMulticast() {
			return errors.New("private addresses are not allowed")
		}
	}
	return nil
}

func (o Options) formatSelector() string {
	if o.Format != "" {
		return o.Format
	}
	if o.AudioOnly || o.Quality == "audio" {
		if o.Codec == "h264" {
			return "ba[acodec^=mp4a]/ba/b"
		}
		return "ba/b"
	}
	h := ""
	if o.Quality != "" && o.Quality != "best" {
		h = "[height<=" + o.Quality + "]"
	}
	c := codecs[o.Codec]
	if c[0] == "" {
		return "bv*" + h + "+ba/b" + h + "/bv*+ba/b"
	}
	return fmt.Sprintf("bv*%s%s+ba%s/b%s%s/bv*%s+ba/b%s/bv*+ba/b", h, c[0], c[1], h, c[0], h, h)
}

func (o Options) args(cfg config) []string {
	a := []string{"-f", o.formatSelector()}
	if o.Sort != "" {
		a = append(a, "-S", o.Sort)
	}
	if o.AudioOnly || o.Quality == "audio" {
		a = append(a, "-x")
		if o.AudioFormat != "" {
			a = append(a, "--audio-format", o.AudioFormat)
		}
	} else if o.Container != "" {
		a = append(a, "--merge-output-format", o.Container, "--remux-video", o.Container)
	}
	if o.Start != "" || o.End != "" {
		end := o.End
		if end == "" {
			end = "inf"
		}
		start := o.Start
		if start == "" {
			start = "0"
		}
		a = append(a, "--download-sections", "*"+start+"-"+end)
	}
	if len(o.Subtitles) > 0 {
		a = append(a, "--write-subs", "--embed-subs", "--sub-langs", strings.Join(o.Subtitles, ","))
	}
	if o.EmbedMetadata {
		a = append(a, "--embed-metadata")
	}
	if o.EmbedThumbnail {
		a = append(a, "--embed-thumbnail")
	}
	if o.EmbedChapters {
		a = append(a, "--embed-chapters")
	}
	if len(o.SponsorBlock) > 0 {
		a = append(a, "--sponsorblock-remove", strings.Join(o.SponsorBlock, ","))
	}
	if o.PlaylistItem > 0 {
		a = append(a, "--yes-playlist", "--playlist-items", strconv.Itoa(o.PlaylistItem))
	} else {
		a = append(a, "--no-playlist")
	}
	if cfg.maxFilesize != "" {
		a = append(a, "--max-filesize", cfg.maxFilesize)
	}
	a = append(a, commonArgs(cfg)...)
	return append(a, o.Args...)
}

func commonArgs(cfg config) []string {
	a := []string{"--ignore-config", "--color", "never", "--cache-dir", cfg.dataDir + "/cache"}
	if cfg.cookies != "" {
		a = append(a, "--cookies", cfg.cookies)
	}
	if cfg.proxy != "" {
		a = append(a, "--proxy", cfg.proxy)
	}
	return append(a, cfg.extraArgs...)
}

func decodeOptions(r *http.Request) (Options, error) {
	var o Options
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		err := jsonDecode(r, &o)
		return o, err
	}
	if err := r.ParseForm(); err != nil {
		return o, err
	}
	return optionsFromQuery(r.Form), nil
}
