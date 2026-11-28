package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	maxWebBytes      = 5 << 20
	maxWebPages      = 100
	maxWebSeen       = 2000
	defaultWebPages  = 20
	defaultWebClient = 20 * time.Second
)

var hrefPattern = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)
var unsafeNameChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// WebSource crawls the configured seed URLs and, when MaxDepth > 0, follows
// same-host links. Each page becomes one HTML (or text) document.
type WebSource struct {
	Client   *http.Client
	MaxBytes int64
}

func (WebSource) Type() Type { return TypeWeb }

func (WebSource) Validate(cfg Config) error {
	if len(cfg.URLs) == 0 {
		return fmt.Errorf("web_crawler: urls is required")
	}
	for _, raw := range cfg.URLs {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("web_crawler: invalid url %q", raw)
		}
	}
	if cfg.MaxDepth < 0 || cfg.MaxDepth > 3 {
		return fmt.Errorf("web_crawler: max_depth must be between 0 and 3")
	}
	if cfg.MaxPages < 0 || cfg.MaxPages > maxWebPages {
		return fmt.Errorf("web_crawler: max_pages must be between 1 and %d", maxWebPages)
	}
	return nil
}

func (s WebSource) Pull(ctx context.Context, cfg Config, cursor map[string]string) (Batch, error) {
	client := s.Client
	if client == nil {
		client = &http.Client{
			Timeout: defaultWebClient,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return checkPublicHost(req.Context(), req.URL.Hostname(), cfg.AllowPrivate)
			},
		}
	}
	maxBytes := s.MaxBytes
	if maxBytes <= 0 {
		maxBytes = maxWebBytes
	}
	limit := cfg.MaxPages
	if limit <= 0 {
		limit = defaultWebPages
	}
	if limit > maxWebPages {
		limit = maxWebPages
	}
	seen := decodeSeen(cursor["seen"])

	type queued struct {
		url   string
		depth int
	}
	queue := make([]queued, 0, len(cfg.URLs))
	for _, raw := range cfg.URLs {
		queue = append(queue, queued{url: strings.TrimSpace(raw), depth: 0})
	}

	batch := Batch{NextCursor: map[string]string{}, Done: true}
	visited := make(map[string]bool)
	for len(queue) > 0 && len(batch.Items) < limit {
		next := queue[0]
		queue = queue[1:]
		if next.url == "" || visited[next.url] || seen[next.url] {
			continue
		}
		visited[next.url] = true

		parsed, err := url.Parse(next.url)
		if err != nil || parsed.Host == "" {
			continue
		}
		if err := checkPublicHost(ctx, parsed.Hostname(), cfg.AllowPrivate); err != nil {
			return Batch{}, fmt.Errorf("web_crawler: %s: %w", next.url, err)
		}
		page, contentType, links, err := s.fetch(ctx, client, next.url, maxBytes)
		if err != nil {
			// One broken page must not abort the whole sync.
			continue
		}
		seen[next.url] = true
		fileType, ext := webFileType(contentType)
		if fileType == "" {
			continue
		}
		batch.Items = append(batch.Items, Item{
			ExternalID: next.url,
			FileName:   pageFileName(parsed) + ext,
			FileType:   fileType,
			Content:    page,
		})
		if next.depth < cfg.MaxDepth {
			for _, link := range links {
				resolved := resolveLink(parsed, link)
				if resolved == "" {
					continue
				}
				other, err := url.Parse(resolved)
				if err != nil || other.Host != parsed.Host {
					continue
				}
				queue = append(queue, queued{url: resolved, depth: next.depth + 1})
			}
		}
	}
	batch.NextCursor["seen"] = encodeSeen(seen)
	return batch, nil
}

func (s WebSource) fetch(
	ctx context.Context, client *http.Client, target string, maxBytes int64,
) ([]byte, string, []string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", nil, err
	}
	req.Header.Set("User-Agent", "NexusFlow-Connector/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, "", nil, fmt.Errorf("page exceeds %d bytes", maxBytes)
	}
	body := string(raw)
	links := make([]string, 0, 16)
	for _, match := range hrefPattern.FindAllStringSubmatch(body, 64) {
		links = append(links, match[1])
	}
	return raw, resp.Header.Get("Content-Type"), links, nil
}

func webFileType(contentType string) (string, string) {
	lower := strings.ToLower(contentType)
	switch {
	case strings.Contains(lower, "html"), strings.Contains(lower, "xml"):
		return "html", ".html"
	case strings.Contains(lower, "text/plain"), strings.Contains(lower, "markdown"):
		return "txt", ".txt"
	default:
		return "", ""
	}
}

func resolveLink(base *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "javascript:") ||
		strings.HasPrefix(raw, "mailto:") || strings.HasPrefix(raw, "data:") {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	resolved := base.ResolveReference(ref)
	resolved.Fragment = ""
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return ""
	}
	return resolved.String()
}

func pageFileName(parsed *url.URL) string {
	name := strings.Trim(parsed.Path, "/")
	if name == "" {
		name = "index"
	}
	name = strings.ReplaceAll(name, "/", "-")
	name = unsafeNameChars.ReplaceAllString(name, "-")
	name = strings.TrimSuffix(name, path.Ext(name))
	name = strings.Trim(name, "-.")
	if len(name) > 80 {
		name = name[:80]
	}
	if name == "" {
		name = "page"
	}
	return name
}

func encodeSeen(seen map[string]bool) string {
	urls := make([]string, 0, len(seen))
	for target, ok := range seen {
		if ok {
			urls = append(urls, target)
		}
	}
	if len(urls) > maxWebSeen {
		urls = urls[len(urls)-maxWebSeen:]
	}
	raw, _ := json.Marshal(urls)
	return string(raw)
}

func decodeSeen(raw string) map[string]bool {
	out := make(map[string]bool)
	if raw == "" {
		return out
	}
	var urls []string
	if err := json.Unmarshal([]byte(raw), &urls); err != nil {
		return out
	}
	for _, target := range urls {
		out[target] = true
	}
	return out
}

// checkPublicHost rejects loopback/private/link-local targets unless the
// connector explicitly opts in. It runs on every fetch, including redirects'
// resolved URLs, because the seed URL is operator-controlled.
func checkPublicHost(ctx context.Context, host string, allowPrivate bool) error {
	if allowPrivate || host == "" {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return fmt.Errorf("private address %s is not allowed (set allow_private)", ip)
		}
	}
	return nil
}
