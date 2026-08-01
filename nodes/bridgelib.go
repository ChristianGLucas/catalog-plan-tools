package nodes

// bridgelib: the type-bridge checker behind CheckBridges. Ported from the
// offline candidate engine's carrier taxonomy (carriers.py) and port index
// (engine.py): a "carrier" is a specific semantic wire-type (URL, IBAN,
// ISO-8601 instant, ...) that can bridge two nodes in a flow. Freeform
// string / arbitrary JSON are deliberately NOT carriers — they would connect
// every node to every node (zero signal) — which is why the absence of a
// carrier match is weak evidence and never alone yields a "blocked" verdict.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	gen "christiangeorgelucas/catalog-plan-tools/gen"
)

// planBase is the public, unauthenticated package-detail route of the Axiom
// registry: GET <planBase>/<handle>/<package>@<version> returns the node list
// (with input/output message names) and the package's proto source — the
// auth-gated per-node schema.json route is NOT needed. Overridden in tests.
var planBase = "https://api.axiomide.com/api/packages"

// ---------------------------------------------------------------------------
// Carrier taxonomy (port of carriers.py — keep the two in sync)
// ---------------------------------------------------------------------------

type carrierDef struct {
	name   string
	tier   int // 1=broad 2=medium 3=narrow; higher = stronger evidence
	nameRe *regexp.Regexp
	descRe *regexp.Regexp
	types  map[string]bool // JSON types the desc match is allowed on
}

func cd(name string, tier int, nameRe, descRe string, types ...string) carrierDef {
	ts := make(map[string]bool, len(types))
	for _, t := range types {
		ts[t] = true
	}
	return carrierDef{name, tier, regexp.MustCompile(nameRe), regexp.MustCompile(descRe), ts}
}

