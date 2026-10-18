package main

// JS 词法级改写引擎。
//
// 为什么不用"完整 AST"：本模块要改的四类写法（location.host 取值、location 赋值、
// top.location、document.domain 赋值）只需要判断 token 之间的**邻接关系**，
// 不需要理解语法树。而完整 AST 一旦遇到新语法/压缩器产物（可选链、私有字段、
// BigInt、装饰器草案）就整份解析失败，只能放弃改写 —— 反而更不稳定。
// 词法级不会"解析失败"：最坏情况是保守跳过某一处改写，不会产生语法错误。
//
// 正确性的关键在三件事：
//  1. 字符串/模板串/正则/注释必须先被识别为整体，绝不能在内部做替换；
//  2. "/" 是正则还是除号，按"前一个 token 能否结束表达式"判定（与主流解析器一致）；
//  3. 属性访问前缀 "." 与 "?." 都要排除（obj.location 的 location 是属性名）。

import "strings"

type jsKind uint8

const (
	jsIdent jsKind = iota
	jsNum
	jsStr
	jsTpl   // 整个模板串（含 ${} 内嵌表达式）作为一个 token
	jsRegex // 正则字面量
	jsPunct
	jsComment
)

type jsToken struct {
	kind       jsKind
	start, end int
}

// jsTokText 取 token 原文。
func jsTokText(src string, t jsToken) string { return src[t.start:t.end] }

// jsIdentStart 标识符首字符（含 UTF-8 多字节，中文变量名合法）。
func jsIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// jsIdentPart 标识符后续字符。
func jsIdentPart(c byte) bool {
	return jsIdentStart(c) || (c >= '0' && c <= '9')
}

// jsScanString 扫描字符串字面量（src[i] 为引号），返回结束位置（含闭引号）。
func jsScanString(src string, i int) int {
	q := src[i]
	i++
	for i < len(src) {
		switch src[i] {
		case '\\':
			i += 2
			continue
		case q:
			return i + 1
		case '\n':
			return i // 未闭合：不吞行
		}
		i++
	}
	return i
}

// jsScanTemplate 扫描模板串（src[i] 为反引号），含 ${} 内嵌表达式与嵌套模板。
func jsScanTemplate(src string, i int) int {
	i++
	for i < len(src) {
		c := src[i]
		if c == '\\' {
			i += 2
			continue
		}
		if c == '`' {
			return i + 1
		}
		if c == '$' && i+1 < len(src) && src[i+1] == '{' {
			i = jsScanExpr(src, i+2)
			continue
		}
		i++
	}
	return i
}

// jsScanExpr 从 ${ 之后开始扫描，返回匹配的 } 之后的位置。
func jsScanExpr(src string, i int) int {
	depth := 1
	for i < len(src) {
		switch src[i] {
		case '`':
			i = jsScanTemplate(src, i)
			continue
		case '\'', '"':
			i = jsScanString(src, i)
			continue
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
				continue
			}
			if i+1 < len(src) && src[i+1] == '*' {
				if e := strings.Index(src[i+2:], "*/"); e >= 0 {
					i = i + 2 + e + 2
					continue
				}
				return len(src)
			}
			if e, ok := jsScanRegex(src, i); ok {
				i = e
				continue
			}
			i++
		case '{':
			depth++
			i++
		case '}':
			depth--
			i++
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return i
}

// jsScanRegex 尝试把 src[i] 处的 "/" 当成正则字面量扫描；不像正则则返回 false。
func jsScanRegex(src string, i int) (int, bool) {
	j := i + 1
	inClass := false
	for j < len(src) {
		c := src[j]
		switch c {
		case '\\':
			j += 2
			continue
		case '\n':
			return i, false // 跨行：不是正则
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				j++
				for j < len(src) && jsIdentPart(src[j]) { // flags
					j++
				}
				return j, true
			}
		}
		j++
	}
	return i, false
}

