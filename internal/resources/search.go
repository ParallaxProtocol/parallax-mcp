package resources

import (
	"io/fs"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Doc is one embedded document: a docs page or a PIP.
type Doc struct {
	// Path identifies it: "guides/mining/pool" for a page, "pip-0004" for a PIP.
	Path        string
	Kind        string // "docs" or "pip"
	URI         string
	URL         string // where it is published
	Title       string
	Description string
	Text        string // the body without front matter
	plain       string // lower-cased searchable text
	headings    string
	terms       map[string]int
	length      int
}

var (
	indexOnce sync.Once
	index     []*Doc
	docFreq   map[string]int
	avgLen    float64
)

var (
	reTag     = regexp.MustCompile(`</?[A-Za-z][^>]*>`)
	reImport  = regexp.MustCompile(`(?m)^(import|export) .*$`)
	reHeading = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reLink    = regexp.MustCompile(`\]\([^)]*\)`)
	stopwords = map[string]bool{"the": true, "a": true, "an": true, "and": true, "or": true, "of": true, "to": true, "in": true, "is": true, "it": true, "on": true, "for": true, "how": true, "do": true, "i": true, "what": true, "with": true, "can": true, "my": true, "does": true, "are": true, "be": true, "by": true, "as": true, "at": true, "this": true, "that": true}
	docsSite  = "https://docs.parallaxprotocol.org/"
)

func buildIndex() {
	src := source()
	add := func(kind, dir, ext, uriPrefix string) {
		_ = fs.WalkDir(files, dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ext) {
				return nil
			}
			raw, err := files.ReadFile(p)
			if err != nil {
				return nil
			}
			rel := strings.TrimSuffix(strings.TrimPrefix(p, dir+"/"), ext)
			title, desc := frontMatter(p)
			body := stripFrontMatter(string(raw))
			doc := &Doc{Path: rel, Kind: kind, URI: uriPrefix + rel, Title: title, Description: desc, Text: body}
			if doc.Title == "" {
				doc.Title = fallbackTitle(rel, body)
			}
			switch kind {
			case "docs":
				doc.URL = docsSite + src.Docs.Version + "/" + rel
			case "pip":
				doc.URL = frontMatterField(p, "source")
				if doc.URL == "" {
					doc.URL = src.PIPs.Discussions
				}
			}
			var hs []string
			for _, m := range reHeading.FindAllStringSubmatch(body, -1) {
				hs = append(hs, m[1])
			}
			doc.headings = strings.ToLower(strings.Join(hs, " "))
			doc.plain = strings.ToLower(searchable(body))
			doc.terms = map[string]int{}
			for _, t := range tokenize(doc.plain) {
				doc.terms[t]++
				doc.length++
			}
			index = append(index, doc)
			return nil
		})
	}
	add("docs", "content/docs", ".mdx", uriDocs+"/")
	add("pip", "content/pips", ".md", uriPIPs+"/")
	sort.Slice(index, func(i, j int) bool { return index[i].URI < index[j].URI })

	docFreq = map[string]int{}
	total := 0
	for _, d := range index {
		total += d.length
		for t := range d.terms {
			docFreq[t]++
		}
	}
	if len(index) > 0 {
		avgLen = float64(total) / float64(len(index))
	}
}

// Docs returns every embedded document.
func Docs() []*Doc {
	indexOnce.Do(buildIndex)
	return index
}

// Lookup finds a document by path ("guides/mining/pool", "pip-0004",
// "PIP-4"), resource URI, or published docs URL.
func Lookup(ref string) *Doc {
	ref = strings.TrimSpace(ref)
	for _, prefix := range []string{uriDocs + "/", uriPIPs + "/", docsSite + source().Docs.Version + "/", docsSite} {
		ref = strings.TrimPrefix(ref, prefix)
	}
	ref = strings.Trim(strings.TrimSuffix(strings.TrimSuffix(ref, ".mdx"), ".md"), "/")
	low := strings.ToLower(ref)
	if n := strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(low, "pip-"), "pip"), "0"); n != "" && strings.HasPrefix(low, "pip") && isDigits(n) {
		low = "pip-" + strings.Repeat("0", max(0, 4-len(n))) + n
	}
	for _, d := range Docs() {
		if strings.ToLower(d.Path) == low {
			return d
		}
	}
	return nil
}

// Hit is a search result.
type Hit struct {
	Doc     *Doc
	Score   float64
	Excerpt string
}

