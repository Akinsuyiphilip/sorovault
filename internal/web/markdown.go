package web

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Contract authors write their doc comments in Markdown — the zkvote
// fixture's vote function has a table and a bullet list — so the browse UI
// renders them as Markdown rather than as the preformatted text they would
// otherwise be.
//
// Documentation comes from an untrusted chain, which decides how it is
// rendered:
//
//   - goldmark leaves raw HTML out of its output entirely, so a doc cannot
//     smuggle a tag into the page, and it drops the href of a link whose
//     scheme is dangerous;
//   - bluemonday then filters what is left through an allow-list, so only
//     the elements and attributes Markdown needs survive.
//
// The two are belt and braces: the first keeps the parser from interpreting
// HTML at all, the second keeps anything the parser did interpret from
// reaching the browser.
var markdown = goldmark.New(
	// GFM's tables and strikethrough, because contract documentation uses
	// them. The table cells are aligned with an attribute rather than a
	// style, so the alignment survives the sanitiser.
	goldmark.WithExtensions(extension.NewTable(
		extension.WithTableCellAlignMethod(extension.TableCellAlignAttribute),
	), extension.Strikethrough),
	goldmark.WithParserOptions(parser.WithASTTransformers(
		util.Prioritized(&linkRel{}, 100),
	)),
)

// markdownPolicy is the allow-list the rendered documentation must satisfy.
//
// It is the user-generated-content policy, which is built for exactly this:
// HTML written by somebody else. The one addition is the rel attribute,
// which is allowed only with the value the renderer stamps on every link.
var markdownPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("rel").Matching(regexp.MustCompile(`^nofollow noopener$`)).OnElements("a")
	return p
}()

// linkRel stamps rel="nofollow noopener" on the links goldmark renders.
//
// The documentation comes from an untrusted chain, so a link must not pass
// the reader's referrer on, nor hand the page it opens a handle on this
// one. Goldmark leaves the attribute off and the sanitiser allows only
// this value, so the two agree by construction.
type linkRel struct{}

// Transform implements parser.ASTTransformer.
func (l *linkRel) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if link, ok := node.(*ast.Link); ok {
			link.SetAttributeString("rel", "nofollow noopener")
		}
		return ast.WalkContinue, nil
	})
}

// renderMarkdown renders a doc comment as HTML that is safe to put in a
// page.
//
// The result is trusted markup: it has been through the allow-list, so a
// template can insert it as it is.
func renderMarkdown(doc string) template.HTML {
	if strings.TrimSpace(doc) == "" {
		return ""
	}

	var buf bytes.Buffer
	if err := markdown.Convert([]byte(doc), &buf); err != nil {
		// A document that cannot be parsed is shown as it came, escaped.
		return template.HTML(template.HTMLEscapeString(doc))
	}
	return template.HTML(markdownPolicy.Sanitize(buf.String()))
}
