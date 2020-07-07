// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package md implements a parser for CLaaT. It expects, as input, the output of running a Markdown file through
// the Devsite Markdown processor.
package md

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/googlecodelabs/tools/claat/parser"
	"github.com/googlecodelabs/tools/claat/types"
	"github.com/googlecodelabs/tools/claat/util"
	"gopkg.in/russross/blackfriday.v2"
)

// Metadata constants for the YAML header
const (
	MetaAuthors          = "authors"
	MetaBadgePath        = "badge path"
	MetaSummary          = "summary"
	MetaID               = "id"
	MetaCategories       = "categories"
	MetaEnvironments     = "environments"
	MetaStatus           = "status"
	MetaFeedbackLink     = "feedback link"
	MetaAnalyticsAccount = "analytics account"
	MetaTags             = "tags"
)

const (
	metaSep         = ":"           // step instruction format, key:value
	metaDuration    = "duration"    // step duration instruction
	metaEnvironment = "environment" // step environment instruction
	metaTagImport   = "import"      // import remote resource instruction

	// possible content of special header nodes in lower case.
	headerLearn = "what you'll learn"
	headerCover = "what we've covered"
	headerFAQ   = "frequently asked questions"
)

var (
	importsTagRegexp           = regexp.MustCompile("^<<([^<>()]+.md)>>\\s*$")
	convertedImportsDataPrefix = "__unsupported_import_zmcgv2epyv="
	convertedImportsPrefix     = []byte("<!--" + convertedImportsDataPrefix)
	convertedImportsSuffix     = []byte("-->")
)

var metadataRegexp = regexp.MustCompile(`(.+?):(.+)`)
var languageRegexp = regexp.MustCompile(`language-(.+)`)

var (
	// durFactor is a slice of duration parser multipliers,
	// ordered after the usage in codelab docs
	durFactor = []time.Duration{time.Hour, time.Minute, time.Second}

	// textCleaner replaces "smart quotes" and other unicode runes
	// with their respective ascii versions.
	textCleaner = strings.NewReplacer(
		"\u2019", "'", "\u201C", `"`, "\u201D", `"`, "\u2026", "...",
		"\u00A0", " ", "\u0085", " ",
	)
)

var (
	// ErrForbiddenFragmentImports means importing another markdown file in a markdown fragment is forbidden.
	ErrForbiddenFragmentImports = errors.New("importing content in a fragment is forbidden")
	// ErrForbiddenFragmentSteps means declaring extra codelabs step in a markdown fragment is forbidden.
	ErrForbiddenFragmentSteps = errors.New("defining steps in a fragment is forbidden")
)

// init registers this parser so it is available to CLaaT.
func init() {
	parser.Register("md", &Parser{})
}

// Parser is a Markdown parser.
type Parser struct {
}

// Parse parses a codelab written in Markdown.
func (p *Parser) Parse(r io.Reader, opts parser.Options) (*types.Codelab, error) {
	// Convert Markdown to HTML for easy parsing.
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	b = renderToHTML(b)
	h := bytes.NewBuffer(b)
	doc, err := html.Parse(h)
	if err != nil {
		return nil, err
	}
	// Parse the markup.
	return parseMarkup(doc, opts)
}

// ParseFragment parses a codelab fragment written in Markdown.
func (p *Parser) ParseFragment(r io.Reader) ([]types.Node, error) {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	b = renderToHTML(b)
	h := bytes.NewBuffer(b)
	doc, err := html.Parse(h)
	if err != nil {
		return nil, err
	}

	return parsePartialMarkup(doc)
}

func parsePartialMarkup(root *html.Node) ([]types.Node, error) {
	body := findAtom(root, atom.Body)
	if body == nil {
		return nil, fmt.Errorf("document without a body")
	}

	ds := newDocState()
	ds.step = ds.clab.NewStep("fragment")
	for ds.cur = body.FirstChild; ds.cur != nil; ds.cur = ds.cur.NextSibling {
		switch {
		case ds.cur.DataAtom == atom.H1:
			return nil, ErrForbiddenFragmentSteps
		case ds.cur.DataAtom == atom.H2:
			return nil, ErrForbiddenFragmentSteps
		}

		parseTop(ds)
	}

	finalizeStep(ds.step)
	if hasImport(ds) {
		return nil, ErrForbiddenFragmentImports
	}

	return ds.step.Content.Nodes, nil
}

