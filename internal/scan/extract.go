package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// relPath 兼容包装(filepath.Rel + 正斜杠)。
func relPath(base, full string) (string, error) {
	r, err := filepath.Rel(base, full)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(r, "..") {
		return "", fmt.Errorf("path %s outside base %s", full, base)
	}
	return filepath.ToSlash(r), nil
}

// Result 是单个文件的处理结果分类原因。
type SkipReason string

const (
	// SkipIgnored 被忽略规则跳过。
	SkipIgnored SkipReason = "ignored"
	// SkipTooLarge 超过大小上限。
	SkipTooLarge SkipReason = "too_large"
	// SkipBinary 疑似二进制。
	SkipBinary SkipReason = "binary"
	// SkipNonUTF8 非 UTF-8 编码(如 GBK,不支持解码)。
	SkipNonUTF8 SkipReason = "non_utf8"
	// SkipSymlink 符号链接(不跟随)。
	SkipSymlink SkipReason = "symlink"
	// SkipRead 读取失败。
	SkipRead SkipReason = "read_error"
	// SkipOutside 不在索引根目录下。
	SkipOutside SkipReason = "outside_root"
)

// Extracted 是提取出的文档内容。
type Extracted struct {
	Title string
	Body  string
}

// Extractor 按扩展名提取标题与正文。
func Extractor(ext string) func(rel string, data []byte) (Extracted, error) {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "md", "markdown":
		return extractMarkdown
	case "html", "htm":
		return extractHTML
	case "docx":
		return extractDocx
	default:
		// txt/json/csv/代码等按纯文本。
		return extractPlain
	}
}

// extractMarkdown 剥离 front matter,首个一级标题作为 title。
func extractMarkdown(rel string, data []byte) (Extracted, error) {
	text := string(data)
	ex := Extracted{}
	if strings.HasPrefix(text, "---\n") || strings.HasPrefix(text, "---\r\n") {
		// 剥离 YAML front matter。
		rest := strings.TrimPrefix(text, "---\n")
		rest = strings.TrimPrefix(rest, "---\r\n")
		if idx := strings.Index(rest, "\n---"); idx >= 0 {
			after := rest[idx+len("\n---"):]
			after = strings.TrimPrefix(after, "\r\n")
			after = strings.TrimPrefix(after, "\n")
			text = after
		}
	}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			ex.Title = strings.TrimSpace(t[2:])
			break
		}
		if t != "" {
			break // 首个非空行不是标题则停止
		}
	}
	ex.Body = text
	return ex, nil
}

// extractHTML 自写标签剥离器(跳过 script/style,实体用标准库解码)。
func extractHTML(rel string, data []byte) (Extracted, error) {
	text := string(data)
	ex := Extracted{Title: fileNameOf(rel)}
	var body strings.Builder
	i := 0
	n := len(text)
	for i < n {
		if text[i] == '<' {
			end := strings.IndexByte(text[i:], '>')
			if end < 0 {
				break
			}
			tag := strings.ToLower(strings.TrimSpace(text[i+1 : i+end]))
			tagName := tag
			if k := strings.IndexAny(tagName, " \t\r\n/"); k >= 0 {
				tagName = tagName[:k]
			}
			if tagName == "script" || tagName == "style" {
				// 跳过整块内容。
				closeTag := "</" + tagName
				if pos := strings.Index(strings.ToLower(text[i+end:]), closeTag); pos >= 0 {
					j := i + end + pos
					if gt := strings.IndexByte(text[j:], '>'); gt >= 0 {
						i = j + gt + 1
						continue
					}
				}
				i = n
				continue
			}
			if tagName == "title" {
				close := "</title>"
				start := i + end + 1 // 跳过 '>'
				if pos := strings.Index(strings.ToLower(text[start:]), close); pos >= 0 {
					ex.Title = strings.TrimSpace(unescapeHTML(text[start : start+pos]))
					i = start + pos + len(close)
					continue
				}
			}
			// 换行标签提供分隔。
			if tagName == "br" || tagName == "p" || tagName == "div" ||
				tagName == "li" || tagName == "h1" || tagName == "h2" || tagName == "tr" {
				body.WriteByte('\n')
			}
			i += end + 1
			continue
		}
		body.WriteByte(text[i])
		i++
	}
	ex.Body = unescapeHTML(body.String())
	return ex, nil
}

// unescapeHTML 解码 HTML 实体(标准库)。
func unescapeHTML(s string) string {
	return htmlUnescape(s)
}

// extractPlain 纯文本。
func extractPlain(rel string, data []byte) (Extracted, error) {
	return Extracted{Title: "", Body: string(data)}, nil
}

// extractDocx 提取 word/document.xml 中的文本(archive/zip + encoding/xml)。
func extractDocx(rel string, data []byte) (Extracted, error) {
	return extractDocxImpl(rel, data)
}

func fileNameOf(rel string) string {
	name := rel
	if k := strings.LastIndexAny(name, "/\\"); k >= 0 {
		name = name[k+1:]
	}
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		name = name[:dot]
	}
	return name
}

var _ = os.Getenv
