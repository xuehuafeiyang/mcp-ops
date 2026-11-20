package websrv

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/go-shiori/go-readability"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// decodeHTML 按 Content-Type 或 <meta charset> 声明的字符集把响应体转为 UTF-8。
// 国内站点 GBK/GB18030 很常见，直接按 UTF-8 读会得到乱码。
func decodeHTML(r io.Reader, contentType string) (string, error) {
	utf8Reader, err := charset.NewReader(r, contentType)
	if err != nil {
		return "", fmt.Errorf("decode charset: %w", err)
	}
	b, err := io.ReadAll(utf8Reader)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(b), nil
}

// extractArticle 用 Mozilla Readability 算法提取正文并转 Markdown。
// 这是官方 mcp-server-fetch（readabilipy）与第三方实现（github.com/zcaceres/fetch-mcp）
// 的共同做法：先剥离导航/广告/侧栏，只留正文，再转 Markdown。
//
// 提取失败（列表页、API 文档、纯 div 布局等）时回退为整页转换，
// 并保留原页面结构，避免返回空内容。
func extractArticle(rawHTML string, pageURL *url.URL) (title, markdown string, err error) {
	if art, rerr := readability.FromReader(strings.NewReader(rawHTML), pageURL); rerr == nil {
		if strings.TrimSpace(art.Content) != "" {
			if md, merr := htmltomarkdown.ConvertString(art.Content); merr == nil && strings.TrimSpace(md) != "" {
				return strings.TrimSpace(art.Title), strings.TrimSpace(md), nil
			}
		}
	}

	md, merr := htmltomarkdown.ConvertString(stripNoise(rawHTML))
	if merr != nil {
		return "", "", fmt.Errorf("convert html to markdown: %w", merr)
	}
	return "", strings.TrimSpace(md), nil
}

// stripNoise 移除对阅读无意义、但会产生大量噪音的元素。
func stripNoise(h string) string {
	doc, err := html.Parse(strings.NewReader(h))
	if err != nil {
		return h
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if c.Type == html.ElementNode {
				switch c.Data {
				case "script", "style", "noscript", "iframe", "svg", "canvas", "template", "form", "object", "embed":
					n.RemoveChild(c)
					c = next
					continue
				}
			}
			walk(c)
			c = next
		}
	}
	walk(doc)

	var b strings.Builder
	if err := html.Render(&b, doc); err != nil {
		return h
	}
	return b.String()
}