type docState struct {
	clab     *types.Codelab // codelab and its metadata
	totdur   time.Duration  // total codelab duration
	survey   int            // last used survey ID
	step     *types.Step    // current codelab step
	lastNode types.Node     // last appended node
	env      []string       // current enviornment
	cur      *html.Node     // current HTML node
	stack    []*stackItem   // cur and flags stack
}

type stackItem struct {
	cur *html.Node
}

func newDocState() *docState {
	return &docState{
		clab: types.NewCodelab(),
	}
}

func (ds *docState) push(cur *html.Node) {
	if cur == nil {
		cur = ds.cur
	}
	ds.stack = append(ds.stack, &stackItem{ds.cur})
	ds.cur = cur
}

func (ds *docState) pop() {
	n := len(ds.stack)
	if n == 0 {
		return
	}
	item := ds.stack[n-1]
	ds.stack = ds.stack[:n-1]
	ds.cur = item.cur
}

func (ds *docState) appendNodes(nn ...types.Node) {
	if ds.step == nil || len(nn) == 0 {
		return
	}
	if len(ds.env) != 0 {
		for _, n := range nn {
			n.MutateEnv(append(n.Env(), ds.env...))
		}
	}
	ds.step.Content.Append(nn...)
	ds.lastNode = nn[len(nn)-1]
}

// renderToHTML preprocesses markdown bytes and then calls the Blackfriday Markdown parser with some special addons selected.
// It takes a raw markdown bytes and output parsed xhtml in bytes.
func renderToHTML(b []byte) []byte {
	b = convertImports(b)

	htmlFlags := blackfriday.UseXHTML |
		blackfriday.Smartypants |
		blackfriday.SmartypantsFractions |
		blackfriday.SmartypantsDashes |
		blackfriday.SmartypantsLatexDashes

	params := blackfriday.HTMLRendererParameters{Flags: htmlFlags}
	r := blackfriday.NewHTMLRenderer(params)

	extns := blackfriday.FencedCode |
		blackfriday.NoEmptyLineBeforeBlock |
		blackfriday.NoIntraEmphasis |
		blackfriday.DefinitionLists |
		blackfriday.Tables

	return blackfriday.Run(b, blackfriday.WithExtensions(extns), blackfriday.WithRenderer(r))
}

// parseMarkup accepts html nodes to markup created by the Devsite Markdown parser. It returns a pointer to a codelab object, or an error if one occurs.
func parseMarkup(markup *html.Node, opts parser.Options) (*types.Codelab, error) {
	body := findAtom(markup, atom.Body)
	if body == nil {
		return nil, fmt.Errorf("document without a body")
	}

	ds := newDocState()

	for ds.cur = body.FirstChild; ds.cur != nil; ds.cur = ds.cur.NextSibling {
		switch {
		// metadata first
		case ds.cur.DataAtom == atom.H1 && ds.clab.Title == "":
			if v := stringifyNode(ds.cur, true); v != "" {
				ds.clab.Title = v
			}
			continue
		case ds.cur.DataAtom == atom.P && ds.clab.ID == "":
			if err := parseMetadata(ds, opts); err != nil {
				return nil, err
			}
			continue
		case ds.cur.DataAtom == atom.H2:
			newStep(ds)
			continue
		}
		// ignore everything else before the first step
		if ds.step != nil {
			parseTop(ds)
		}
	}

	finalizeStep(ds.step) // TODO: last ds.step is never finalized in newStep
	ds.clab.Tags = util.Unique(ds.clab.Tags)
	sort.Strings(ds.clab.Tags)
	ds.clab.Duration = int(ds.totdur.Minutes())
	return ds.clab, nil
}

