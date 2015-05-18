package main

import (
	"bytes"
	"encoding/xml"
	"html/template"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"
)

const (
	// defaultTitle is the site pages default title.
	defaultTitle = "Google I/O 2015"
	// descDefault is the default site description
	descDefault = "Google I/O 2015 brings together developers for an immersive," +
		" two-day experience focused on exploring the next generation of " +
		"technology, mobile and beyond. Join us online or in person May 28-29, " +
		"2015. #io15"
	// descExperiment is used when users share an experiment link on social.
	descExperiment = "Make music with instruments inspired by material design " +
		"for #io15. Play, record and share."
	// images for og:image meta tag
	ogImageDefault    = "images/io15-color.png"
	ogImageExperiment = "images/io15-experiment.png"

	// templatesDir is the templates directory path relative to config.Dir.
	templatesDir = "templates"
)

var (
	// tmplFunc is a map of functions available to all templates.
	tmplFunc = template.FuncMap{
		"safeHTML": func(v string) template.HTML { return template.HTML(v) },
		"url":      resourceURL,
	}
	// tmplCache caches HTML templates parsed in parseTemplate()
	tmplCache = &templateCache{templates: make(map[string]*template.Template)}

	// don't include these in sitemap
	skipSitemap = []string{
		"embed",
		"upgrade",
		"admin/",
		"debug/",
		"layout_",
		"error_",
	}
)

// templateCache is in-memory cache for parsed templates
type templateCache struct {
	sync.Mutex
	templates map[string]*template.Template
}

// templateData is the templates context
type templateData struct {
	Env          string
	ClientID     string
	Prefix       string
	Slug         string
	Canonical    string
	Title        string
	Desc         string
	OgTitle      string
	OgImage      string
	StartDateStr string
	// livestream youtube video IDs
	LiveIDs []string
}

type sitemap struct {
	XMLName xml.Name `xml:"http://www.sitemaps.org/schemas/sitemap/0.9 urlset"`
	Items   []*sitemapItem
}

type sitemapItem struct {
	XMLName xml.Name   `xml:"url"`
	Loc     string     `xml:"loc"`
	Freq    string     `xml:"changefreq,omitempty"`
	Mod     *time.Time `xml:"lastmod,omitempty"`
}

// renderTemplate executes a template found in name.html file
// using either layout_full.html or layout_partial.html as the root template.
// env is the app current environment: "dev", "stage" or "prod".
func renderTemplate(c context.Context, name string, partial bool, data *templateData) ([]byte, error) {
	tpl, err := parseTemplate(name, partial)
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = &templateData{}
	}
	if data.Env == "" {
		data.Env = config.Env
	}
	data.ClientID = config.Google.Auth.Client
	data.Slug = name
	data.Prefix = config.Prefix
	data.StartDateStr = config.Schedule.Start.In(config.Schedule.Location).Format(time.RFC3339)
	if data.Desc == "" {
		data.Desc = descDefault
	}
	if data.OgImage == "" {
		data.OgImage = ogImageDefault
	}
	if data.Title == "" {
		data.Title = pageTitle(tpl)
	}
	if data.OgTitle == "" {
		data.OgTitle = data.Title
	}

	var b bytes.Buffer
	if err := tpl.Execute(&b, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// parseTemplate creates a template identified by name, using appropriate layout.
// HTTP error layout is used for name arg prefixed with "error_", e.g. "error_404".
func parseTemplate(name string, partial bool) (*template.Template, error) {
	var layout string
	switch {
	case strings.HasPrefix(name, "error_"):
		layout = "layout_error.html"
	case name == "upgrade":
		layout = "layout_bare.html"
	case partial:
		layout = "layout_partial.html"
	default:
		layout = "layout_full.html"
	}

	key := name + layout
	tmplCache.Lock()
	defer tmplCache.Unlock()
	if t, ok := tmplCache.templates[key]; ok {
		return t, nil
	}

	t, err := template.New(layout).Delims("{%", "%}").Funcs(tmplFunc).ParseFiles(
		filepath.Join(config.Dir, templatesDir, layout),
		filepath.Join(config.Dir, templatesDir, name+".html"),
	)
	if err != nil {
		return nil, err
	}
	if !isDev() {
		tmplCache.templates[key] = t
	}
	return t, nil
}

// pageTitle executes "title" template and returns its result or defaultTitle.
func pageTitle(t *template.Template) string {
	b := new(bytes.Buffer)
	if err := t.ExecuteTemplate(b, "title", nil); err != nil || b.Len() == 0 {
		return defaultTitle
	}
	return b.String()
}

// resourceURL returns absolute path to a resource referenced by parts.
// For instance, given config.Prefix = "/myprefix", resourceURL("images", "img.jpg")
// returns "/myprefix/images/img.jpg".
// If the first part starts with http(s)://, it is the returned value.
func resourceURL(parts ...string) string {
	lp := strings.ToLower(parts[0])
	if strings.HasPrefix(lp, "http://") || strings.HasPrefix(lp, "https://") {
		return parts[0]
	}
	p := strings.Join(parts, "/")
	if !strings.HasPrefix(p, config.Prefix) {
		p = config.Prefix + "/" + p
	}
	return path.Clean(p)
}

// getSitemap returns a sitemap containing both templated pages
// and schedule session details.
func getSitemap(c context.Context, baseURL *url.URL) (*sitemap, error) {
	items := make([]*sitemapItem, 0)

	// templated pages
	root := filepath.Join(config.Dir, templatesDir)
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		ext := filepath.Ext(p)
		if p == root || fi.IsDir() || ext != ".html" {
			return nil
		}
		name := p[len(root)+1 : len(p)-len(ext)]
		for _, s := range skipSitemap {
			if strings.HasPrefix(name, s) {
				return nil
			}
		}
		freq := "weekly"
		if name == "home" {
			name = ""
			freq = "daily"
		}
		item := &sitemapItem{
			Loc:  baseURL.ResolveReference(&url.URL{Path: name}).String(),
			Freq: freq,
		}
		items = append(items, item)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// schedule
	sched, err := getLatestEventData(c, nil)
	if err != nil {
		return nil, err
	}
	mod := sched.modified.In(time.UTC)
	for id, _ := range sched.Sessions {
		u := baseURL.ResolveReference(&url.URL{Path: "schedule"})
		u.RawQuery = url.Values{"sid": {id}}.Encode()
		item := &sitemapItem{
			Loc:  u.String(),
			Mod:  &mod,
			Freq: "daily",
		}
		items = append(items, item)
	}

	return &sitemap{Items: items}, nil
}
