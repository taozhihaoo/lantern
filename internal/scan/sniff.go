package scan

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"html"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

func htmlUnescape(s string) string { return html.UnescapeString(s) }

// extractDocxImpl 从 docx(zip)中提取 word/document.xml 的文本。
func extractDocxImpl(rel string, data []byte) (Extracted, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Extracted{}, fmt.Errorf("scan: open docx: %w", err)
	}
	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return Extracted{}, fmt.Errorf("scan: docx missing word/document.xml")
	}
	rc, err := doc.Open()
	if err != nil {
		return Extracted{}, fmt.Errorf("scan: open document.xml: %w", err)
	}
	defer rc.Close()
	var xml bytes.Buffer
	if _, err := xml.ReadFrom(rc); err != nil {
		return Extracted{}, fmt.Errorf("scan: read document.xml: %w", err)
	}
	var body strings.Builder
	s := xml.String()
	i := 0
	for i < len(s) {
		if s[i] == '<' {
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				break
			}
			tag := s[i+1 : i+end]
			// w:t 文本节点内容直接收集;段落 w:p 结束时换行。
			if strings.HasPrefix(tag, "w:p") && (strings.HasSuffix(tag, "/") ||
				strings.HasSuffix(strings.TrimSpace(tag), "/")) {
				body.WriteByte('\n')
			}
			if strings.HasPrefix(tag, "/w:p") {
				body.WriteByte('\n')
			}
			i += end + 1
			continue
		}
		// 文本:收集到下一个 '<'。
		j := strings.IndexByte(s[i:], '<')
		var text string
		if j < 0 {
			text = s[i:]
			i = len(s)
		} else {
			text = s[i : i+j]
			i += j
		}
		// 仅保留 w:t 内文本:粗略策略——XML 文本节点只出现在标签间,
		// document.xml 中非 w:t 文本极少,直接收集并解码实体。
		body.WriteString(html.UnescapeString(text))
	}
	return Extracted{Body: body.String()}, nil
}

// SniffResult 是编码嗅探结果。
type SniffResult struct {
	// Kind: "utf8" | "utf16le" | "utf16be" | "binary" | "nonutf8"
	Kind string
	Text string // utf16 情况下已转码为 UTF-8
}

// Sniff 对原始字节做二进制/编码判定(规格 9.2):
// 前 8KB 含 NUL,或非法 UTF-8 占比过高 → binary;
// UTF-16 BOM 转码;其他非 UTF-8 → nonutf8(GBK 等不支持)。
func Sniff(data []byte) SniffResult {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		// UTF-16 BOM 例外(含 NUL)。
		if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
			return SniffResult{Kind: "utf16le", Text: decodeUTF16(data[2:], false)}
		}
		if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
			return SniffResult{Kind: "utf16be", Text: decodeUTF16(data[2:], true)}
		}
		return SniffResult{Kind: "binary"}
	}
	if !utf8.Valid(data) {
		// 含控制字符(除
		// 含控制字符(除 tab/lf/cr)则视为二进制;其余非法 UTF-8(GBK 等)
		// 归为 nonutf8(不支持解码,计数上报,见 DECISIONS.md D17)。
		for _, b := range head {
			if b < 0x20 && b != 0x09 && b != 0x0A && b != 0x0D {
				return SniffResult{Kind: "binary"}
			}
		}
		return SniffResult{Kind: "nonutf8"}
	}
	return SniffResult{Kind: "utf8", Text: string(data)}
}

// decodeUTF16 转码 UTF-16 为 UTF-8(标准库 unicode/utf16)。
func decodeUTF16(data []byte, bigEndian bool) string {
	u := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		var v uint16
		if bigEndian {
			v = binary.BigEndian.Uint16(data[i:])
		} else {
			v = binary.LittleEndian.Uint16(data[i:])
		}
		u = append(u, v)
	}
	return string(utf16.Decode(u))
}