func finalizeStep(s *types.Step) {
	if s == nil {
		return
	}
	s.Tags = util.Unique(s.Tags)
	sort.Strings(s.Tags)
	s.Content.Nodes = blockNodes(s.Content.Nodes)
	s.Content.Nodes = compactNodes(s.Content.Nodes)
}

// parseTop parses nodes tree starting at, and including, ds.cur.
// Parsed nodes are squashed and added to ds.step content.
func parseTop(ds *docState) {
	if n, ok := parseNode(ds); ok {
		if n != nil {
			ds.appendNodes(n)
		}
		return
	}
	ds.push(nil)
	nn := parseSubtree(ds)
	ds.pop()
	ds.appendNodes(compactNodes(nn)...)
}

// parseSubtree parses children of root recursively.
// It may modify ds.cur, so the caller is responsible for wrapping
// this function in ds.push and ds.pop.
func parseSubtree(ds *docState) []types.Node {
	var nodes []types.Node
	for ds.cur = ds.cur.FirstChild; ds.cur != nil; ds.cur = ds.cur.NextSibling {
		if n, ok := parseNode(ds); ok {
			if n != nil {
				nodes = append(nodes, n)
			}
			continue
		}
		ds.push(nil)
		nodes = append(nodes, parseSubtree(ds)...)
		ds.pop()
	}
	return nodes
}

// parseNode parses html node hn if it is a recognized node construction.
// It returns a bool indicating that hn has been accepted and parsed.
// Some nodes result in metadata parsing, in which case the returned bool is still true,
// but resuling types.Node is nil.
//
// The flag argument modifies default behavour of the func.
func parseNode(ds *docState) (types.Node, bool) {
	// we have \n end of line nodes after each tag from the blackfriday parser.
	// We just want to ignore them as it makes previous node detection fuzzy.
	if ds.cur.Type == html.TextNode && strings.TrimSpace(ds.cur.Data) == "" {
		return nil, true
	}
	switch {
	case isMeta(ds.cur):
		metaStep(ds)
		return nil, true
	case ds.cur.Type == html.TextNode || ds.cur.DataAtom == atom.Br:
		return text(ds), true
	case ds.cur.DataAtom == atom.A:
		return link(ds), true
	case ds.cur.DataAtom == atom.Img:
		return image(ds), true
	case isButton(ds.cur):
		return button(ds), true
	case isHeader(ds.cur):
		return header(ds), true
	case isList(ds.cur):
		return list(ds), true
	case isConsole(ds.cur):
		return code(ds, true), true
	case isCode(ds.cur):
		return code(ds, false), true
	case isInfobox(ds.cur):
		return infobox(ds), true
	case isSurvey(ds.cur):
		return survey(ds), true
	case isTable(ds.cur):
		return table(ds), true
	case isYoutube(ds.cur):
		return youtube(ds), true
	case isFragmentImport(ds.cur):
		return fragmentImport(ds), true
	}
	return nil, false
}

// newStep creates a new codelab step from ds.cur
// and finalizes nodes of the previous step.
func newStep(ds *docState) {
	t := stringifyNode(ds.cur, true)
	if t == "" {
		return
	}
	finalizeStep(ds.step)
	ds.step = ds.clab.NewStep(t)
	ds.env = nil
}

// parseMetadata parses the first <p> of a codelab doc to populate metadata.
func parseMetadata(ds *docState, opts parser.Options) error {
	m := map[string]string{}
	// Split the keys from values.
	d := ds.cur.FirstChild.Data
	scanner := bufio.NewScanner(strings.NewReader(d))
	for scanner.Scan() {
		s := metadataRegexp.FindStringSubmatch(scanner.Text())
		if len(s) != 3 {
			continue
		}

		k := strings.ToLower(strings.TrimSpace(s[1]))
		v := strings.TrimSpace(s[2])
		m[k] = v

	}
	if _, ok := m["id"]; !ok || m["id"] == "" {
		return fmt.Errorf("invalid metadata format, missing at least id: %v", m)
	}
	return addMetadataToCodelab(m, ds.clab, opts)
}

