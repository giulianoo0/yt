package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

// NativeMedia is what a site-specific extractor found without yt-dlp.
type NativeMedia struct {
	Provider   string
	ID         string
	Title      string
	Author     string
	WebpageURL string
	Thumbnail  string
	Duration   float64
	Videos     []Variant // progressive files with audio
}

type Variant struct {
	URL         string
	Width       int
	Height      int
	Bitrate     int
	ContentType string
	Ext         string
}

// best returns the largest variant not taller than maxHeight (0 for any).
func (n *NativeMedia) best(maxHeight int) *Variant {
	var pick *Variant
	for i := range n.Videos {
		v := &n.Videos[i]
		if maxHeight > 0 && v.Height > maxHeight {
			continue
		}
		if pick == nil || v.Height > pick.Height || (v.Height == pick.Height && v.Bitrate > pick.Bitrate) {
			pick = v
		}
	}
	if pick == nil {
		for i := range n.Videos {
			v := &n.Videos[i]
			if pick == nil || v.Height < pick.Height {
				pick = v
			}
		}
	}
	return pick
}

func (n *NativeMedia) filename(v *Variant) string {
	name := strings.Join(strings.Fields(n.Title), " ")
	name = strings.NewReplacer("/", "⧸", "\\", "⧹", "\x00", "").Replace(name)
	if name == "" {
		name = n.Provider
	}
	if r := []rune(name); len(r) > 120 {
		name = string(r[:120])
	}
	ext := "mp4"
	if v != nil && v.Ext != "" {
		ext = v.Ext
	}
	if n.ID != "" {
		return name + " [" + n.ID + "]." + ext
	}
	return name + "." + ext
}

type nativeExtractor struct {
	name  string
	match func(u *url.URL) bool
	fetch func(ctx context.Context, u *url.URL) (*NativeMedia, error)
}

var natives = []nativeExtractor{
	{"Twitter", matchTwitter, fetchTwitter},
	{"Bluesky", matchBluesky, fetchBluesky},
	{"Streamable", matchStreamable, fetchStreamable},
	{"Twitch", matchTwitchClip, fetchTwitchClip},
	{"Mastodon", matchMastodon, fetchMastodon},
	{"Direct", matchDirect, fetchDirect},
}

var nativeClient = &http.Client{Timeout: 20 * time.Second}

// resolveNative tries the site-specific extractors. A failing extractor is
// logged and treated as a miss so callers fall back to yt-dlp.
func resolveNative(ctx context.Context, raw string) (*NativeMedia, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	u.Host = strings.ToLower(u.Host)
	for _, x := range natives {
		if !x.match(u) {
			continue
		}
		n, err := x.fetch(ctx, u)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("native %s: %s: %v", x.name, raw, err)
			}
			return nil, false
		}
		if n == nil || len(n.Videos) == 0 {
			return nil, false
		}
		if n.Provider == "" {
			n.Provider = x.name
		}
		if n.WebpageURL == "" {
			n.WebpageURL = raw
		}
		for i := range n.Videos {
			v := &n.Videos[i]
			if v.ContentType == "" {
				v.ContentType = "video/mp4"
			}
			if v.Ext == "" {
				v.Ext = extOf(v.ContentType, v.URL)
			}
		}
		return n, true
	}
	return nil, false
}

func extOf(ctype, u string) string {
	switch {
	case strings.Contains(ctype, "webm"):
		return "webm"
	case strings.Contains(ctype, "quicktime"):
		return "mov"
	case strings.HasPrefix(ctype, "audio/mpeg"):
		return "mp3"
	case strings.HasPrefix(ctype, "audio/"):
		return "m4a"
	}
	if p, err := url.Parse(u); err == nil {
		if e := strings.TrimPrefix(strings.ToLower(path.Ext(p.Path)), "."); e != "" && len(e) <= 4 {
			return e
		}
	}
	return "mp4"
}

func host(u *url.URL) string { return strings.TrimPrefix(u.Hostname(), "www.") }