var carrierDefs = []carrierDef{
	// --- network / web ---
	cd("url", 1, `(^|_)(url|uri|link|href|endpoint|webhook)s?($|_)`, `\bhttps?://|\b(url|uri)\b`, "string"),
	cd("domain", 2, `(^|_)(domain|hostname|fqdn|host)s?($|_)`, `domain name|hostname|fqdn`, "string"),
	cd("email", 2, `(^|_)(email|e_mail|mail_address)s?($|_)`, `e-?mail address`, "string"),
	cd("ip_addr", 2, `(^|_)(ip|ip_addr|ipv4|ipv6|ip_address)s?($|_)`, `\bip(v4|v6)? address`, "string"),
	cd("mac_addr", 3, `(^|_)(mac|mac_addr|mac_address)s?($|_)`, `mac address`, "string"),
	cd("user_agent", 3, `(^|_)(user_agent|ua_string)s?($|_)`, `user-?agent`, "string"),
	cd("mime_type", 2, `(^|_)(mime|mimetype|content_type|media_type)s?($|_)`, `mime type|content-type`, "string"),
	cd("port", 2, `(^|_)port($|_)`, `\bport number`, "integer", "number"),
	// --- geo / time ---
	cd("latlon", 3, `(^|_)(lat|latitude|lon|lng|longitude)($|_)`, `latitude|longitude`, "number", "string"),
	cd("geojson", 3, `(^|_)geo_?json($|_)`, `geojson`, "string", "object"),
	cd("wkt", 3, `(^|_)wkt($|_)`, `well-known text`, "string"),
	cd("instant", 1, `(^|_)(timestamp|datetime|date_time|iso8601|instant)($|_)|(_at|_iso)$`, `iso-?8601|rfc-?3339|unix (time|seconds)|timestamp`, "string", "integer", "number"),
	cd("date", 2, `(^|_)date($|_)|_date$`, `\bdate\b \(yyyy|calendar date`, "string"),
	cd("duration", 2, `(^|_)(duration|ttl|timeout|elapsed)($|_)|_(seconds|ms|millis|secs)$`, `duration|in seconds|milliseconds`, "integer", "number", "string"),
	cd("timezone", 3, `(^|_)(timezone|tz|tzid)($|_)`, `iana time ?zone|tz database`, "string"),
	cd("cron", 3, `(^|_)(cron|crontab)($|_)|cron_expr`, `cron (expression|schedule)`, "string"),
	// --- money / codes ---
	cd("money", 2, `(^|_)(amount|price|cost|balance)($|_)`, `monetary amount|amount of money`, "number", "string", "integer"),
	cd("currency_code", 3, `(^|_)(currency|currency_code|ccy)($|_)`, `iso-?4217|currency code`, "string"),
	cd("country_code", 3, `(^|_)(country|country_code|iso_country)($|_)`, `iso-?3166|country code`, "string"),
	cd("lang_code", 3, `(^|_)(lang|language|locale|lang_code)($|_)`, `bcp-?47|iso-?639|language code|locale`, "string"),
	// --- financial / identity codes (narrow, high value) ---
	cd("iban", 3, `(^|_)iban($|_)`, `\biban\b`, "string"),
	cd("bic_swift", 3, `(^|_)(bic|swift)($|_)`, `\b(bic|swift) code`, "string"),
	cd("vin", 3, `(^|_)vin($|_)`, `vehicle identification`, "string"),
	cd("isbn", 3, `(^|_)isbn($|_)`, `\bisbn\b`, "string"),
	cd("uuid", 2, `(^|_)(uuid|guid)($|_)`, `\b(uuid|guid)\b`, "string"),
	cd("phone", 3, `(^|_)(phone|msisdn|e164|telephone)($|_)`, `phone number|e\.?164`, "string"),
	cd("hash", 2, `(^|_)(sha1|sha256|sha512|md5|hash|digest|checksum|fingerprint)($|_)`, `sha-?(1|256|512)|\bmd5\b|checksum|message digest`, "string"),
	cd("jwt", 3, `(^|_)(jwt|jws|jose)($|_)`, `json web token|\bjw(t|s)\b|jose`, "string"),
	cd("color", 3, `(^|_)(color|colour|hex_color|rgb)($|_)`, `hex color|rgb color|#rrggbb`, "string"),
	cd("semver", 2, `(^|_)(semver|version_range|version_constraint)($|_)`, `semver|semantic version|version range`, "string"),
	cd("regex", 2, `(^|_)(regex|regexp|pattern)($|_)`, `regular expression`, "string"),
	// --- document / media payloads ---
	cd("html", 2, `(^|_)html($|_)`, `html (document|markup|content)`, "string"),
	cd("markdown", 2, `(^|_)(markdown|md_text)($|_)|_md$`, `markdown (text|document|source)`, "string"),
	cd("xml", 2, `(^|_)xml($|_)`, `xml (document|content|string)`, "string"),
	cd("csv", 2, `(^|_)(csv|tsv)($|_)`, `csv|tsv|comma-separated|tab-separated`, "string"),
	cd("sql", 2, `(^|_)(sql|sql_query|statement)($|_)`, `\bsql\b (statement|query|text)`, "string"),
	cd("ical", 3, `(^|_)(ical|ics|vevent|icalendar)($|_)`, `icalendar|\bical\b`, "string"),
	cd("yaml", 2, `(^|_)yaml($|_)`, `yaml (document|content)`, "string"),
	cd("bytes_blob", 1, `(^|_)(bytes|blob|base64|file_data|content_bytes|raw_data)($|_)`, `base64|raw bytes|binary content`, "string"),
	cd("image", 2, `(^|_)(image|png|jpeg|jpg|photo|picture|thumbnail|img)($|_)`, `\b(image|png|jpeg)\b`, "string"),
	cd("pdf", 3, `(^|_)pdf($|_)`, `pdf document`, "string"),
	cd("audio", 3, `(^|_)(audio|wav|mp3)($|_)`, `audio (data|file|clip)`, "string"),
}

var carrierTier = func() map[string]int {
	m := make(map[string]int, len(carrierDefs))
	for _, d := range carrierDefs {
		m[d.name] = d.tier
	}
	return m
}()