// jsRegexAllowed 判断当前位置的 "/" 能否开始一个正则字面量。
// 依据上一个 token：标识符（非关键字）、数字、字符串、正则、")"、"]"、"}" → 除号。
func jsRegexAllowed(havePrev bool, kind jsKind, text string) bool {
	if !havePrev {
		return true
	}
	switch kind {
	case jsIdent:
		switch text {
		case "return", "typeof", "instanceof", "in", "of", "new", "delete", "void",
			"throw", "case", "do", "else", "yield", "await":
			return true
		}
		return false
	case jsNum, jsStr, jsTpl, jsRegex:
		return false
	case jsPunct:
		switch text {
		case ")", "]", "}":
			return false
		}
		return true
	}
	return true
}

// jsPuncts 多字符运算符（最长优先匹配），用于避免把 "!==" 拆成 "=" 等。
var jsPuncts = []string{
	">>>=",
	"===", "!==", ">>>", "**=", "<<=", ">>=", "...", "&&=", "||=", "??=",
	"=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.", "++", "--",
	"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<", ">>", "**",
}

// jsTokenize 把源码切成 token（注释也保留为 token，供改写时跳过）。
func jsTokenize(src string) []jsToken {
	var toks []jsToken
	var prevKind jsKind
	var prevText string
	havePrev := false
	n := len(src)
	i := 0
	for i < n {
		c := src[i]

		// 空白
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		// 注释
		if c == '/' && i+1 < n && src[i+1] == '/' {
			s := i
			for i < n && src[i] != '\n' {
				i++
			}
			toks = append(toks, jsToken{jsComment, s, i})
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '*' {
			s := i
			if e := strings.Index(src[i+2:], "*/"); e >= 0 {
				i = i + 2 + e + 2
			} else {
				i = n
			}
			toks = append(toks, jsToken{jsComment, s, i})
			continue
		}
		// 字符串 / 模板串
		if c == '\'' || c == '"' {
			s := i
			i = jsScanString(src, i)
			t := jsToken{jsStr, s, i}
			toks = append(toks, t)
			prevKind, prevText, havePrev = toks[len(toks)-1].kind, jsTokText(src, toks[len(toks)-1]), true
			continue
		}
		if c == '`' {
			s := i
			i = jsScanTemplate(src, i)
			toks = append(toks, jsToken{jsTpl, s, i})
			prevKind, prevText, havePrev = toks[len(toks)-1].kind, jsTokText(src, toks[len(toks)-1]), true
			continue
		}
		// 标识符 / 数字
		if jsIdentStart(c) {
			s := i
			for i < n && jsIdentPart(src[i]) {
				i++
			}
			toks = append(toks, jsToken{jsIdent, s, i})
			prevKind, prevText, havePrev = toks[len(toks)-1].kind, jsTokText(src, toks[len(toks)-1]), true
			continue
		}
		if (c >= '0' && c <= '9') || (c == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9') {
			s := i
			for i < n {
				d := src[i]
				if jsIdentPart(d) || d == '.' || d == '_' { // 0x1f / 1e-9 / 1_000n
					// 指数符号
					if (d == 'e' || d == 'E') && i+1 < n && (src[i+1] == '+' || src[i+1] == '-') {
						i += 2
						continue
					}
					i++
					continue
				}
				break
			}
			toks = append(toks, jsToken{jsNum, s, i})
			prevKind, prevText, havePrev = toks[len(toks)-1].kind, jsTokText(src, toks[len(toks)-1]), true
			continue
		}
		// 正则 or 除号
		if c == '/' {
			if jsRegexAllowed(havePrev, prevKind, prevText) {
				if e, ok := jsScanRegex(src, i); ok {
					s := i
					i = e
					toks = append(toks, jsToken{jsRegex, s, i})
					prevKind, prevText, havePrev = toks[len(toks)-1].kind, jsTokText(src, toks[len(toks)-1]), true
					continue
				}
			}
			toks = append(toks, jsToken{jsPunct, i, i + 1})
			prevKind, prevText, havePrev = toks[len(toks)-1].kind, jsTokText(src, toks[len(toks)-1]), true
			i++
			continue
		}
		// 多字符运算符
		matched := false
		for _, p := range jsPuncts {
			if strings.HasPrefix(src[i:], p) {
				// "?." 后紧跟数字时按 "?" + 数字处理（a?.5:b）
				if p == "?." && i+2 < n && src[i+2] >= '0' && src[i+2] <= '9' {
					continue
				}
				toks = append(toks, jsToken{jsPunct, i, i + len(p)})
				prevKind, prevText, havePrev = toks[len(toks)-1].kind, jsTokText(src, toks[len(toks)-1]), true
				i += len(p)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		// 单字符标点
		toks = append(toks, jsToken{jsPunct, i, i + 1})
		prevKind, prevText, havePrev = jsPunct, src[i:i+1], true
		i++
	}
	return toks
}

// ---- 改写 ----

type jsEdit struct {
	start, end int
	text       string
}

// jsIsPunct 判断 token k 是否为指定标点。
func jsIsPunct(src string, toks []jsToken, k int, s string) bool {
	return k >= 0 && k < len(toks) && toks[k].kind == jsPunct && jsTokText(src, toks[k]) == s
}

// jsIsIdent 判断 token k 是否为指定标识符（大小写敏感，JS 关键字均小写）。
func jsIsIdent(src string, toks []jsToken, k int, s string) bool {
	return k >= 0 && k < len(toks) && toks[k].kind == jsIdent && jsTokText(src, toks[k]) == s
}

// jsIsDotLike 属性访问前缀：".", "?."（可选链上的同名属性也不是全局变量）。
func jsIsDotLike(src string, toks []jsToken, k int) bool {
	return jsIsPunct(src, toks, k, ".") || jsIsPunct(src, toks, k, "?.")
}

// jsIsAssignOp 是否为赋值运算符（含复合赋值）。location.host = … / += … 都在
// 赋值左侧，不能替换成字符串字面量。
func jsIsAssignOp(src string, toks []jsToken, k int) bool {
	if k < 0 || k >= len(toks) || toks[k].kind != jsPunct {
		return false
	}
	switch jsTokText(src, toks[k]) {
	case "=", "+=", "-=", "*=", "/=", "%=", "**=", "<<=", ">>=", ">>>=",
		"&=", "|=", "^=", "&&=", "||=", "??=":
		return true
	}
	return false
}

// jsIsGlobalLocation 判断 token k 处的 "location" 是否指向全局 location。
//
//	location.host          → 是
//	window.location.host   → 是（限定符是全局对象）
//	self.location.host     → 是
//	n.location.host        → 否（自定义对象的同名属性，百度大包实测）
//	a?.location.host       → 否（可选链上的属性，保守跳过）
func jsIsGlobalLocation(src string, toks []jsToken, k int) bool {
	if !jsIsIdent(src, toks, k, "location") {
		return false
	}
	if !jsIsDotLike(src, toks, k-1) {
		return true
	}
	// 前面是 "."：只有 "window.location" / "self.location" 这类才成立
	if !jsIsPunct(src, toks, k-1, ".") || k-2 < 0 || toks[k-2].kind != jsIdent {
		return false
	}
	if !jsLocQualifiers[jsTokText(src, toks[k-2])] {
		return false
	}
	return !jsIsDotLike(src, toks, k-3) // x.window.location 不是全局
}

// jsLocQualifiers 可作为 location 前缀的全局对象。
var jsLocQualifiers = map[string]bool{
	"document": true, "window": true, "top": true, "self": true, "parent": true,
}

// jsLocProps 需要替换为面板值的 location 属性。
var jsLocProps = map[string]bool{
	"hostname": true, "host": true, "origin": true, "protocol": true, "port": true,
}

// browserRewriteJSLex 词法级改写：只改确定的 token 区间，绝不猜测。
func browserRewriteJSLex(js, panelScheme, panelHost string) string {
	vals := map[string]string{
		"hostname": `"` + panelHostname(panelHost) + `"`,
		"host":     `"` + panelHost + `"`,
		"origin":   `"` + panelScheme + "://" + panelHost + `"`,
		"protocol": `"` + panelScheme + `:"`,
		"port":     `"` + panelPort(panelHost) + `"`,
	}
	toks := jsTokenize(js)
	if len(toks) == 0 {
		return js
	}
	var edits []jsEdit
	occupied := func(s, e int) bool {
		for _, ed := range edits {
			if s < ed.end && ed.start < e {
				return true
			}
		}
		return false
	}
	add := func(s, e int, text string) {
		if s < e && !occupied(s, e) {
			edits = append(edits, jsEdit{s, e, text})
		}
	}

	for k := 0; k < len(toks); k++ {
		// 规则 1：location 赋值 → 经代理的导航（优先于规则 2，因为区间会重叠）
		if s, e, rhsStart, rhsEnd, ok := jsMatchLocAssign(srcOf(js), toks, k); ok {
			rhs := strings.TrimSpace(js[rhsStart:rhsEnd])
			term := ""
			if e <= len(js) && e > rhsEnd {
				term = ";" // 匹配吞掉了分号
			}
			if jsIsLocObjectText(rhs) {
				// 帧跳出惯用法：top.location = self.location（右侧是 Location 对象），
				// 包装成导航等于"把当前 URL 再导航一次" → iframe 里自己刷自己 → 白屏
				add(s, e, "void 0"+term)
			} else {
				add(s, e, "self.location.href=(window.__xbp?__xbp("+rhs+"):("+rhs+"))"+term)
			}
			continue
		}
		// 规则 2：location.host/hostname/origin/protocol/port 取值 → 面板值
		if jsIsIdent(js, toks, k, "location") && jsIsGlobalLocation(js, toks, k) &&
			jsIsPunct(js, toks, k+1, ".") && k+2 < len(toks) && toks[k+2].kind == jsIdent {
			prop := strings.ToLower(jsTokText(js, toks[k+2]))
			if jsLocProps[prop] {
				// 赋值左侧（location.host = x / += x）不能换成字面量，否则语法错误
				if !jsIsAssignOp(js, toks, k+3) {
					if v, has := vals[prop]; has {
						// "window.location.host" 要连限定符一起替换，
						// 否则会拼出 window."http://..." 这种语法错误
						from := toks[k].start
						if jsIsPunct(js, toks, k-1, ".") && k-2 >= 0 {
							from = toks[k-2].start
						}
						add(from, toks[k+2].end, v)
					}
				}
				continue
			}
		}
		// 规则 3：top.location → self.location（跨源读 top 抛 SecurityError）
		if jsIsIdent(js, toks, k, "top") && !jsIsDotLike(js, toks, k-1) &&
			jsIsPunct(js, toks, k+1, ".") && jsIsIdent(js, toks, k+2, "location") {
			add(toks[k].start, toks[k].end, "self")
			continue
		}
		// 规则 4：document.domain = "x" → 注释（现代浏览器直接抛错，中断整个脚本）
		if jsIsIdent(js, toks, k, "document") && !jsIsDotLike(js, toks, k-1) &&
			jsIsPunct(js, toks, k+1, ".") && jsIsIdent(js, toks, k+2, "domain") &&
			jsIsPunct(js, toks, k+3, "=") && k+4 < len(toks) && toks[k+4].kind == jsStr {
			add(toks[k].start, toks[k+4].end, "/*xb*/")
			continue
		}
	}
	if len(edits) == 0 {
		return js
	}
	// 按起点排序后拼接
	for i := 1; i < len(edits); i++ {
		for j := i; j > 0 && edits[j].start < edits[j-1].start; j-- {
			edits[j], edits[j-1] = edits[j-1], edits[j]
		}
	}
	var b strings.Builder
	b.Grow(len(js) + 128)
	last := 0
	for _, ed := range edits {
		if ed.start < last {
			continue
		}
		b.WriteString(js[last:ed.start])
		b.WriteString(ed.text)
		last = ed.end
	}
	b.WriteString(js[last:])
	return b.String()
}

// srcOf 仅用于让辅助函数签名统一（token 文本都取自同一份源码）。
func srcOf(js string) string { return js }

// jsMatchLocAssign 匹配"明确的 location 赋值"：
//
//	(window|document|top|self|parent).location[.href] = ...
//	location.href = ...
//
// 裸 location = x 不算——它可能是局部变量赋值（jQuery 1.x 就是
// location = window.location），改写会直接破坏代码。
//
// 返回替换区间 [s,e)（若吞到分号则 e 含分号）、右值区间 [rhsStart,rhsEnd)。
func jsMatchLocAssign(src string, toks []jsToken, k int) (s, e, rhsStart, rhsEnd int, ok bool) {
	eq := -1
	if toks[k].kind != jsIdent {
		return
	}
	word := jsTokText(src, toks[k])
	if jsIsDotLike(src, toks, k-1) {
		return
	}
	switch {
	case jsLocQualifiers[word]:
		if !jsIsPunct(src, toks, k+1, ".") || !jsIsIdent(src, toks, k+2, "location") {
			return
		}
		eq = k + 3
		if jsIsPunct(src, toks, eq, ".") && jsIsIdent(src, toks, eq+1, "href") {
			eq += 2
		}
	case word == "location":
		if !jsIsPunct(src, toks, k+1, ".") || !jsIsIdent(src, toks, k+2, "href") {
			return
		}
		eq = k + 3
	default:
		return
	}
	if !jsIsPunct(src, toks, eq, "=") {
		return
	}
	rhsStartTok := eq + 1
	if rhsStartTok >= len(toks) {
		return
	}
	rhsEndTok, term := jsExprEnd(src, toks, rhsStartTok)
	if rhsEndTok <= rhsStartTok {
		return
	}
	s = toks[k].start
	rhsStart = toks[rhsStartTok].start
	rhsEnd = toks[rhsEndTok-1].end
	e = rhsEnd
	if term {
		e = toks[rhsEndTok].end // 含分号
	}
	return s, e, rhsStart, rhsEnd, true
}

// jsExprEnd 找赋值右值的结束 token（按括号配平）。
// 返回结束 token 下标，以及该处是否为分号。
func jsExprEnd(src string, toks []jsToken, start int) (int, bool) {
	depth := 0
	for i := start; i < len(toks); i++ {
		if toks[i].kind == jsComment {
			continue
		}
		if toks[i].kind == jsPunct {
			switch jsTokText(src, toks[i]) {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				if depth == 0 {
					return i, false // 属于外层表达式，不吞
				}
				depth--
			case ";":
				if depth == 0 {
					return i, true // 分号归入替换区间
				}
			case ",":
				if depth == 0 {
					return i, false
				}
			}
		}
	}
	return len(toks), false
}

// jsIsLocObjectText 右值是否为"Location 对象本身"（而非地址字符串）。
// 支持 location、location.href 及带窗口限定的形式。
func jsIsLocObjectText(rhs string) bool {
	s := strings.TrimSpace(rhs)
	if s == "" {
		return false
	}
	// 去掉所有空白后再精确比对（token 间可能有空格/换行）
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n' {
			continue
		}
		b.WriteByte(s[i])
	}
	t := b.String()
	if t == "location" || t == "location.href" {
		return true
	}
	for q := range jsLocQualifiers {
		if t == q+".location" || t == q+".location.href" {
			return true
		}
	}
	return false
}