func getJSON(ctx context.Context, method, u string, headers map[string]string, body io.Reader, v any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/json")
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	res, err := nativeClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("http %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(v)
}

// --- x / twitter: public syndication endpoint, permanent hotlinkable mp4s

var tweetRe = regexp.MustCompile(`^/(?:[^/]+/status(?:es)?|i/status|i/web/status)/(\d+)`)

func matchTwitter(u *url.URL) bool {
	switch host(u) {
	case "x.com", "twitter.com", "mobile.twitter.com", "mobile.x.com", "vxtwitter.com", "fxtwitter.com", "fixupx.com":
		return tweetRe.MatchString(u.Path)
	}
	return false
}

// tweetToken mirrors twitter's client: ((id/1e15)*pi).toString(36) without zeros and dots.
func tweetToken(id string) string {
	n, _ := strconv.ParseFloat(id, 64)
	x := n / 1e15 * math.Pi
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	ip := int64(x)
	frac := x - float64(ip)
	s := strconv.FormatInt(ip, 36)
	for i := 0; i < 12 && frac > 0; i++ {
		frac *= 36
		d := int(frac)
		s += string(digits[d])
		frac -= float64(d)
	}
	return strings.NewReplacer("0", "", ".", "").Replace(s)
}

type tweet struct {
	ID   string `json:"id_str"`
	Text string `json:"text"`
	User struct {
		Name       string `json:"name"`
		ScreenName string `json:"screen_name"`
	} `json:"user"`
	Media       []tweetMedia `json:"mediaDetails"`
	QuotedTweet *tweet       `json:"quoted_tweet"`
}

type tweetMedia struct {
	Type         string `json:"type"`
	MediaURL     string `json:"media_url_https"`
	OriginalInfo struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"original_info"`
	VideoInfo struct {
		DurationMillis int `json:"duration_millis"`
		Variants       []struct {
			Bitrate     int    `json:"bitrate"`
			ContentType string `json:"content_type"`
			URL         string `json:"url"`
		} `json:"variants"`
	} `json:"video_info"`
}

var twimgDims = regexp.MustCompile(`/(\d+)x(\d+)/`)

func fetchTwitter(ctx context.Context, u *url.URL) (*NativeMedia, error) {
	id := tweetRe.FindStringSubmatch(u.Path)[1]
	var t tweet
	if err := getJSON(ctx, http.MethodGet, "https://cdn.syndication.twimg.com/tweet-result?id="+id+"&token="+tweetToken(id)+"&lang=en", nil, nil, &t); err != nil {
		return nil, err
	}
	pick := &t
	var media *tweetMedia
	for _, m := range t.Media {
		if m.Type == "video" || m.Type == "animated_gif" {
			media = &m
			break
		}
	}
	if media == nil && t.QuotedTweet != nil {
		for _, m := range t.QuotedTweet.Media {
			if m.Type == "video" || m.Type == "animated_gif" {
				media, pick = &m, t.QuotedTweet
				break
			}
		}
	}
	if media == nil {
		return nil, errors.New("tweet has no video")
	}
	title := strings.TrimSpace(regexp.MustCompile(`\s*https://t\.co/\w+\s*$`).ReplaceAllString(pick.Text, ""))
	if title == "" {
		title = pick.User.Name + " on X"
	}
	n := &NativeMedia{
		ID: pick.ID, Title: title, Author: pick.User.Name + " (@" + pick.User.ScreenName + ")",
		WebpageURL: "https://x.com/" + pick.User.ScreenName + "/status/" + pick.ID,
		Thumbnail:  media.MediaURL, Duration: float64(media.VideoInfo.DurationMillis) / 1000,
	}
	if pick.ID == "" {
		n.ID = id
	}
	for _, v := range media.VideoInfo.Variants {
		if v.ContentType != "video/mp4" {
			continue
		}
		w, h := media.OriginalInfo.Width, media.OriginalInfo.Height
		if m := twimgDims.FindStringSubmatch(v.URL); m != nil {
			w, _ = strconv.Atoi(m[1])
			h, _ = strconv.Atoi(m[2])
		}
		n.Videos = append(n.Videos, Variant{URL: v.URL, Width: w, Height: h, Bitrate: v.Bitrate, ContentType: "video/mp4", Ext: "mp4"})
	}
	return n, nil
}

// --- bluesky: public appview + the original blob from the user's pds

var bskyRe = regexp.MustCompile(`^/profile/([^/]+)/post/([^/]+)`)

func matchBluesky(u *url.URL) bool { return host(u) == "bsky.app" && bskyRe.MatchString(u.Path) }

func fetchBluesky(ctx context.Context, u *url.URL) (*NativeMedia, error) {
	m := bskyRe.FindStringSubmatch(u.Path)
	at := "at://" + m[1] + "/app.bsky.feed.post/" + m[2]
	var res struct {
		Thread struct {
			Post struct {
				URI    string `json:"uri"`
				Author struct {
					DID         string `json:"did"`
					Handle      string `json:"handle"`
					DisplayName string `json:"displayName"`
				} `json:"author"`
				Record struct {
					Text  string `json:"text"`
					Embed struct {
						Video *struct {
							MimeType string `json:"mimeType"`
						} `json:"video"`
						Media *struct {
							Video *struct {
								MimeType string `json:"mimeType"`
							} `json:"video"`
						} `json:"media"`
					} `json:"embed"`
				} `json:"record"`
				Embed json.RawMessage `json:"embed"`
			} `json:"post"`
		} `json:"thread"`
	}
	if err := getJSON(ctx, http.MethodGet, "https://public.api.bsky.app/xrpc/app.bsky.feed.getPostThread?depth=0&parentHeight=0&uri="+url.QueryEscape(at), nil, nil, &res); err != nil {
		return nil, err
	}
	p := res.Thread.Post
	type videoView struct {
		Type        string `json:"$type"`
		CID         string `json:"cid"`
		Thumbnail   string `json:"thumbnail"`
		AspectRatio struct {
			Width, Height int
		} `json:"aspectRatio"`
	}
	var view videoView
	var wrapped struct {
		Media videoView `json:"media"`
	}
	json.Unmarshal(p.Embed, &view)
	if view.Type != "app.bsky.embed.video#view" {
		json.Unmarshal(p.Embed, &wrapped)
		view = wrapped.Media
	}
	if view.Type != "app.bsky.embed.video#view" || view.CID == "" {
		return nil, errors.New("post has no video")
	}
	ctype := "video/mp4"
	if v := p.Record.Embed.Video; v != nil && v.MimeType != "" {
		ctype = v.MimeType
	} else if md := p.Record.Embed.Media; md != nil && md.Video != nil && md.Video.MimeType != "" {
		ctype = md.Video.MimeType
	}
	title := strings.TrimSpace(p.Record.Text)
	if title == "" {
		title = p.Author.DisplayName + " on Bluesky"
	}
	author := p.Author.DisplayName
	if author == "" {
		author = p.Author.Handle
	} else {
		author += " (@" + p.Author.Handle + ")"
	}
	return &NativeMedia{
		ID: m[2], Title: title, Author: author, Thumbnail: view.Thumbnail,
		WebpageURL: "https://bsky.app/profile/" + p.Author.Handle + "/post/" + m[2],
		Videos: []Variant{{
			URL:   "https://bsky.social/xrpc/com.atproto.sync.getBlob?did=" + url.QueryEscape(p.Author.DID) + "&cid=" + url.QueryEscape(view.CID),
			Width: view.AspectRatio.Width, Height: view.AspectRatio.Height, ContentType: ctype,
		}},
	}, nil
}

// --- streamable

var streamableRe = regexp.MustCompile(`^/(?:[a-z]/[^/]+/)?([A-Za-z0-9]+)/?$`)

func matchStreamable(u *url.URL) bool {
	return host(u) == "streamable.com" && streamableRe.MatchString(u.Path)
}

func fetchStreamable(ctx context.Context, u *url.URL) (*NativeMedia, error) {
	id := streamableRe.FindStringSubmatch(u.Path)[1]
	var res struct {
		Status    int    `json:"status"`
		Title     string `json:"title"`
		Thumbnail string `json:"thumbnail_url"`
		Files     map[string]struct {
			URL      string  `json:"url"`
			Width    int     `json:"width"`
			Height   int     `json:"height"`
			Bitrate  int     `json:"bitrate"`
			Duration float64 `json:"duration"`
		} `json:"files"`
	}
	if err := getJSON(ctx, http.MethodGet, "https://api.streamable.com/videos/"+id, nil, nil, &res); err != nil {
		return nil, err
	}
	if res.Status != 2 {
		return nil, fmt.Errorf("video not ready (status %d)", res.Status)
	}
	n := &NativeMedia{ID: id, Title: res.Title, Thumbnail: res.Thumbnail}
	if strings.HasPrefix(n.Thumbnail, "//") {
		n.Thumbnail = "https:" + n.Thumbnail
	}
	for _, k := range []string{"mp4", "mp4-mobile"} {
		f, ok := res.Files[k]
		if !ok || f.URL == "" {
			continue
		}
		src := strings.TrimPrefix(f.URL, "https:")
		if strings.HasPrefix(src, "//") {
			src = "https:" + src
		}
		n.Duration = f.Duration
		n.Videos = append(n.Videos, Variant{URL: src, Width: f.Width, Height: f.Height, Bitrate: f.Bitrate, ContentType: "video/mp4", Ext: "mp4"})
	}
	return n, nil
}

// --- twitch clips: public gql client id, signed mp4 valid ~24h

var (
	twitchClipRe  = regexp.MustCompile(`^/([A-Za-z0-9_-]+)/?$`)
	twitchClipRe2 = regexp.MustCompile(`^/[^/]+/clip/([A-Za-z0-9_-]+)`)
)

func twitchSlug(u *url.URL) string {
	switch host(u) {
	case "clips.twitch.tv":
		if m := twitchClipRe.FindStringSubmatch(u.Path); m != nil {
			return m[1]
		}
	case "twitch.tv", "m.twitch.tv":
		if m := twitchClipRe2.FindStringSubmatch(u.Path); m != nil {
			return m[1]
		}
	}
	return ""
}

func matchTwitchClip(u *url.URL) bool { return twitchSlug(u) != "" }

func fetchTwitchClip(ctx context.Context, u *url.URL) (*NativeMedia, error) {
	slug := twitchSlug(u)
	q := fmt.Sprintf(`{"query":"{ clip(slug:%q){ id title durationSeconds thumbnailURL broadcaster{displayName} videoQualities{quality frameRate sourceURL} playbackAccessToken(params:{platform:\"web\",playerBackend:\"mediaplayer\",playerType:\"site\"}){signature value} } }"}`, slug)
	var res struct {
		Data struct {
			Clip *struct {
				ID          string  `json:"id"`
				Title       string  `json:"title"`
				Duration    float64 `json:"durationSeconds"`
				Thumbnail   string  `json:"thumbnailURL"`
				Broadcaster struct {
					DisplayName string `json:"displayName"`
				} `json:"broadcaster"`
				Qualities []struct {
					Quality   string  `json:"quality"`
					FrameRate float64 `json:"frameRate"`
					SourceURL string  `json:"sourceURL"`
				} `json:"videoQualities"`
				Token struct {
					Signature string `json:"signature"`
					Value     string `json:"value"`
				} `json:"playbackAccessToken"`
			} `json:"clip"`
		} `json:"data"`
	}
	if err := getJSON(ctx, http.MethodPost, "https://gql.twitch.tv/gql", map[string]string{"Client-ID": "kimne78kx3ncx6brgo4mv6wki5h1ko", "Content-Type": "application/json"}, strings.NewReader(q), &res); err != nil {
		return nil, err
	}
	c := res.Data.Clip
	if c == nil {
		return nil, errors.New("clip not found")
	}
	n := &NativeMedia{ID: slug, Title: c.Title, Author: c.Broadcaster.DisplayName, Thumbnail: c.Thumbnail, Duration: c.Duration, WebpageURL: "https://clips.twitch.tv/" + slug}
	for _, qv := range c.Qualities {
		h, _ := strconv.Atoi(qv.Quality)
		n.Videos = append(n.Videos, Variant{
			URL:   qv.SourceURL + "?sig=" + url.QueryEscape(c.Token.Signature) + "&token=" + url.QueryEscape(c.Token.Value),
			Width: h * 16 / 9, Height: h, Bitrate: int(qv.FrameRate), ContentType: "video/mp4", Ext: "mp4",
		})
	}
	return n, nil
}

// --- mastodon (any instance): /@user/123 or /users/x/statuses/123

var mastodonRe = regexp.MustCompile(`^/(?:@[^/]+|users/[^/]+/statuses)/(\d+)/?$`)

func matchMastodon(u *url.URL) bool { return mastodonRe.MatchString(u.Path) }

func fetchMastodon(ctx context.Context, u *url.URL) (*NativeMedia, error) {
	id := mastodonRe.FindStringSubmatch(u.Path)[1]
	var st struct {
		URL     string `json:"url"`
		Content string `json:"content"`
		Account struct {
			DisplayName string `json:"display_name"`
			Acct        string `json:"acct"`
		} `json:"account"`
		Media []struct {
			Type    string `json:"type"`
			URL     string `json:"url"`
			Preview string `json:"preview_url"`
			Meta    struct {
				Original struct {
					Width    int     `json:"width"`
					Height   int     `json:"height"`
					Duration float64 `json:"duration"`
					Bitrate  int     `json:"bitrate"`
				} `json:"original"`
			} `json:"meta"`
		} `json:"media_attachments"`
	}
	if err := getJSON(ctx, http.MethodGet, u.Scheme+"://"+u.Host+"/api/v1/statuses/"+id, nil, nil, &st); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(stripTags(st.Content))
	if title == "" {
		title = st.Account.DisplayName + " on " + host(u)
	}
	n := &NativeMedia{Provider: host(u), ID: id, Title: title, Author: st.Account.DisplayName + " (@" + st.Account.Acct + ")", WebpageURL: st.URL}
	for _, m := range st.Media {
		if m.Type != "video" && m.Type != "gifv" {
			continue
		}
		n.Thumbnail, n.Duration = m.Preview, m.Meta.Original.Duration
		n.Videos = append(n.Videos, Variant{URL: m.URL, Width: m.Meta.Original.Width, Height: m.Meta.Original.Height, Bitrate: m.Meta.Original.Bitrate, ContentType: "video/mp4", Ext: "mp4"})
		break
	}
	if len(n.Videos) == 0 {
		return nil, errors.New("status has no video")
	}
	return n, nil
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string {
	s = strings.NewReplacer("</p>", "\n", "<br>", "\n", "<br/>", "\n", "<br />", "\n").Replace(s)
	s = tagRe.ReplaceAllString(s, "")
	s = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// --- direct media files

func matchDirect(u *url.URL) bool {
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".mp4", ".m4v", ".webm", ".mov":
		return true
	}
	return false
}

func fetchDirect(ctx context.Context, u *url.URL) (*NativeMedia, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	res, err := nativeClient.Do(req)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	ctype := strings.ToLower(res.Header.Get("Content-Type"))
	if res.StatusCode != 200 || !strings.HasPrefix(ctype, "video/") {
		return nil, fmt.Errorf("not a video (%d %s)", res.StatusCode, ctype)
	}
	name := strings.TrimSuffix(path.Base(u.Path), path.Ext(u.Path))
	if name, err = url.PathUnescape(name); err != nil {
		name = path.Base(u.Path)
	}
	return &NativeMedia{Provider: host(u), Title: name, WebpageURL: u.String(), Videos: []Variant{{URL: u.String(), ContentType: ctype}}}, nil
}