// standardSplit takes a string, splits it along a comma delimiter, then on each fragment, trims Unicode spaces
// from both ends and converts them to lowercase. It returns a slice of the processed strings.
func standardSplit(s string) []string {
	strs := strings.Split(s, ",")
	for k, v := range strs {
		strs[k] = strings.ToLower(strings.TrimSpace(v))
	}
	return strs
}

// addMetadataToCodelab takes a map of strings to strings, a pointer to a Codelab, and an options struct. It reads the keys of the map,
// and assigns the values to any keys that match a codelab metadata field as defined by the meta* constants.
func addMetadataToCodelab(m map[string]string, c *types.Codelab, opts parser.Options) error {
	for k, v := range m {
		switch k {
		case MetaAuthors:
			// Directly assign the summary to the codelab field.
			c.Authors = v
		case MetaBadgePath:
			// Directly assign the badge Path to the codelab field.
			c.BadgePath = v
		case MetaSummary:
			// Directly assign the summary to the codelab field.
			c.Summary = v
			break
		case MetaID:
			// Directly assign the ID to the codelab field.
			c.ID = v
			break
		case MetaCategories:
			// Standardize the categories and append to codelab field.
			c.Categories = append(c.Categories, standardSplit(v)...)
			break
		case MetaEnvironments:
			// Standardize the tags and append to the codelab field.
			c.Tags = append(c.Tags, standardSplit(v)...)
			break
		case MetaStatus:
			// Standardize the statuses and append to the codelab field.
			statuses := standardSplit(v)
			statusesAsLegacy := types.LegacyStatus(statuses)
			c.Status = &statusesAsLegacy
			break
		case MetaFeedbackLink:
			// Directly assign the feedback link to the codelab field.
			c.Feedback = v
			break
		case MetaAnalyticsAccount:
			// Directly assign the GA id to the codelab field.
			c.GA = v
			break
		case MetaTags:
			// Standardize the tags and append to the codelab field.
			c.Tags = append(c.Tags, standardSplit(v)...)
			break
		default:
			// If not explicitly parsed, it might be a pass_metadata value.
			if _, ok := opts.PassMetadata[k]; ok {
				c.Extra[k] = v
			}
			break
		}
	}
	return nil
}

// metaStep parses a codelab step meta instructions.
func metaStep(ds *docState) {
	var text string
	for {
		text += stringifyNode(ds.cur, false)
		if ds.cur.NextSibling == nil || !isMeta(ds.cur.NextSibling) {
			break
		}
		ds.cur = ds.cur.NextSibling
	}
	meta := strings.SplitN(strings.TrimSpace(text), metaSep, 2)
	if len(meta) != 2 {
		return
	}
	value := strings.TrimSpace(meta[1])
	switch strings.ToLower(strings.TrimSpace(meta[0])) {
	case metaDuration:
		parts := strings.SplitN(value, ":", len(durFactor))
		if len(parts) == 1 {
			parts = append(parts, "0") // default number is minutes
		}
		var d time.Duration
		for i, v := range parts {
			vi, err := strconv.Atoi(v)
			if err != nil {
				continue
			}
			d += time.Duration(vi) * durFactor[len(durFactor)-len(parts)+i]
		}
		ds.step.Duration = roundDuration(d)
		ds.totdur += ds.step.Duration
	case metaEnvironment:
		ds.env = util.Unique(stringSlice(value))
		toLowerSlice(ds.env)
		ds.step.Tags = append(ds.step.Tags, ds.env...)
		ds.clab.Tags = append(ds.clab.Tags, ds.env...)
		if ds.lastNode != nil && types.IsHeader(ds.lastNode.Type()) {
			ds.lastNode.MutateEnv(ds.env)
		}
	}
}