// Search ranks documents for a query with BM25 over their text, boosted
// when the terms appear in the title, headings or path, and when the whole
// query appears verbatim. kind filters to "docs" or "pip" when not empty.
func Search(query, kind string, limit int) []Hit {
	docs := Docs()
	q := strings.ToLower(strings.TrimSpace(query))
	terms := uniq(tokenize(q))
	if len(terms) == 0 {
		return nil
	}
	const k1, b = 1.2, 0.75
	n := float64(len(docs))
	var hits []Hit
	for _, d := range docs {
		if kind != "" && d.Kind != kind {
			continue
		}
		var score float64
		matched := 0
		title := strings.ToLower(d.Title + " " + d.Description)
		for _, t := range terms {
			tf := float64(d.terms[t])
			inTitle := strings.Contains(title, t)
			inHeads := strings.Contains(d.headings, t)
			inPath := strings.Contains(strings.ToLower(d.Path), t)
			if tf == 0 && !inTitle && !inPath {
				continue
			}
			matched++
			idf := math.Log(1 + (n-float64(docFreq[t])+0.5)/(float64(docFreq[t])+0.5))
			score += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(d.length)/avgLen))
			if inTitle {
				score += 2.5 * idf
			}
			if inHeads {
				score += 1.0 * idf
			}
			if inPath {
				score += 1.5 * idf
			}
		}
		if matched == 0 {
			continue
		}
		// Favour documents that cover every term, and exact phrases.
		score *= float64(matched) / float64(len(terms))
		if len(terms) > 1 && strings.Contains(d.plain, q) {
			score *= 1.5
		}
		hits = append(hits, Hit{Doc: d, Score: score})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	for i := range hits {
		hits[i].Excerpt = excerpt(hits[i].Doc.Text, terms, 320)
	}
	return hits
}

// excerpt returns the paragraph-sized window of text that contains the most
// query terms.
func excerpt(text string, terms []string, size int) string {
	clean := searchable(text)
	low := strings.ToLower(clean)
	best, bestScore := 0, -1
	for _, t := range terms {
		for from := 0; ; {
			i := strings.Index(low[from:], t)
			if i < 0 {
				break
			}
			pos := from + i
			start := max(0, pos-size/3)
			window := low[start:min(len(low), start+size)]
			s := 0
			for _, u := range terms {
				if strings.Contains(window, u) {
					s++
				}
			}
			if s > bestScore {
				best, bestScore = start, s
			}
			from = pos + len(t)
			if from >= len(low) {
				break
			}
		}
	}
	if bestScore < 0 {
		best = 0
	}
	end := min(len(clean), best+size)
	for best > 0 && !isBoundary(clean[best-1]) {
		best--
	}
	out := strings.Join(strings.Fields(clean[best:end]), " ")
	if best > 0 {
		out = "…" + out
	}
	if end < len(clean) {
		out += "…"
	}
	return out
}

// fallbackTitle names a page without a title in its front matter the way
// Mintlify does, from its file name, unless it opens with a heading.
func fallbackTitle(rel, body string) string {
	if m := reHeading.FindStringSubmatch(body); m != nil && strings.HasPrefix(strings.TrimSpace(body), "#") {
		return strings.TrimSpace(m[1])
	}
	name := strings.ReplaceAll(path.Base(rel), "-", " ")
	if name == "" {
		return rel
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func isBoundary(c byte) bool { return c == ' ' || c == '\n' || c == '\t' }

// searchable removes markup that carries no meaning for search: imports,
// JSX components and link targets. Code blocks stay: commands and flags are
// what people search for.
func searchable(s string) string {
	s = reImport.ReplaceAllString(s, "")
	s = reTag.ReplaceAllString(s, " ")
	s = reLink.ReplaceAllString(s, "]")
	return s
}

func tokenize(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.'
	}) {
		f = strings.Trim(f, ".-")
		if len(f) < 2 || stopwords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

func uniq(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func stripFrontMatter(s string) string {
	if !strings.HasPrefix(s, "---") {
		return s
	}
	rest := s[3:]
	if i := strings.Index(rest, "\n---"); i >= 0 {
		return strings.TrimLeft(rest[i+4:], "\n")
	}
	return s
}

func frontMatterField(name, field string) string {
	raw, err := files.ReadFile(name)
	if err != nil || !strings.HasPrefix(string(raw), "---") {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		if strings.TrimSpace(line) == "---" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == field {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
