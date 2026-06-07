package iop

import (
	"crypto/md5"
	"fmt"
	"strings"

	"github.com/flarco/g"
	"github.com/spf13/cast"
)

// IndexDef is the unified internal representation of an index, populated both
// from the columns: inline modifier DSL and from target_options.table_keys.index.
// DDL generation reads from this single source of truth.
type IndexDef struct {
	Name       string        `json:"name,omitempty"`       // auto-generated if empty
	Columns    []IndexColumn `json:"columns,omitempty"`    // mutually exclusive with Expression
	Expression string        `json:"expression,omitempty"` // raw SQL for expression indexes
	Unique     bool          `json:"unique,omitempty"`
	Where      string        `json:"where,omitempty"`
	Type       string        `json:"type,omitempty"` // btree / hash / gin / ...
	Include    []string      `json:"include,omitempty"`

	// Priority is used only while merging column-inline composite indexes (same
	// index Name across multiple columns); it orders the columns and is not part
	// of the rendered DDL.
	Priority int `json:"priority,omitempty"`
}

// IndexColumn is one column (or per-column expression) within an index.
type IndexColumn struct {
	Name string `json:"name"`           // column name or expression
	Sort string `json:"sort,omitempty"` // "asc" / "desc" / ""
}

// ColumnNames returns the column/expression names of the index.
func (id IndexDef) ColumnNames() (names []string) {
	for _, c := range id.Columns {
		names = append(names, c.Name)
	}
	return
}

// reservedModifiers are recognized as well-formed but not yet supported in v1.
// The tokenizer accepts their shape and returns a clear "not yet supported" error
// so they can be added later without grammar changes.
var reservedModifiers = map[string]bool{
	"default":        true,
	"check":          true,
	"auto_increment": true,
	"identity":       true,
}

// ParseModifiers parses the column type slot for the modifier DSL:
//
//	<type> [<modifier> ...]
//
// where <modifier> is one of: not_null, nullable, primary_key, unique,
// description(<text>), index, index(<kwargs>), unique_index(<kwargs>).
//
// It mutates the column's Type, Metadata, Description and inline index metadata.
// It must be called on the type slot only (after the `|` runtime-constraint split
// performed by SetConstraint).
func (col *Column) ParseModifiers() (err error) {
	raw := strings.TrimSpace(string(col.Type))
	if raw == "" {
		return nil
	}

	tokens, err := tokenizeModifiers(raw)
	if err != nil {
		return g.Error(err, "could not parse modifiers for column %s", col.Name)
	}
	if len(tokens) <= 1 {
		return nil // just a type, no modifiers
	}

	// first token is the type (may itself carry (length)/(precision,scale))
	col.Type = ColumnType(tokens[0])

	var indexDefs []IndexDef
	sawNotNull, sawNullable := false, false

	for _, tok := range tokens[1:] {
		name, payload, hasPayload := splitModifier(tok)
		lname := strings.ToLower(name)

		// reserved-but-unsupported modifiers (with or without payload)
		if reservedModifiers[lname] {
			return g.Error("column %s: modifier %q is not yet supported", col.Name, lname)
		}

		switch lname {
		case "not_null":
			if hasPayload {
				return g.Error("column %s: modifier not_null does not take arguments", col.Name)
			}
			sawNotNull = true
			col.SetMetadata(ColMetaNullable.String(), "false")
			col.SetMetadata(ColMetaDDLExplicit.String(), "true")
		case "nullable":
			if hasPayload {
				return g.Error("column %s: modifier nullable does not take arguments", col.Name)
			}
			sawNullable = true
			col.SetMetadata(ColMetaNullable.String(), "true")
			col.SetMetadata(ColMetaDDLExplicit.String(), "true")
		case "primary_key":
			if hasPayload {
				return g.Error("column %s: modifier primary_key does not take arguments", col.Name)
			}
			col.SetMetadata(ColMetaIsPrimaryKey.String(), "true")
			col.SetMetadata(ColMetaDDLExplicit.String(), "true")
		case "unique":
			if hasPayload {
				return g.Error("column %s: modifier unique does not take arguments", col.Name)
			}
			col.SetMetadata(ColMetaUnique.String(), "true")
			col.SetMetadata(ColMetaDDLExplicit.String(), "true")
		case "description":
			if !hasPayload {
				return g.Error("column %s: modifier description requires a value, e.g. description('...')", col.Name)
			}
			text, err := parseStringPayload(payload)
			if err != nil {
				return g.Error(err, "column %s: invalid description value", col.Name)
			}
			col.Description = text
			col.SetMetadata(ColMetaDescription.String(), text)
			col.SetMetadata(ColMetaDDLExplicit.String(), "true")
		case "index", "unique_index":
			def, err := parseIndexModifier(payload, hasPayload, lname == "unique_index")
			if err != nil {
				return g.Error(err, "column %s: invalid %s modifier", col.Name, lname)
			}
			indexDefs = append(indexDefs, def)
		default:
			return g.Error("column %s: unknown modifier %q", col.Name, name)
		}
	}

	if sawNotNull && sawNullable {
		return g.Error("column %s: conflicting modifiers not_null and nullable", col.Name)
	}

	if len(indexDefs) > 0 {
		col.SetMetadata(ColMetaIndex.String(), g.Marshal(indexDefs))
	}

	return nil
}