// header creates a HeaderNode out of hn.
// It returns nil if header content is empty.
// A non-empty header will always reset ds.env to nil.
//
// Given that headers do not belong to any block, the returned node's B
// field is always nil.
func header(ds *docState) types.Node {
	ds.push(nil)
	nodes := parseSubtree(ds)
	ds.pop()
	if len(nodes) == 0 {
		return nil
	}
	n := types.NewHeaderNode(headerLevel[ds.cur.DataAtom], nodes...)
	switch strings.ToLower(stringifyNode(ds.cur, true)) {
	case headerLearn, headerCover:
		n.MutateType(types.NodeHeaderCheck)
	case headerFAQ:
		n.MutateType(types.NodeHeaderFAQ)
	}
	ds.env = nil
	return n
}

// infobox doesn't have a block parent.
func infobox(ds *docState) types.Node {
	negativeInfoBox := isInfoboxNegative(ds.cur)
	// iterate twice on next sibling as there is a \n node in between
	ds.cur = ds.cur.NextSibling.NextSibling
	ds.push(nil)
	nn := parseSubtree(ds)
	nn = blockNodes(nn)
	nn = compactNodes(nn)
	ds.pop()
	if len(nn) == 0 {
		return nil
	}
	kind := types.InfoboxPositive
	if negativeInfoBox {
		kind = types.InfoboxNegative
	}
	return types.NewInfoboxNode(kind, nn...)
}

// table parses an arbitrary <table> element and its children.
// It may return other elements if the table is just a wrap.
func table(ds *docState) types.Node {
	var rows [][]*types.GridCell
	for _, tr := range findChildAtoms(ds.cur, atom.Tr) {
		ds.push(tr)
		r := tableRow(ds)
		ds.pop()
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return nil
	}
	return types.NewGridNode(rows...)
}

func tableRow(ds *docState) []*types.GridCell {
	var row []*types.GridCell
	for td := findAtom(ds.cur, atom.Td); td != nil; td = td.NextSibling {
		if td.DataAtom != atom.Td {
			continue
		}
		ds.push(td)
		nn := parseSubtree(ds)
		nn = blockNodes(nn)
		nn = compactNodes(nn)
		ds.pop()
		if len(nn) == 0 {
			continue
		}
		cs, err := strconv.Atoi(nodeAttr(td, "colspan"))
		if err != nil {
			cs = 1
		}
		rs, err := strconv.Atoi(nodeAttr(td, "rowspan"))
		if err != nil {
			rs = 1
		}
		cell := &types.GridCell{
			Colspan: cs,
			Rowspan: rs,
			Content: types.NewListNode(nn...),
		}
		row = append(row, cell)
	}
	return row
}

// survey expects a name Node followed by 1 or more inputs Nodes. Each input node is expected to have a value attribute.
func survey(ds *docState) types.Node {
	var gg []*types.SurveyGroup
	hn := ds.cur
	n := findAtom(hn, atom.Name)
	inputs := findChildAtoms(hn, atom.Input)
	opt := surveyOpt(inputs)

	if len(opt) > 0 {
		gg = append(gg, &types.SurveyGroup{
			Name:    strings.TrimSpace(n.FirstChild.Data),
			Options: opt,
		})
	}

	if len(gg) == 0 {
		return nil
	}
	ds.survey++
	id := fmt.Sprintf("%s-%d", ds.clab.ID, ds.survey)
	return types.NewSurveyNode(id, gg...)
}

func surveyOpt(inputs []*html.Node) []string {
	var opt []string
	for _, input := range inputs {
		for _, attr := range input.Attr {
			if attr.Key == "value" {
				opt = append(opt, attr.Val)
			}
		}
	}
	return opt
}

// code parses hn as inline or block codes.
// Inline code node will be of type NodeText.
func code(ds *docState, term bool) types.Node {
	elem := findParent(ds.cur, atom.Pre)
	// inline <code> text
	if elem == nil {
		return text(ds)
	}
	// block code or terminal
	v := stringifyNode(ds.cur, false)
	if v == "" {
		if countDirect(ds.cur.Parent) > 1 {
			return nil
		}
		v = "\n"
	} else if ds.cur.Parent.FirstChild == ds.cur && ds.cur.Parent.DataAtom != atom.Span {
		v = "\n" + v
	}
	n := types.NewCodeNode(v, term)
	n.MutateBlock(elem)
	return n
}

