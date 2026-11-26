package query

import "strings"

const defaultChunkChars = 500

// ChunkMarkdown splits a parsed document into retrieval chunks. Paragraphs
// (separated by blank lines) form the natural unit; a paragraph longer than
// maxChars is further split on rune boundaries with sentence-aware breaking so
// Chinese text stays readable without an external tokenizer.
func ChunkMarkdown(markdown string, maxChars int) []Chunk {
	if maxChars <= 0 {
		maxChars = defaultChunkChars
	}
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")

	var chunks []Chunk
	for _, para := range splitParagraphs(markdown) {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		for _, piece := range splitByRunes(para, maxChars) {
			piece = strings.TrimSpace(piece)
			if piece == "" {
				continue
			}
			chunks = append(chunks, Chunk{Content: piece})
		}
	}
	return chunks
}

// splitParagraphs splits on one or more blank lines.
func splitParagraphs(s string) []string {
	var out []string
	var cur strings.Builder
	lines := strings.Split(s, "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// splitByRunes breaks text into pieces of at most maxChars runes, preferring to
// end a piece at a sentence terminator (。！？.!?;；) or whitespace within a
// small look-back window.
func splitByRunes(text string, maxChars int) []string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return []string{text}
	}
	var out []string
	start := 0
	for start < len(runes) {
		end := start + maxChars
		if end >= len(runes) {
			out = append(out, string(runes[start:]))
			break
		}
		// look back for a break point within 15% of maxChars
		lookBack := maxChars / 6
		if lookBack < 1 {
			lookBack = 1
		}
		breakAt := end
		for i := end; i > end-lookBack && i > start; i-- {
			r := runes[i-1]
			if isSentenceEnd(r) || r == '\n' || r == ' ' || r == '\t' {
				breakAt = i
				break
			}
		}
		out = append(out, string(runes[start:breakAt]))
		start = breakAt
	}
	return out
}

func isSentenceEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '.', '!', '?', ';', '；':
		return true
	}
	return false
}