// classifyCarriers returns the carriers a field earns. High precision: the
// field NAME matching is the strong signal (type-independent); a description
// match counts only on a compatible JSON type.
func classifyCarriers(name, jtype, desc string) []string {
	name = strings.ToLower(name)
	desc = strings.ToLower(desc)
	var out []string
	for _, d := range carrierDefs {
		if d.nameRe.MatchString(name) || (d.types[jtype] && d.descRe.MatchString(desc)) {
			out = append(out, d.name)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Proto-source parsing (the registry's public package detail carries the
// package's whole messages.proto; the per-node schema route is auth-gated)
// ---------------------------------------------------------------------------

type protoField struct {
	name     string
	typ      string // raw proto type token ("string", "int64", "PlanStep", ...)
	repeated bool
	desc     string // leading-comment documentation
}

var fieldRe = regexp.MustCompile(`^(repeated\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*\d+`)
var msgOpenRe = regexp.MustCompile(`^message\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{`)
var blockOpenRe = regexp.MustCompile(`^(enum|oneof|extend|service|rpc)\b`)
var blockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)

// protoLines splits proto source into one statement/comment per line so the
// parser is layout-independent: `message M { string a = 1; }` on one line
// parses the same as the multi-line form. Trailing same-line comments are
// DROPPED (the registry extracts leading comments only; re-ordering a
// trailing comment would mis-attach it to the next field), and blank lines
// are preserved because they terminate a leading-comment block.
func protoLines(src string) []string {
	var out []string
	breaker := strings.NewReplacer("{", "{\n", "}", "\n}\n", ";", ";\n")
	for _, raw := range strings.Split(src, "\n") {
		if strings.TrimSpace(raw) == "" {
			out = append(out, "")
			continue
		}
		code := raw
		if i := strings.Index(raw, "//"); i >= 0 {
			code = raw[:i]
			if strings.TrimSpace(code) == "" {
				out = append(out, strings.TrimSpace(raw)) // comment-only line
				continue
			}
			// code + trailing comment: keep the code, drop the comment
		}
		for _, l := range strings.Split(breaker.Replace(code), "\n") {
			if strings.TrimSpace(l) != "" {
				out = append(out, strings.TrimSpace(l))
			}
		}
	}
	return out
}

// parseProtoMessages extracts every message's fields (with leading-comment
// descriptions) from proto source. Brace-aware — a nested message neither
// corrupts its parent's field list nor is lost (it is registered under its
// simple name; a rare cross-scope name collision keeps the last definition).
// enum bodies are skipped; oneof members attach to the enclosing message.
// A message's opening brace must be on the declaration line (protoc style).
func parseProtoMessages(src string) map[string][]protoField {
	src = blockCommentRe.ReplaceAllString(src, "")
	msgs := make(map[string][]protoField)

	type frame struct {
		msgName string // "" for non-message blocks (enum, oneof, ...)
		skip    bool   // true inside enum/service bodies
	}
	var stack []frame
	curMsg := func() string {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].skip {
				return ""
			}
			if stack[i].msgName != "" {
				return stack[i].msgName
			}
		}
		return ""
	}

	var comment []string
	for _, line := range protoLines(src) {
		switch {
		case line == "":
			comment = nil
			continue
		case strings.HasPrefix(line, "//"):
			comment = append(comment, strings.TrimSpace(strings.TrimPrefix(line, "//")))
			continue
		}

		if m := msgOpenRe.FindStringSubmatch(line); m != nil {
			name := m[1]
			stack = append(stack, frame{msgName: name})
			if _, exists := msgs[name]; !exists {
				msgs[name] = nil
			} else {
				msgs[name] = nil // redefinition: last wins, start fresh
			}
			comment = nil
			continue
		}
		if blockOpenRe.MatchString(line) {
			// oneof members are ordinary fields of the enclosing message;
			// enum/service/extend bodies must not be scanned for fields.
			skip := !strings.HasPrefix(line, "oneof")
			stack = append(stack, frame{skip: skip})
			comment = nil
			if !strings.Contains(line, "{") {
				// opening brace on the next line: pop the frame back off and
				// re-push when it arrives — in practice scaffolded protos put
				// it on the same line, so treat a missing brace as same-frame.
				_ = line
			}
			continue
		}
		if strings.HasPrefix(line, "}") {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			comment = nil
			continue
		}

		if name := curMsg(); name != "" && strings.HasSuffix(line, ";") {
			if m := fieldRe.FindStringSubmatch(line); m != nil {
				typ := m[2]
				switch typ {
				case "option", "reserved", "map": // map<..> never matches fieldRe's type token anyway; guard the keywords
					comment = nil
					continue
				}
				msgs[name] = append(msgs[name], protoField{
					name:     m[3],
					typ:      typ,
					repeated: m[1] != "",
					desc:     strings.Join(comment, " "),
				})
			}
		}
		comment = nil
	}
	return msgs
}

// protoJSONType maps a proto scalar type to the JSON-schema type family the
// carrier taxonomy's desc-gate uses. "" means not a scalar (message type).
func protoJSONType(t string, isMessage bool) string {
	if isMessage {
		return ""
	}
	switch t {
	case "string", "bytes":
		return "string"
	case "bool":
		return "boolean"
	case "double", "float":
		return "number"
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64",
		"fixed32", "fixed64", "sfixed32", "sfixed64":
		return "integer"
	}
	// Unknown named type (an enum, or an unresolvable import): serialized as a
	// string in JSON — classify it as one rather than dropping the field.
	return "string"
}

// portLeaf is one classified schema field: a producer output leaf (dotted
// path) or a consumer top-level input field.
type portLeaf struct {
	path     string
	jtype    string
	carriers []string
}

// walkOutputs deep-walks a message's fields — every leaf is pickable by an
// edge adapter (value.record.phone), so producers expose their whole tree.
func walkOutputs(msgs map[string][]protoField, msgName string) []portLeaf {
	var out []portLeaf
	var rec func(name, prefix string, depth int, seen map[string]bool)
	rec = func(name, prefix string, depth int, seen map[string]bool) {
		if depth > 6 || seen[name] {
			return
		}
		seen[name] = true
		defer delete(seen, name)
		for _, f := range msgs[name] {
			_, isMsg := msgs[f.typ]
			path := prefix + f.name
			if isMsg {
				sep := "."
				if f.repeated {
					sep = "[]."
				}
				rec(f.typ, path+sep, depth+1, seen)
				continue
			}
			jt := protoJSONType(f.typ, false)
			out = append(out, portLeaf{path: path, jtype: jt, carriers: classifyCarriers(f.name, jt, f.desc)})
		}
	}
	rec(msgName, "", 0, map[string]bool{})
	return out
}

// feedableInputs returns the consumer's top-level fields a FOREIGN producer
// can actually feed via an edge adapter: scalars and repeated scalars. A
// message-typed input is a whole-message port a cross-package producer cannot
// satisfy leaf-by-leaf, so it is excluded (mirrors engine.py).
func feedableInputs(msgs map[string][]protoField, msgName string) []portLeaf {
	var out []portLeaf
	for _, f := range msgs[msgName] {
		if _, isMsg := msgs[f.typ]; isMsg {
			continue
		}
		jt := protoJSONType(f.typ, false)
		out = append(out, portLeaf{path: f.name, jtype: jt, carriers: classifyCarriers(f.name, jt, f.desc)})
	}
	return out
}

// ---------------------------------------------------------------------------
// Registry fetch + per-node port resolution
// ---------------------------------------------------------------------------

type pkgDetail struct {
	Nodes []struct {
		Name   string `json:"name"`
		Input  string `json:"input"`
		Output string `json:"output"`
	} `json:"nodes"`
	Messages []struct {
		Name        string `json:"name"`
		ProtoSource string `json:"proto_source"`
	} `json:"messages"`
}

// fetchPackageDetail GETs the public package-detail route for "handle/pkg" at
// an exact version pin.
func fetchPackageDetail(ctx context.Context, pkg, version string) (*pkgDetail, error) {
	parts := strings.SplitN(pkg, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("malformed package ref %q (want handle/name)", pkg)
	}
	u := planBase + "/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	if version != "" {
		u += url.PathEscape("@" + version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, detail)
	}
	var d pkgDetail
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("decoding package detail: %w", err)
	}
	return &d, nil
}