// tokenizeModifiers splits a string on whitespace while keeping balanced parens
// and quoted runs (single quotes, double quotes and backticks) intact.
func tokenizeModifiers(s string) (tokens []string, err error) {
	var b strings.Builder
	depth := 0
	var quote rune // 0 if not in quote, else the quote rune

	flush := func() {
		if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}

	for _, r := range s {
		switch {
		case quote != 0:
			b.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"' || r == '`':
			quote = r
			b.WriteRune(r)
		case r == '(':
			depth++
			b.WriteRune(r)
		case r == ')':
			depth--
			if depth < 0 {
				return nil, g.Error("unbalanced parenthesis")
			}
			b.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n') && depth == 0:
			flush()
		default:
			b.WriteRune(r)
		}
	}

	if depth != 0 {
		return nil, g.Error("unbalanced parenthesis")
	}
	if quote != 0 {
		return nil, g.Error("unterminated quote")
	}
	flush()

	return tokens, nil
}

// splitModifier separates a modifier token into its name and optional
// parenthesized payload. e.g. "index(name=foo)" => ("index", "name=foo", true).
func splitModifier(tok string) (name, payload string, hasPayload bool) {
	idx := strings.Index(tok, "(")
	if idx < 0 {
		return tok, "", false
	}
	name = tok[:idx]
	// payload is everything between the first ( and the matching trailing )
	inner := tok[idx+1:]
	inner = strings.TrimSuffix(inner, ")")
	return name, inner, true
}

// parseStringPayload parses a single string argument that may be wrapped in
// single quotes, double quotes or backticks. Doubled quotes ('' or "") unescape
// to a single quote, matching SQL string-literal conventions.
func parseStringPayload(payload string) (string, error) {
	s := strings.TrimSpace(payload)
	if s == "" {
		return "", nil
	}
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"' || s[0] == '`') {
		q := s[0]
		if s[len(s)-1] != q {
			return "", g.Error("unterminated quoted value: %s", payload)
		}
		inner := s[1 : len(s)-1]
		switch q {
		case '\'':
			inner = strings.ReplaceAll(inner, "''", "'")
		case '"':
			inner = strings.ReplaceAll(inner, `""`, `"`)
		}
		return inner, nil
	}
	return s, nil
}

