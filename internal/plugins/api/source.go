package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// A package source: a local folder or a folder in a git host, read file by
// file (PLUGIN_MARKETPLACE_RFC §18). Nothing read from it is trusted until
// SIGNATURE.json verifies and every file matches its signed hash.
type source interface {
	read(ctx context.Context, rel string, max int64) ([]byte, error)
	kind() string // "path" | "url"
	location() string
}

type localSource struct{ dir string }

func (s localSource) kind() string     { return "path" }
func (s localSource) location() string { return s.dir }

func (s localSource) read(_ context.Context, rel string, max int64) ([]byte, error) {
	p := filepath.Join(s.dir, filepath.FromSlash(rel))
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", rel)
	}
	return readCapped(p, max)
}

type urlSource struct {
	base   string // raw-file root, no trailing slash
	orig   string
	header [2]string
	client *http.Client
}

func (s *urlSource) kind() string     { return "url" }
func (s *urlSource) location() string { return s.orig }

func (s *urlSource) read(ctx context.Context, rel string, max int64) ([]byte, error) {
	segs := strings.Split(rel, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/"+strings.Join(segs, "/"), nil)
	if err != nil {
		return nil, err
	}
	if s.header[0] != "" {
		req.Header.Set(s.header[0], s.header[1])
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", rel, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s: %w", rel, errTooLarge)
	}
	return b, nil
}

// sourceClient fetches package files: https only (unless allowHTTP, tests),
// redirects stay on https, bounded time.
func sourceClient(allowHTTP bool) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && !allowHTTP {
				return errors.New("redirect to a non-https URL")
			}
			return nil
		},
	}
}

// parseSource turns what the user pasted into a source:
//   - an absolute local path;
//   - a GitHub folder link: https://github.com/<owner>/<repo>/tree/<ref>/<path>
//     (or the repo root, read at HEAD);
//   - a GitLab folder link: https://<host>/<group>/<project>/-/tree/<ref>/<path>;
//   - any other https URL: a folder whose files are <url>/<file>.
//
// <ref> is one path segment (a tag, a branch without "/", or a commit). The
// optional token is sent to private repos and never stored.
func parseSource(input, token string, allowHTTP bool) (source, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, errors.New("source is required")
	}
	if filepath.IsAbs(input) {
		return localSource{dir: filepath.Clean(input)}, nil
	}
	u, err := url.Parse(input)
	if err != nil || u.Host == "" {
		return nil, errors.New("source must be an absolute path or an https link")
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return nil, errors.New("links must be https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("link must not carry credentials, a query or a fragment")
	}
	p := strings.Trim(path.Clean(u.Path), "/")
	if strings.Contains(p, "..") {
		return nil, errors.New("bad path in link")
	}
	s := &urlSource{orig: input, client: sourceClient(allowHTTP)}
	switch {
	case strings.EqualFold(u.Host, "github.com"):
		parts := strings.Split(p, "/")
		if len(parts) < 2 {
			return nil, errors.New("GitHub link must name an owner and a repository")
		}
		ref, sub := "HEAD", ""
		if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
			ref, sub = parts[3], strings.Join(parts[4:], "/")
		} else if len(parts) > 2 {
			return nil, errors.New("GitHub link must be a repository or a /tree/<ref>/<folder> link")
		}
		s.base = "https://raw.githubusercontent.com/" + parts[0] + "/" + parts[1] + "/" + ref
		if sub != "" {
			s.base += "/" + sub
		}
		if token != "" {
			s.header = [2]string{"Authorization", "token " + token}
		}
	case strings.Contains(p, "/-/tree/") || strings.Contains(p, "/-/blob/"):
		i := strings.Index(p, "/-/")
		project, rest := p[:i], strings.Split(p[i+3:], "/")
		if len(rest) < 2 {
			return nil, errors.New("GitLab link must be a /-/tree/<ref>/<folder> link")
		}
		s.base = u.Scheme + "://" + u.Host + "/" + project + "/-/raw/" + rest[1]
		if sub := strings.Join(rest[2:], "/"); sub != "" {
			s.base += "/" + sub
		}
		if token != "" {
			s.header = [2]string{"PRIVATE-TOKEN", token}
		}
	case strings.Contains(p+"/", "/-/"):
		return nil, errors.New("GitLab link must be a /-/tree/<ref>/<folder> link")
	default:
		s.base = u.Scheme + "://" + u.Host + "/" + p
		s.base = strings.TrimSuffix(s.base, "/")
		if token != "" {
			s.header = [2]string{"Authorization", "Bearer " + token}
		}
	}
	return s, nil
}
