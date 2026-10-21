package fakeanthropic

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// BindingErrorText is the fixed clause of the API's 400 for a thinking
// block replayed against a different conversation; the fake's message is
// "messages.<i>.content.<j>: " + BindingErrorText. It is modelled on the
// error-mode text of the preserved-thinking docs, and deploy spike L5
// replaces it with the exact live text. It must always contain
// protocol/fixtures/binding-error.txt (S9), which the tests check.
const BindingErrorText = "Invalid `signature` in `thinking` block. The block is bound to a different conversation. " +
	"Remove the block, or set `thinking.block_binding.prefix_mismatch_behavior` to \"drop_block\"."

// Canonical is the byte form a thinking signature's prefix MAC covers
// (README "Canonicalisation"). Given a request, or a {system, tools,
// messages} prefix, it keeps only system, tools and messages, and it
// ignores what the API's binding check ignores (Ruling S24):
//
//  1. every "cache_control" member, at any depth;
//  2. string content versus a single plain text block (system,
//     messages[*].content, tool_result content): the block becomes its
//     string;
//  3. leading and trailing white space of a text block;
//  4. text blocks that are only white space (dropped);
//  5. key order: every object's keys are sorted by their decoded bytes;
//  6. the order of tools: they are sorted by name, a name-keyed set;
//  7. request parameters other than system, tools and messages;
//  8. thinking and redacted_thinking blocks (the chain rule checks them;
//     see checkBinding).
//
// It is re-encoded with no whitespace between tokens. Everything else
// counts: strings (apart from rule 3's edges) and numbers are copied byte
// for byte as sent, escapes included. Input that is not a JSON object gets
// rules 1 and 5 only. Invalid JSON gives nil.
func Canonical(prefix json.RawMessage) []byte {
	root, ok := parseJSON(prefix)
	if !ok {
		return nil
	}
	if root.kind != '{' {
		var out bytes.Buffer
		root.write(&out)
		return out.Bytes()
	}
	var out bytes.Buffer
	out.WriteByte('{')
	sep := ""
	part := func(key string, b []byte) {
		out.WriteString(sep + `"` + key + `":`)
		out.Write(b)
		sep = ","
	}
	if m := root.get("messages"); m != nil {
		var b bytes.Buffer
		if m.kind == '[' {
			b.WriteByte('[')
			for i, e := range m.elems {
				if i > 0 {
					b.WriteByte(',')
				}
				b.Write(canonicalMessage(e))
			}
			b.WriteByte(']')
		} else {
			m.write(&b)
		}
		part("messages", b.Bytes())
	}
	if s := root.get("system"); s != nil {
		part("system", canonicalSystem(s))
	}
	if t := root.get("tools"); t != nil {
		part("tools", canonicalTools(t))
	}
	out.WriteByte('}')
	return out.Bytes()
}

func canonicalMessage(m *node) []byte {
	if c := m.get("content"); c != nil {
		m = m.with("content", normalizeContent(c))
	}
	var out bytes.Buffer
	m.write(&out)
	return out.Bytes()
}

func canonicalSystem(s *node) []byte {
	var out bytes.Buffer
	normalizeContent(s).write(&out)
	return out.Bytes()
}

// canonicalTools sorts the tools by name (rule 6).
func canonicalTools(t *node) []byte {
	if t.kind == '[' {
		sorted := *t
		sorted.elems = slices.Clone(t.elems)
		slices.SortStableFunc(sorted.elems, func(a, b *node) int {
			return strings.Compare(a.get("name").str(), b.get("name").str())
		})
		t = &sorted
	}
	var out bytes.Buffer
	t.write(&out)
	return out.Bytes()
}

// normalizeContent applies rules 2, 3, 4 and 8 to a content value (a
// string or an array of blocks), and recurses into tool_result content.
func normalizeContent(c *node) *node {
	var blocks []*node
	switch c.kind {
	case '"':
		blocks = []*node{textBlockOf(c)}
	case '[':
		blocks = c.elems
	default:
		return c
	}
	out := make([]*node, 0, len(blocks))
	for _, b := range blocks {
		switch b.get("type").str() {
		case "thinking", "redacted_thinking":
			continue // rule 8
		case "text":
			if t := b.get("text"); t != nil && t.kind == '"' {
				trimmed := trimString(t.raw) // rule 3
				if len(trimmed) == 2 {
					continue // rule 4: `""`
				}
				b = b.with("text", &node{kind: '"', raw: trimmed})
			}
		case "tool_result":
			if tc := b.get("content"); tc != nil {
				b = b.with("content", normalizeContent(tc))
			}
		}
		out = append(out, b)
	}
	if len(out) == 1 && isPlainText(out[0]) {
		return out[0].get("text") // rule 2
	}
	return &node{kind: '[', elems: out}
}