// parseIndexModifier parses the kwargs of an index(...)/unique_index(...) modifier.
func parseIndexModifier(payload string, hasPayload, unique bool) (def IndexDef, err error) {
	def.Unique = unique

	if !hasPayload || strings.TrimSpace(payload) == "" {
		return def, nil // bare `index` / `index()`
	}

	kwargs, err := parseKwargs(payload)
	if err != nil {
		return def, err
	}

	for k, v := range kwargs {
		switch strings.ToLower(k) {
		case "name":
			def.Name = v
		case "priority":
			def.Priority = cast.ToInt(v)
		case "sort":
			sort := strings.ToLower(v)
			if sort != "asc" && sort != "desc" {
				return def, g.Error("invalid sort value %q (expected asc/desc)", v)
			}
			// applied to the column placeholder below
			def.Columns = []IndexColumn{{Sort: sort}}
		case "where":
			def.Where = v
		case "type", "using":
			def.Type = strings.ToLower(v)
		case "include":
			for _, part := range strings.Split(v, ",") {
				if p := strings.TrimSpace(part); p != "" {
					def.Include = append(def.Include, p)
				}
			}
		case "unique":
			def.Unique = cast.ToBool(v)
		default:
			return def, g.Error("unknown index kwarg %q", k)
		}
	}

	return def, nil
}

// parseKwargs parses a comma-separated list of key=value pairs where values may
// be single-quoted or backtick-quoted (commas inside quotes are preserved).
func parseKwargs(payload string) (kwargs map[string]string, err error) {
	kwargs = map[string]string{}

	parts, err := splitTopLevel(payload, ',')
	if err != nil {
		return nil, err
	}

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			return nil, g.Error("invalid index kwarg %q (expected key=value)", part)
		}
		key := strings.TrimSpace(part[:eq])
		val, err := parseStringPayload(strings.TrimSpace(part[eq+1:]))
		if err != nil {
			return nil, err
		}
		kwargs[key] = val
	}

	return kwargs, nil
}