// list parses <ul> and <ol> lists.
// It returns nil if the list has no items.
func list(ds *docState) types.Node {
	typ := nodeAttr(ds.cur, "type")
	if ds.cur.DataAtom == atom.Ol && typ == "" {
		typ = "1"
	}
	start, _ := strconv.Atoi(nodeAttr(ds.cur, "start"))
	list := types.NewItemsListNode(typ, start)
	for hn := findAtom(ds.cur, atom.Li); hn != nil; hn = hn.NextSibling {
		if hn.DataAtom != atom.Li {
			continue
		}
		ds.push(hn)
		nn := parseSubtree(ds)
		nn = compactNodes(nn)
		ds.pop()
		if len(nn) > 0 {
			list.NewItem(nn...)
		}
	}
	if len(list.Items) == 0 {
		return nil
	}
	if ds.lastNode != nil {
		switch ds.lastNode.Type() {
		case types.NodeHeaderCheck:
			list.MutateType(types.NodeItemsCheck)
		case types.NodeHeaderFAQ:
			list.MutateType(types.NodeItemsFAQ)
		}
	}
	return list
}

// image creates a new ImageNode out of hn, parsing its src attribute.
// It returns nil if src is empty.
// It may also return a YouTubeNode if alt property contains specific substring.
func image(ds *docState) types.Node {
	alt := nodeAttr(ds.cur, "alt")
	if strings.Contains(alt, "youtube.com/watch") {
		return youtube(ds)
	} else if strings.Contains(alt, "https://") {
		u, err := url.Parse(nodeAttr(ds.cur, "alt"))
		if err != nil {
			return nil
		}
		// For iframe, make sure URL ends in whitelisted domain.
		ok := false
		for _, domain := range types.IframeWhitelist {
			if strings.HasSuffix(u.Hostname(), domain) {
				ok = true
				break
			}
		}
		if ok {
			return iframe(ds)
		}
	}
	s := nodeAttr(ds.cur, "src")
	if s == "" {
		return nil
	}

	n := types.NewImageNode(s)

	if alt := nodeAttr(ds.cur, "alt"); alt != "" {
		n.Alt = alt
	}

	if title := nodeAttr(ds.cur, "title"); title != "" {
		n.Title = title
	}

	if ws := nodeAttr(ds.cur, "width"); ws != "" {
		w, err := strconv.ParseFloat(ws, 64)
		if err != nil {
			return nil
		}
		n.Width = float32(w)
	}

	n.MutateBlock(findBlockParent(ds.cur))
	return n
}

func youtube(ds *docState) types.Node {
	for _, attr := range ds.cur.Attr {
		if attr.Key == "id" {
			n := types.NewYouTubeNode(attr.Val)
			n.MutateBlock(true)
			return n
		}
	}
	return nil
}

func fragmentImport(ds *docState) types.Node {
	if url := strings.TrimPrefix(ds.cur.Data, convertedImportsDataPrefix); url != "" {
		return types.NewImportNode(url)
	}

	return nil
}

func iframe(ds *docState) types.Node {
	u, err := url.Parse(nodeAttr(ds.cur, "alt"))
	if err != nil {
		return nil
	}
	// Allow only https.
	if u.Scheme != "https" {
		return nil
	}
	n := types.NewIframeNode(u.String())
	n.MutateBlock(true)
	return n
}

// button returns either a text node, if no <a> child element is present,
// or link node, containing the button.
// It returns nil if no content nodes are present.
func button(ds *docState) types.Node {
	a := findAtom(ds.cur, atom.A)
	if a == nil {
		return text(ds)
	}
	href := nodeAttr(a, "href")
	if href == "" {
		return nil
	}

	ds.push(a)
	nodes := parseSubtree(ds)
	ds.pop()
	if len(nodes) == 0 {
		return nil
	}

	s := strings.ToLower(stringifyNode(a, true))
	dl := strings.HasPrefix(s, "download ")
	btn := types.NewButtonNode(true, true, dl, nodes...)

	ln := types.NewURLNode(href, btn)
	ln.MutateBlock(findBlockParent(ds.cur))
	return ln
}

