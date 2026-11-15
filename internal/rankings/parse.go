package rankings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// ---------- HTML DOM 小工具 ----------

func parseHTMLDoc(raw []byte) (*html.Node, error) {
	return html.Parse(bytes.NewReader(raw))
}

// walk 深度优先遍历，fn 返回 false 时不再进入该节点的子树。
func walk(n *html.Node, fn func(*html.Node) bool) {
	if n == nil || !fn(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

func findFirst(n *html.Node, pred func(*html.Node) bool) *html.Node {
	var found *html.Node
	walk(n, func(x *html.Node) bool {
		if found != nil {
			return false
		}
		if pred(x) {
			found = x
			return false
		}
		return true
	})
	return found
}

func collect(n *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	walk(n, func(x *html.Node) bool {
		if pred(x) {
			out = append(out, x)
		}
		return true
	})
	return out
}

func nodeClass(n *html.Node) string {
	if n == nil {
		return ""
	}
	return attr(n, "class")
}

func hasClass(n *html.Node, cls string) bool {
	for _, f := range strings.Fields(nodeClass(n)) {
		if f == cls {
			return true
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// textOf 取节点全部文本并把连续空白压成一个空格。
func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	walk(n, func(x *html.Node) bool {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		return true
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

func imgSrc(n *html.Node) string {
	for _, k := range []string{"data-src", "data-original", "src"} {
		if v := strings.TrimSpace(attr(n, k)); v != "" {
			return v
		}
	}
	return ""
}

func absURL(base, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	b, err := url.Parse(base)
	if err != nil {
		return href
	}
	return b.ResolveReference(u).String()
}

func atoiSafe(s string) int {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// ---------- Nuxt payload 解码 ----------

// nuxtMarkers 是 Nuxt 3 payload 里 devalue 序列化产生的包装标记。
var nuxtMarkers = map[string]bool{
	"ShallowReactive": true,
	"Reactive":        true,
	"ShallowRef":      true,
	"Ref":             true,
	"EmptyRef":        true,
	"EmptyShallowRef": true,
}

// decodeNuxtPayload 还原 Nuxt 3 的扁平化 payload。
//
// 该格式把所有对象/数组拍平进一个 JSON 数组，容器里的数字是「下标引用」，
// 根节点固定是 0 号元素。直接按普通 JSON 解析只会拿到一堆数字。
func decodeNuxtPayload(raw []byte) (any, error) {
	var flat []any
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, err
	}
	if len(flat) == 0 {
		return nil, fmt.Errorf("payload 为空")
	}
	values := make([]any, len(flat))
	state := make([]int8, len(flat)) // 0 未解析 1 解析中 2 完成

	var resolve func(int) any
	resolve = func(i int) any {
		if i < 0 || i >= len(flat) {
			return nil
		}
		if state[i] == 2 {
			return values[i]
		}
		if state[i] == 1 { // 循环引用，直接断开
			return nil
		}
		state[i] = 1
		switch v := flat[i].(type) {
		case map[string]any:
			m := make(map[string]any, len(v))
			values[i] = m
			for k, raw := range v {
				if idx, ok := refIndex(raw); ok {
					m[k] = resolve(idx)
				} else {
					m[k] = raw
				}
			}
		case []any:
			// ["ShallowReactive", 2] 这类标记：取被包住的那个值
			if len(v) > 0 {
				if marker, ok := v[0].(string); ok && nuxtMarkers[marker] && len(v) > 1 {
					if idx, ok := refIndex(v[1]); ok {
						values[i] = resolve(idx)
					} else {
						values[i] = v[1]
					}
					state[i] = 2
					return values[i]
				}
			}
			arr := make([]any, 0, len(v))
			values[i] = arr
			for _, raw := range v {
				if idx, ok := refIndex(raw); ok {
					arr = append(arr, resolve(idx))
				} else {
					arr = append(arr, raw)
				}
			}
			values[i] = arr
		default:
			values[i] = v
		}
		state[i] = 2
		return values[i]
	}
	return resolve(0), nil
}

// refIndex 判断一个值是不是 payload 数组下标引用。
func refIndex(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		i := int(n)
		if float64(i) == n && i >= 0 {
			return i, true
		}
	case int:
		if n >= 0 {
			return n, true
		}
	case json.Number:
		i, err := n.Int64()
		if err == nil && i >= 0 {
			return int(i), true
		}
	}
	return 0, false
}

// findRankingList 在解码后的任意结构里找出「最长的一组成员对象」。
// 不依赖具体字段路径，站点改版把 list 挪一层也不会直接失效。
func findRankingList(v any) []map[string]any {
	var best []map[string]any
	var visit func(any)
	visit = func(x any) {
		switch t := x.(type) {
		case []any:
			items := make([]map[string]any, 0, len(t))
			for _, e := range t {
				if m, ok := e.(map[string]any); ok {
					if _, hasTitle := m["title"]; hasTitle {
						items = append(items, m)
					}
				}
			}
			if len(items) > len(best) {
				best = items
			}
			for _, e := range t {
				visit(e)
			}
		case map[string]any:
			for _, e := range t {
				visit(e)
			}
		}
	}
	visit(v)
	return best
}

func strVal(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch s := v.(type) {
			case string:
				if strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			case float64:
				return strconv.FormatInt(int64(s), 10)
			}
		}
	}
	return ""
}

func intVal(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return int(n)
			case string:
				return atoiSafe(n)
			}
		}
	}
	return 0
}

// ---------- HTTP ----------

func limitRead(r io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, n))
}