func textBlockOf(s *node) *node {
	return &node{kind: '{', members: []member{
		{key: "type", raw: []byte(`"type"`), val: &node{kind: '"', raw: []byte(`"text"`)}},
		{key: "text", raw: []byte(`"text"`), val: s},
	}}
}

// isPlainText reports a text block with nothing but its type and text
// (cache_control aside): one that a string can stand for.
func isPlainText(b *node) bool {
	if b.kind != '{' || b.get("type").str() != "text" {
		return false
	}
	for _, m := range b.members {
		switch m.key {
		case "cache_control", "type":
		case "text":
			if m.val.kind != '"' {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// trimString drops leading and trailing white space (unicode.IsSpace)
// from a raw string token, escaped or not, and keeps the bytes in between
// exactly as sent.
func trimString(raw []byte) []byte {
	c := raw[1 : len(raw)-1]
	first, last := -1, -1
	for i := 0; i < len(c); {
		start := i
		var r rune
		r, i = nextRune(c, i)
		if !unicode.IsSpace(r) {
			if first < 0 {
				first = start
			}
			last = i
		}
	}
	switch {
	case first < 0:
		return []byte(`""`)
	case first == 0 && last == len(c):
		return raw
	}
	out := make([]byte, 0, last-first+2)
	out = append(out, '"')
	out = append(out, c[first:last]...)
	return append(out, '"')
}

// nextRune decodes one character of a valid JSON string's contents at i,
// an escape or a UTF-8 sequence, and returns it with the next offset.
func nextRune(c []byte, i int) (rune, int) {
	if c[i] != '\\' {
		r, n := utf8.DecodeRune(c[i:])
		return r, i + n
	}
	if c[i+1] != 'u' {
		switch c[i+1] {
		case 'n':
			return '\n', i + 2
		case 't':
			return '\t', i + 2
		case 'r':
			return '\r', i + 2
		case 'b':
			return '\b', i + 2
		case 'f':
			return '\f', i + 2
		default: // \" \\ \/
			return rune(c[i+1]), i + 2
		}
	}
	r := hex4(c[i+2 : i+6])
	i += 6
	if utf16.IsSurrogate(r) && i+6 <= len(c) && c[i] == '\\' && c[i+1] == 'u' {
		if d := utf16.DecodeRune(r, hex4(c[i+2:i+6])); d != unicode.ReplacementChar {
			return d, i + 6
		}
	}
	return r, i
}

func hex4(b []byte) rune {
	v, _ := strconv.ParseUint(string(b), 16, 32) // valid: json.Valid accepted it
	return rune(v)
}

// node is a parsed JSON value whose scalars keep their bytes as sent.
type node struct {
	kind    byte     // '{' object, '[' array, '"' string, '0' number/true/false/null
	src     []byte   // the value's bytes as sent (parsed nodes only)
	raw     []byte   // a scalar's token as sent, a string's quotes included
	members []member // an object's members, in the order sent
	elems   []*node  // an array's elements
}

type member struct {
	key string // decoded, for lookups and sorting
	raw []byte // as sent
	val *node
}

// get returns the object member named key (the last, as encoding/json
// does), or nil.
func (n *node) get(key string) *node {
	if n == nil || n.kind != '{' {
		return nil
	}
	for i := len(n.members) - 1; i >= 0; i-- {
		if n.members[i].key == key {
			return n.members[i].val
		}
	}
	return nil
}

// str is a string node's decoded value, or "" for anything else.
func (n *node) str() string {
	if n == nil || n.kind != '"' {
		return ""
	}
	var s string
	_ = json.Unmarshal(n.raw, &s) // valid: json.Valid accepted it
	return s
}

// with returns a copy of an object with member key's value replaced.
func (n *node) with(key string, v *node) *node {
	c := *n
	c.members = slices.Clone(n.members)
	for i := range c.members {
		if c.members[i].key == key {
			c.members[i].val = v
		}
	}
	return &c
}

// write encodes n with sorted keys, no cache_control and no whitespace
// (rules 1 and 5).
func (n *node) write(out *bytes.Buffer) {
	switch n.kind {
	case '{':
		ms := make([]member, 0, len(n.members))
		for _, m := range n.members {
			if m.key != "cache_control" {
				ms = append(ms, m)
			}
		}
		slices.SortStableFunc(ms, func(a, b member) int { return strings.Compare(a.key, b.key) })
		out.WriteByte('{')
		for i, m := range ms {
			if i > 0 {
				out.WriteByte(',')
			}
			out.Write(m.raw)
			out.WriteByte(':')
			m.val.write(out)
		}
		out.WriteByte('}')
	case '[':
		out.WriteByte('[')
		for i, e := range n.elems {
			if i > 0 {
				out.WriteByte(',')
			}
			e.write(out)
		}
		out.WriteByte(']')
	default:
		out.Write(n.raw)
	}
}

// parseJSON parses b into a node tree, or reports false for invalid JSON.
func parseJSON(b []byte) (*node, bool) {
	if !json.Valid(b) {
		return nil, false
	}
	p := parser{src: b}
	return p.value(), true
}

// parser walks JSON that json.Valid has accepted, so it does not re-check
// the grammar.
type parser struct {
	src []byte
	pos int
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value() *node {
	p.skipSpace()
	start := p.pos
	var n *node
	switch p.src[p.pos] {
	case '{':
		n = p.object()
	case '[':
		n = p.array()
	case '"':
		n = &node{kind: '"', raw: p.str()}
	default:
		n = p.scalar()
	}
	n.src = p.src[start:p.pos]
	return n
}

// str returns the raw string token at pos, quotes and escapes included.
func (p *parser) str() []byte {
	start := p.pos
	p.pos++
	for {
		switch p.src[p.pos] {
		case '\\':
			p.pos += 2
		case '"':
			p.pos++
			return p.src[start:p.pos]
		default:
			p.pos++
		}
	}
}

func (p *parser) scalar() *node {
	start := p.pos
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ',', ']', '}', ' ', '\t', '\n', '\r':
			return &node{kind: '0', raw: p.src[start:p.pos]}
		}
		p.pos++
	}
	return &node{kind: '0', raw: p.src[start:]}
}

func (p *parser) array() *node {
	n := &node{kind: '['}
	p.pos++ // [
	p.skipSpace()
	if p.src[p.pos] == ']' {
		p.pos++
		return n
	}
	for {
		n.elems = append(n.elems, p.value())
		p.skipSpace()
		if p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		p.pos++ // ]
		return n
	}
}

func (p *parser) object() *node {
	n := &node{kind: '{'}
	p.pos++ // {
	p.skipSpace()
	if p.src[p.pos] == '}' {
		p.pos++
		return n
	}
	for {
		p.skipSpace()
		raw := p.str()
		p.skipSpace()
		p.pos++ // :
		v := p.value()
		n.members = append(n.members, member{key: decodeKey(raw), raw: raw, val: v})
		p.skipSpace()
		if p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		p.pos++ // }
		return n
	}
}

func decodeKey(raw []byte) string {
	if !bytes.ContainsRune(raw, '\\') {
		return string(raw[1 : len(raw)-1])
	}
	var s string
	_ = json.Unmarshal(raw, &s) // valid: json.Valid accepted the document
	return s
}

// boundRequest is the part of a request the binding covers.
type boundRequest struct {
	system, tools *node // nil when absent
	messages      []*node
	decoded       []apiMessage // for the script marker only; the check reads the nodes
}

// prefixSigner computes HMAC-SHA256(key, Canonical(prefix)) for the prefix
// of each message in turn, without re-hashing earlier messages. Canonical
// of a {system, tools, messages} prefix is
//
//	{"messages":[m0,m1,...],"system":S,"tools":T}
//
// (an absent part is left out), so a running MAC over the head and the
// messages, cloned and finished with the tail, gives each prefix's MAC in
// one pass.
type prefixSigner struct {
	key   []byte
	mac   hash.Hash
	canon [][]byte // canonical messages written so far
	tail  []byte
}

func newPrefixSigner(key []byte, r *boundRequest) *prefixSigner {
	var tail bytes.Buffer
	tail.WriteByte(']')
	if r.system != nil {
		tail.WriteString(`,"system":`)
		tail.Write(canonicalSystem(r.system))
	}
	if r.tools != nil {
		tail.WriteString(`,"tools":`)
		tail.Write(canonicalTools(r.tools))
	}
	tail.WriteByte('}')
	p := &prefixSigner{key: key, mac: hmac.New(sha256.New, key), tail: tail.Bytes()}
	p.mac.Write([]byte(`{"messages":[`))
	return p
}

// add appends one message to the prefix.
func (p *prefixSigner) add(m *node) {
	c := canonicalMessage(m)
	if len(p.canon) > 0 {
		p.mac.Write([]byte{','})
	}
	p.mac.Write(c)
	p.canon = append(p.canon, c)
}

// sum is the MAC of the prefix so far.
func (p *prefixSigner) sum() []byte {
	if cl, ok := p.mac.(hash.Cloner); ok {
		if h, err := cl.Clone(); err == nil {
			_, _ = h.Write(p.tail) // a hash's Write never fails
			return h.Sum(nil)
		}
	}
	// No clonable state (a FIPS build): hash the prefix afresh.
	h := hmac.New(sha256.New, p.key)
	h.Write([]byte(`{"messages":[`))
	h.Write(bytes.Join(p.canon, []byte{','}))
	h.Write(p.tail)
	return h.Sum(nil)
}

// A signature is the block's prefix MAC (32 bytes), followed, when the
// block was generated after earlier thinking, by a 16-byte chain tag that
// binds it to the last thinking block of the request it answered.
const (
	macLen   = sha256.Size
	chainLen = 16
)

func chainTag(key, mac, predecessor []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("fakeanthropic chain\x00"))
	h.Write(mac)
	h.Write(predecessor)
	return h.Sum(nil)[:chainLen]
}

func sign(key, mac, predecessor []byte) []byte {
	if predecessor == nil {
		return mac
	}
	return append(slices.Clone(mac), chainTag(key, mac, predecessor)...)
}

// validBlock checks one replayed signature against its message's prefix
// MAC and, unless it is the first block replayed, against the replayed
// block before it (prev).
func validBlock(key, sig, mac, prev []byte) bool {
	switch len(sig) {
	case macLen: // generated with no thinking before it
		return hmac.Equal(sig, mac) && prev == nil
	case macLen + chainLen:
		return hmac.Equal(sig[:macLen], mac) &&
			(prev == nil || hmac.Equal(sig[macLen:], chainTag(key, mac, prev)))
	default:
		return false
	}
}

// bindingFailure locates the first replayed block that fails the check.
// A tampered block (one the fake cannot have minted) gets the API's
// invalid-signature text; a well-formed block replayed against another
// prefix or out of its chain gets BindingErrorText.
type bindingFailure struct {
	msg, block int
	tampered   bool
	typ        string // thinking or redacted_thinking
}

func (f bindingFailure) message() string {
	if f.tampered {
		field := "signature"
		if f.typ == "redacted_thinking" {
			field = "data"
		}
		return fmt.Sprintf("messages.%d.content.%d: Invalid `%s` in `%s` block.", f.msg, f.block, field, f.typ)
	}
	return fmt.Sprintf("messages.%d.content.%d: %s", f.msg, f.block, BindingErrorText)
}

// replayedSignature reads a thinking or redacted_thinking block the way it
// was minted: a thinking block carries `thinking: ""` (the fake mints no
// text, so any other value is an edit) and a base64 `signature`, a
// redacted_thinking block a base64 `data`, each of a signature's length.
// Anything else is tampering.
func replayedSignature(b *node, typ string) ([]byte, bool) {
	field := "data"
	if typ == "thinking" {
		field = "signature"
		if t := b.get("thinking"); t == nil || t.kind != '"' || t.str() != "" {
			return nil, false
		}
	}
	s := b.get(field)
	if s == nil || s.kind != '"' {
		return nil, false
	}
	sig, err := base64.StdEncoding.DecodeString(s.str())
	if err != nil || (len(sig) != macLen && len(sig) != macLen+chainLen) {
		return nil, false
	}
	return sig, true
}

// checkBinding re-verifies every thinking and redacted_thinking block of
// every assistant message, and returns the signature for a reply to this
// request. It reads the parse tree, so it fails closed: every block whose
// type is thinking or redacted_thinking is checked, whatever surrounds it.
// Each block must be as minted (replayedSignature), match its own
// message's prefix (Canonical, which leaves thinking out), and the replayed
// blocks must be a contiguous window of the sequence they were generated
// in: every block after the first replayed one must chain to the replayed
// block just before it. Dropping blocks from the front, the back or both
// passes; a block missing from the middle, a reorder, or a block put back
// after a later one was generated without it fails the block after the gap.
func checkBinding(key []byte, r *boundRequest) (replySig string, fail *bindingFailure) {
	p := newPrefixSigner(key, r)
	var prev []byte // the previous replayed block's signature
	for i, m := range r.messages {
		if c := m.get("content"); m.get("role").str() == "assistant" && c != nil && c.kind == '[' {
			var mac []byte
			for j, b := range c.elems {
				typ := b.get("type").str()
				if typ != "thinking" && typ != "redacted_thinking" {
					continue
				}
				sig, ok := replayedSignature(b, typ)
				if !ok {
					return "", &bindingFailure{msg: i, block: j, tampered: true, typ: typ}
				}
				if mac == nil {
					mac = p.sum()
				}
				if !validBlock(key, sig, mac, prev) {
					return "", &bindingFailure{msg: i, block: j, typ: typ}
				}
				prev = sig
			}
		}
		p.add(m)
	}
	return base64.StdEncoding.EncodeToString(sign(key, p.sum(), prev)), nil
}