// nodePorts is a picked node's resolved schema surface. err carries the
// stage-attributed reason resolution failed ("" on success).
type nodePorts struct {
	ref      string // "package/Node@version"
	outLeafs []portLeaf
	inFields []portLeaf
	err      string
}

// resolvePorts turns a package detail + node name into that node's classified
// producer/consumer ports.
func resolvePorts(d *pkgDetail, ref, nodeName string) *nodePorts {
	np := &nodePorts{ref: ref}
	var inMsg, outMsg string
	for _, n := range d.Nodes {
		if n.Name == nodeName {
			inMsg, outMsg = n.Input, n.Output
			break
		}
	}
	if outMsg == "" && inMsg == "" {
		np.err = fmt.Sprintf("node %q not found in package detail", nodeName)
		return np
	}
	// Every message entry carries the package's proto source; parse each
	// distinct source once (multi-file packages concatenate cleanly).
	seen := map[string]bool{}
	msgs := make(map[string][]protoField)
	for _, m := range d.Messages {
		if m.ProtoSource == "" || seen[m.ProtoSource] {
			continue
		}
		seen[m.ProtoSource] = true
		for name, fields := range parseProtoMessages(m.ProtoSource) {
			msgs[name] = fields
		}
	}
	if _, ok := msgs[outMsg]; !ok {
		np.err = fmt.Sprintf("output message %q not found in package proto source", outMsg)
		return np
	}
	if _, ok := msgs[inMsg]; !ok {
		np.err = fmt.Sprintf("input message %q not found in package proto source", inMsg)
		return np
	}
	np.outLeafs = walkOutputs(msgs, outMsg)
	np.inFields = feedableInputs(msgs, inMsg)
	return np
}