// Link creates a URLNode out of hn, parsing href and name attributes.
// It returns nil if hn contents is empty.
// The resuling link's content is always a single text node.
func link(ds *docState) types.Node {
	href := nodeAttr(ds.cur, "href")

	ds.push(nil)
	parsedChildNodes := parseSubtree(ds)
	ds.pop()

	// Check outside styles
	outsideBold := isBold(ds.cur.Parent)
	outsideItalic := isItalic(ds.cur.Parent)
	// Apply outside styles to inside parsed (text) nodes
	for _, node := range parsedChildNodes {
		if textNode, ok := node.(*types.TextNode); ok {
			textNode.Bold = textNode.Bold || outsideBold
			textNode.Italic = textNode.Italic || outsideItalic
		}
	}

	n := types.NewURLNode(href, parsedChildNodes...)
	n.Name = nodeAttr(ds.cur, "name")
	if v := nodeAttr(ds.cur, "target"); v != "" {
		n.Target = v
	}
	n.MutateBlock(findBlockParent(ds.cur))
	return n
}

// text creates a TextNode using hn.Data as contents.
// It returns nil if hn.Data is empty or contains only space runes.
func text(ds *docState) types.Node {
	bold := isBold(ds.cur)
	italic := isItalic(ds.cur)
	code := isCode(ds.cur) || isConsole(ds.cur)

	// TODO: verify whether this actually does anything
	if a := findAtom(ds.cur, atom.A); a != nil {
		ds.push(a)
		l := link(ds)
		ds.pop()
		if l != nil {
			l.MutateBlock(findBlockParent(ds.cur))
			return l
		}
	}

	v := stringifyNode(ds.cur, false)
	n := types.NewTextNode(v)
	n.Bold = bold
	n.Italic = italic
	n.Code = code
	n.MutateBlock(findBlockParent(ds.cur))
	return n
}

// slug converts any string s to a slug.
// It replaces [^a-z0-9\-] with non-repeating '-'.
func slug(s string) string {
	var buf bytes.Buffer
	dash := true
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && !dash {
			buf.WriteRune(r)
			dash = r == '-'
			continue
		}
		if !dash {
			buf.WriteRune('-')
			dash = true
		}
	}
	return buf.String()
}

// stringSlice splits v by comma "," while ignoring empty elements.
func stringSlice(v string) []string {
	f := strings.Split(v, ",")
	a := f[0:0]
	for _, s := range f {
		s = strings.TrimSpace(s)
		if s != "" {
			a = append(a, s)
		}
	}
	return a
}

func toLowerSlice(a []string) {
	for i, s := range a {
		a[i] = strings.ToLower(s)
	}
}

// roundDuration rounds time to the nearest minute, always rounding
// up when there is any fractional portion of a minute.
// Ex:
//  59s --> 1m
//  60s --> 1m
//  61s --> 2m
func roundDuration(d time.Duration) time.Duration {
	rd := time.Duration(d.Minutes()) * time.Minute
	if rd < d {
		rd += time.Minute
	}
	return rd
}

func convertImports(content []byte) []byte {
	slices := bytes.Split(content, []byte("\n"))
	escaped := [][]byte{}
	for _, slice := range slices {
		if matches := importsTagRegexp.FindSubmatch(slice); len(matches) > 0 {
			if len(matches) > 1 {
				url := string(matches[1])
				slice = bytes.Join([][]byte{
					convertedImportsPrefix,
					[]byte(html.EscapeString(url)),
					convertedImportsSuffix,
				}, []byte(""))
			}
		}

		escaped = append(escaped, slice)
	}

	return bytes.Join(escaped, []byte("\n"))
}

func hasImport(ds *docState) bool {
	for _, step := range ds.clab.Steps {
		if len(types.ImportNodes(step.Content.Nodes)) > 0 {
			return true
		}
	}

	return false
}
