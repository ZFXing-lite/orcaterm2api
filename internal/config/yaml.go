// Package config 提供 YAML 配置加载/回写与环境变量覆盖。
//
// 为保持零第三方依赖，内含一个最小 YAML 子集解析器，
// 仅支持本项目配置用到的写法：缩进映射、"- " 序列、纯量/单双引号
// 字符串、行尾注释。写回时按固定模板生成，避免往返失真。
package config

import (
	"fmt"
	"strings"
)

// parseYAML 将 YAML 子集解析为 map[string]any / []any / string。
func parseYAML(src string) (map[string]any, error) {
	p := &yamlParser{lines: strings.Split(src, "\n")}
	v, next, err := p.parseBlock(0, 0)
	if err != nil {
		return nil, err
	}
	_ = next
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("yaml: 顶层必须是映射")
	}
	return m, nil
}

type yamlParser struct {
	lines []string
}

type yamlLine struct {
	indent int
	text   string // 去掉缩进与行尾注释后的内容
	raw    string
}

// blockLine 返回第 i 行解析后的逻辑行；空行/纯注释行返回 ok=false。
func (p *yamlParser) blockLine(i int) (l yamlLine, ok bool) {
	if i < 0 || i >= len(p.lines) {
		return yamlLine{}, false
	}
	raw := p.lines[i]
	trimmed := strings.TrimRight(raw, " \t\r")
	if strings.TrimSpace(trimmed) == "" {
		return yamlLine{}, false
	}
	indent := 0
	for indent < len(trimmed) && trimmed[indent] == ' ' {
		indent++
	}
	rest := trimmed[indent:]
	if strings.HasPrefix(rest, "#") {
		return yamlLine{}, false
	}
	rest = stripComment(rest)
	rest = strings.TrimRight(rest, " \t")
	if rest == "" {
		return yamlLine{}, false
	}
	return yamlLine{indent: indent, text: rest, raw: raw}, true
}

// stripComment 去掉不在引号内的行尾注释。
func stripComment(s string) string {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if c == q {
				q = 0
			} else if c == '\\' {
				i++
			}
			continue
		}
		if c == '"' || c == '\'' {
			q = c
			continue
		}
		if c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return strings.TrimRight(s[:i], " \t")
		}
	}
	return s
}

// parseBlock 从第 i 行、缩进 base 起解析一个块，返回 值/下一行号。
func (p *yamlParser) parseBlock(i, base int) (any, int, error) {
	l, ok := p.blockLine(i)
	if !ok {
		// 跳过空行
		j := i + 1
		for {
			if j >= len(p.lines) {
				return nil, j, nil
			}
			if _, ok2 := p.blockLine(j); ok2 {
				break
			}
			j++
		}
		return p.parseBlock(j, base)
	}
	if l.indent < base {
		return nil, i, nil
	}
	if l.indent > base {
		return nil, 0, fmt.Errorf("yaml: 第 %d 行缩进异常: %q", i+1, l.raw)
	}
	if strings.HasPrefix(l.text, "- ") || l.text == "-" {
		return p.parseSeq(i, base)
	}
	return p.parseMap(i, base)
}

func (p *yamlParser) parseMap(i, base int) (map[string]any, int, error) {
	m := map[string]any{}
	j := i
	for j < len(p.lines) {
		l, ok := p.blockLine(j)
		if !ok {
			j++
			continue
		}
		if l.indent < base {
			break
		}
		if l.indent > base {
			return nil, 0, fmt.Errorf("yaml: 第 %d 行缩进异常: %q", j+1, l.raw)
		}
		if strings.HasPrefix(l.text, "- ") || l.text == "-" {
			break // 序列项属于上层
		}
		key, val, hasVal := splitKey(l.text)
		if key == "" {
			return nil, 0, fmt.Errorf("yaml: 第 %d 行缺少键: %q", j+1, l.raw)
		}
		j++
		if hasVal {
			m[key] = unquote(val)
			continue
		}
		// 值在下一缩进块
		nl, ok2 := p.blockLine(j)
		for !ok2 && j < len(p.lines) {
			j++
			nl, ok2 = p.blockLine(j)
		}
		if !ok2 || nl.indent <= base {
			m[key] = nil
			continue
		}
		v, next, err := p.parseBlock(j, nl.indent)
		if err != nil {
			return nil, 0, err
		}
		m[key] = v
		j = next
	}
	return m, j, nil
}