// splitTopLevel splits on sep while respecting balanced parens and quoted runs.
func splitTopLevel(s string, sep rune) (parts []string, err error) {
	var b strings.Builder
	depth := 0
	var quote rune

	for _, r := range s {
		switch {
		case quote != 0:
			b.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"' || r == '`':
			quote = r
			b.WriteRune(r)
		case r == '(':
			depth++
			b.WriteRune(r)
		case r == ')':
			depth--
			b.WriteRune(r)
		case r == sep && depth == 0:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, g.Error("unterminated quote")
	}
	parts = append(parts, b.String())

	return parts, nil
}

// CollectInlineIndexes gathers the inline index definitions declared across all
// columns via the columns: modifier DSL and merges them into final []IndexDef.
//
// Columns sharing the same index Name form a composite index, ordered by
// Priority. Per-column attributes are Priority and Sort; shared attributes
// (Where, Type, Unique, Include) must be identical (or omitted) across every
// column declaring the same named index, else an error is returned. Anonymous
// indexes (no Name) are auto-named idx_<table>_<col> and never merged.
func (cols Columns) CollectInlineIndexes(tableName string, maxLength int) (defs []IndexDef, err error) {
	// in-progress named composite: the def plus the per-column priorities
	type composite struct {
		def        *IndexDef
		priorities []int
	}

	order := []string{} // preserve declaration order of named composites
	named := map[string]*composite{}

	for _, col := range cols {
		for _, raw := range col.GetIndexDefs() {
			colName := col.Name
			sort := ""
			if len(raw.Columns) == 1 {
				sort = raw.Columns[0].Sort
			}

			if raw.Name == "" {
				// anonymous single-column index, auto-named, never merged
				defs = append(defs, IndexDef{
					Name:    MakeIndexName(tableName, []string{colName}, maxLength),
					Columns: []IndexColumn{{Name: colName, Sort: sort}},
					Unique:  raw.Unique,
					Where:   raw.Where,
					Type:    raw.Type,
					Include: raw.Include,
				})
				continue
			}

			// named index: merge by name
			existing, ok := named[raw.Name]
			if !ok {
				named[raw.Name] = &composite{
					def: &IndexDef{
						Name:    raw.Name,
						Columns: []IndexColumn{{Name: colName, Sort: sort}},
						Unique:  raw.Unique,
						Where:   raw.Where,
						Type:    raw.Type,
						Include: raw.Include,
					},
					priorities: []int{raw.Priority},
				}
				order = append(order, raw.Name)
				continue
			}

			// consistency checks for shared attributes
			if err = mergeSharedAttr("where", raw.Name, &existing.def.Where, raw.Where); err != nil {
				return nil, err
			}
			if err = mergeSharedAttr("type", raw.Name, &existing.def.Type, raw.Type); err != nil {
				return nil, err
			}
			exInc := strings.Join(existing.def.Include, ",")
			if err = mergeSharedAttr("include", raw.Name, &exInc, strings.Join(raw.Include, ",")); err != nil {
				return nil, err
			}
			if exInc != "" {
				existing.def.Include = strings.Split(exInc, ",")
			}
			// unique: if any column declares unique_index, the whole index is unique
			if raw.Unique {
				existing.def.Unique = true
			}

			existing.def.Columns = append(existing.def.Columns, IndexColumn{Name: colName, Sort: sort})
			existing.priorities = append(existing.priorities, raw.Priority)
		}
	}

	// finalize named composites: validate priorities, order columns by priority
	for _, name := range order {
		c := named[name]

		seen := map[int]bool{}
		anySet := false
		for _, p := range c.priorities {
			if p != 0 {
				anySet = true
				if seen[p] {
					return nil, g.Error("index %q: duplicate priority %d", name, p)
				}
				seen[p] = true
			}
		}

		if anySet && len(c.def.Columns) > 1 {
			// stable insertion sort columns by priority (dependency-free)
			for i := 1; i < len(c.def.Columns); i++ {
				for j := i; j > 0 && c.priorities[j-1] > c.priorities[j]; j-- {
					c.priorities[j-1], c.priorities[j] = c.priorities[j], c.priorities[j-1]
					c.def.Columns[j-1], c.def.Columns[j] = c.def.Columns[j], c.def.Columns[j-1]
				}
			}
		}

		defs = append(defs, *c.def)
	}

	return defs, nil
}

// mergeSharedAttr enforces that a shared index attribute is identical (or
// omitted) across all columns declaring the same named index.
func mergeSharedAttr(attr, idxName string, existing *string, incoming string) error {
	if incoming == "" {
		return nil
	}
	if *existing == "" {
		*existing = incoming
		return nil
	}
	if *existing != incoming {
		return g.Error("index %q: conflicting %s values (%q vs %q); shared index attributes must match across columns", idxName, attr, *existing, incoming)
	}
	return nil
}

// MakeIndexName builds a deterministic index name from a table name and the
// index's column/expression names. When the result exceeds maxLength (the
// dialect's identifier limit), it truncates and appends a short stable hash
// suffix so distinct auto-named indexes never collide on a shared prefix.
func MakeIndexName(tableName string, parts []string, maxLength int) string {
	clean := func(s string) string {
		// keep identifier-friendly characters for the readable portion
		s = strings.ToLower(s)
		s = replacePattern.ReplaceAllString(s, "_")
		return strings.Trim(s, "_")
	}

	nameParts := []string{"idx", clean(tableName)}
	for _, p := range parts {
		if c := clean(p); c != "" {
			nameParts = append(nameParts, c)
		}
	}
	full := strings.Join(nameParts, "_")

	if maxLength <= 0 || len(full) <= maxLength {
		return full
	}

	// deterministic 6-char hash of the full untruncated name
	sum := md5.Sum([]byte(full))
	hash := fmt.Sprintf("%x", sum)[:6]

	keep := maxLength - 7 // "_" + 6-char hash
	if keep < 1 {
		keep = 1
	}
	if keep > len(full) {
		keep = len(full)
	}
	return full[:keep] + "_" + hash
}