// ---------------------------------------------------------------------------
// The verdict
// ---------------------------------------------------------------------------

// judgeBridge renders one BridgeCheck between a producing and a consuming
// pick. Verdict ladder (see the proto docs): carrier match => "bridged";
// structural impossibility => "blocked"; fetch/parse failure => "unknown";
// otherwise => "plausible".
func judgeBridge(from, to *nodePorts) *gen.BridgeCheck {
	bc := &gen.BridgeCheck{FromNode: from.ref, ToNode: to.ref}
	if from.err != "" || to.err != "" {
		bc.Verdict = "unknown"
		var parts []string
		if from.err != "" {
			parts = append(parts, from.ref+": "+from.err)
		}
		if to.err != "" {
			parts = append(parts, to.ref+": "+to.err)
		}
		bc.Detail = "schema unavailable — " + strings.Join(parts, "; ")
		return bc
	}

	// Strongest shared carrier wins; break ties toward the shortest output
	// path (a top-level field beats a deeply nested one as the example).
	bestTier, bestPathLen := 0, 0
	for _, ol := range from.outLeafs {
		if len(ol.carriers) == 0 {
			continue
		}
		for _, inf := range to.inFields {
			for _, c := range ol.carriers {
				if !contains(inf.carriers, c) {
					continue
				}
				t := carrierTier[c]
				if t > bestTier || (t == bestTier && len(ol.path) < bestPathLen) {
					bestTier, bestPathLen = t, len(ol.path)
					bc.Via = fmt.Sprintf("%s: %s -> %s", c, ol.path, inf.path)
				}
			}
		}
	}
	if bestTier > 0 {
		bc.Verdict = "bridged"
		bc.Compatible = true
		return bc
	}

	if len(to.inFields) == 0 {
		bc.Verdict = "blocked"
		bc.Detail = "consumer exposes no top-level scalar input a foreign producer can feed via an edge adapter (message-typed inputs only)"
		return bc
	}
	// Producer scalar leaves, ignoring the ubiquitous "error" field — an
	// error string feeding anything is not a real bridge.
	outTypes := map[string]bool{}
	nOut := 0
	for _, ol := range from.outLeafs {
		if ol.path == "error" {
			continue
		}
		outTypes[ol.jtype] = true
		nOut++
	}
	if nOut == 0 {
		bc.Verdict = "blocked"
		bc.Detail = "producer emits no output leaf (beyond an error string) an edge adapter could pick"
		return bc
	}
	inTypes := map[string]bool{}
	for _, inf := range to.inFields {
		inTypes[inf.jtype] = true
	}
	var overlap []string
	for t := range outTypes {
		if inTypes[t] {
			overlap = append(overlap, t)
		}
	}
	sort.Strings(overlap)
	if len(overlap) == 0 {
		bc.Verdict = "blocked"
		bc.Detail = fmt.Sprintf("type sets are disjoint: producer emits only %s, consumer accepts only %s",
			typeList(outTypes), typeList(inTypes))
		return bc
	}
	bc.Verdict = "plausible"
	bc.Detail = fmt.Sprintf("no shared typed carrier; %d producer leaves and %d consumer inputs overlap on %s — the edge is possible but unverified, pair the fields manually",
		nOut, len(to.inFields), strings.Join(overlap, "/"))
	return bc
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func typeList(m map[string]bool) string {
	var out []string
	for t := range m {
		out = append(out, t)
	}
	sort.Strings(out)
	return strings.Join(out, "/")
}