func (p *yamlParser) parseSeq(i, base int) ([]any, int, error) {
	var out []any
	j := i
	for j < len(p.lines) {
		l, ok := p.blockLine(j)
		if !ok {
			j++
			continue
		}
		if l.indent < base {
			break
		}
		if l.indent > base {
			return nil, 0, fmt.Errorf("yaml: 第 %d 行缩进异常: %q", j+1, l.raw)
		}
		t := l.text
		if !(strings.HasPrefix(t, "- ") || t == "-") {
			break
		}
		j++
		rest := ""
		if len(t) > 1 {
			rest = strings.TrimSpace(t[1:])
		}
		if rest == "" {
			// "-" 独占一行，值在下一缩进块
			nl, ok2 := p.blockLine(j)
			for !ok2 && j < len(p.lines) {
				j++
				nl, ok2 = p.blockLine(j)
			}
			if !ok2 || nl.indent <= base {
				out = append(out, nil)
				continue
			}
			v, next, err := p.parseBlock(j, nl.indent)
			if err != nil {
				return nil, 0, err
			}
			out = append(out, v)
			j = next
			continue
		}
		// "- key: value" 行内映射
		if key, val, hasVal := splitKey(rest); key != "" && strings.HasPrefix(strings.TrimSpace(rest), key+":") {
			m := map[string]any{}
			if hasVal {
				m[key] = unquote(val)
			} else {
				// "- key:" 后跟缩进块
				nl, ok2 := p.blockLine(j)
				for !ok2 && j < len(p.lines) {
					j++
					nl, ok2 = p.blockLine(j)
				}
				if ok2 && nl.indent > base {
					v, next, err := p.parseBlock(j, nl.indent)
					if err != nil {
						return nil, 0, err
					}
					m[key] = v
					j = next
				} else {
					m[key] = nil
				}
			}
			// 同缩进的后续 "key: value" 行并入同一映射
			for {
				nl, ok2 := p.blockLine(j)
				if !ok2 {
					j++
					if j >= len(p.lines) {
						break
					}
					continue
				}
				if nl.indent != base+2 && nl.indent <= base {
					break
				}
				if nl.indent <= base || strings.HasPrefix(nl.text, "- ") || nl.text == "-" {
					break
				}
				k2, v2, hv2 := splitKey(nl.text)
				if k2 == "" {
					break
				}
				j++
				if hv2 {
					m[k2] = unquote(v2)
				} else {
					m[k2] = nil
				}
			}
			out = append(out, m)
			continue
		}
		out = append(out, unquote(rest))
	}
	return out, j, nil
}

// splitKey 把 "key: value" 切成键与值；无冒号返回 hasVal=false。
func splitKey(s string) (key, val string, hasVal bool) {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if c == q {
				q = 0
			} else if c == '\\' {
				i++
			}
			continue
		}
		if c == '"' || c == '\'' {
			q = c
			continue
		}
		if c == ':' && (i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t') {
			k := strings.TrimSpace(s[:i])
			// 键一般不带引号；带引号时去引号
			if len(k) >= 2 && ((k[0] == '"' && k[len(k)-1] == '"') || (k[0] == '\'' && k[len(k)-1] == '\'')) {
				if u, ok := unquote(k).(string); ok {
					k = u
				}
			}
			v := strings.TrimSpace(s[i+1:])
			return k, v, v != ""
		}
	}
	return "", "", false
}

// unquote 去掉首尾引号并处理转义；未加引号的 [a, b] 解析为流式序列。
func unquote(s string) any {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return parseFlowSeq(s)
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		var b strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) {
				i++
				switch inner[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case '"':
					b.WriteByte('"')
				case '\\':
					b.WriteByte('\\')
				default:
					b.WriteByte('\\')
					b.WriteByte(inner[i])
				}
				continue
			}
			b.WriteByte(inner[i])
		}
		return b.String()
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

// parseFlowSeq 解析流式序列 "[a, b, c]"（引号内的逗号不切分）。
func parseFlowSeq(s string) []any {
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []any{}
	}
	var out []any
	var cur strings.Builder
	var q byte
	flush := func() {
		out = append(out, unquote(cur.String()))
		cur.Reset()
	}
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if q != 0 {
			cur.WriteByte(c)
			if c == q {
				q = 0
			} else if c == '\\' && i+1 < len(inner) {
				i++
				cur.WriteByte(inner[i])
			}
			continue
		}
		if c == '"' || c == '\'' {
			q = c
			cur.WriteByte(c)
			continue
		}
		if c == ',' {
			flush()
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return out
}
