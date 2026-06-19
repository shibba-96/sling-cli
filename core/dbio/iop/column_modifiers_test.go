package iop

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseCol is a helper that runs the same pipeline as ColumnsPrepared:
// SetConstraint -> ParseModifiers -> SetLengthPrecisionScale
func parseCol(t *testing.T, name, typeStr string) (Column, error) {
	t.Helper()
	col := Column{Name: name, Type: ColumnType(typeStr)}
	col.SetConstraint()
	err := col.ParseModifiers()
	if err != nil {
		return col, err
	}
	col.SetLengthPrecisionScale()
	return col, nil
}

func TestParseModifiersSingle(t *testing.T) {
	// not_null
	col, err := parseCol(t, "id", "bigint not_null")
	require.NoError(t, err)
	assert.Equal(t, BigIntType, col.Type)
	assert.False(t, col.IsNullable())
	assert.True(t, col.IsDDLExplicit())

	// nullable
	col, err = parseCol(t, "notes", "text nullable")
	require.NoError(t, err)
	assert.True(t, col.IsNullable())

	// primary_key
	col, err = parseCol(t, "id", "bigint primary_key")
	require.NoError(t, err)
	assert.True(t, col.IsPrimaryKey())

	// unique
	col, err = parseCol(t, "email", "text unique")
	require.NoError(t, err)
	assert.True(t, col.HasUniqueConstraint())
}

func TestParseModifiersDescription(t *testing.T) {
	col, err := parseCol(t, "desc_col", "text description('hello world')")
	require.NoError(t, err)
	assert.Equal(t, "hello world", col.Description)
	assert.Equal(t, TextType, col.Type)

	// nested parens in payload
	col, err = parseCol(t, "desc_col", "text description('see (note)')")
	require.NoError(t, err)
	assert.Equal(t, "see (note)", col.Description)

	// doubled single-quote escaping
	col, err = parseCol(t, "desc_col", "text description('it''s fine')")
	require.NoError(t, err)
	assert.Equal(t, "it's fine", col.Description)

	// double-quote inside single-quoted value
	col, err = parseCol(t, "desc_col", `text description('she said "hi"')`)
	require.NoError(t, err)
	assert.Equal(t, `she said "hi"`, col.Description)

	// double-quoted payload (single quote inside is a literal)
	col, err = parseCol(t, "desc_col", `text description("hello world won't do")`)
	require.NoError(t, err)
	assert.Equal(t, "hello world won't do", col.Description)

	// doubled double-quote escaping inside double-quoted value
	col, err = parseCol(t, "desc_col", `text description("she said ""hi""")`)
	require.NoError(t, err)
	assert.Equal(t, `she said "hi"`, col.Description)
}

func TestParseModifiersOrderIndependence(t *testing.T) {
	a, err := parseCol(t, "id", "bigint not_null primary_key")
	require.NoError(t, err)
	b, err := parseCol(t, "id", "bigint primary_key not_null")
	require.NoError(t, err)

	assert.False(t, a.IsNullable())
	assert.True(t, a.IsPrimaryKey())
	assert.Equal(t, a.IsNullable(), b.IsNullable())
	assert.Equal(t, a.IsPrimaryKey(), b.IsPrimaryKey())
}

func TestParseModifiersCaseInsensitive(t *testing.T) {
	col, err := parseCol(t, "id", "bigint NOT_NULL Primary_Key")
	require.NoError(t, err)
	assert.False(t, col.IsNullable())
	assert.True(t, col.IsPrimaryKey())

	col, err = parseCol(t, "desc_col", "text Description('hi')")
	require.NoError(t, err)
	assert.Equal(t, "hi", col.Description)
}

func TestParseModifiersConflict(t *testing.T) {
	_, err := parseCol(t, "id", "bigint not_null nullable")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicting")
}

func TestParseModifiersWithRuntimeConstraint(t *testing.T) {
	col := Column{Name: "val", Type: ColumnType("int not_null | value > 0")}
	col.SetConstraint()
	// after SetConstraint, the type slot retains the modifiers, the | is split off
	require.NoError(t, col.ParseModifiers())
	col.SetLengthPrecisionScale()

	assert.Equal(t, ColumnType("int"), col.Type) // normalization to "integer" happens later in NewColumns
	assert.False(t, col.IsNullable())
	// runtime constraint expression preserved
	if col.Constraint != nil {
		assert.Equal(t, "value > 0", col.Constraint.Expression)
	}
}

func TestParseModifiersLengthPrecision(t *testing.T) {
	col, err := parseCol(t, "amount", "decimal(10,2) not_null")
	require.NoError(t, err)
	assert.Equal(t, DecimalType, col.Type)
	assert.Equal(t, 10, col.DbPrecision)
	assert.Equal(t, 2, col.DbScale)
	assert.False(t, col.IsNullable())

	col, err = parseCol(t, "name", "string(100) unique")
	require.NoError(t, err)
	assert.Equal(t, StringType, col.Type)
	assert.Equal(t, 100, col.DbPrecision)
	assert.True(t, col.HasUniqueConstraint())
}

func TestParseModifiersIndex(t *testing.T) {
	// bare index
	col, err := parseCol(t, "search_col", "text index")
	require.NoError(t, err)
	defs := col.GetIndexDefs()
	require.Len(t, defs, 1)
	assert.False(t, defs[0].Unique)

	// index()
	col, err = parseCol(t, "search_col", "text index()")
	require.NoError(t, err)
	require.Len(t, col.GetIndexDefs(), 1)

	// index(name=foo)
	col, err = parseCol(t, "c", "text index(name=foo)")
	require.NoError(t, err)
	defs = col.GetIndexDefs()
	require.Len(t, defs, 1)
	assert.Equal(t, "foo", defs[0].Name)

	// index with priority/sort
	col, err = parseCol(t, "ts", "timestamptz index(name=foo, priority=3, sort=desc)")
	require.NoError(t, err)
	defs = col.GetIndexDefs()
	require.Len(t, defs, 1)
	assert.Equal(t, "foo", defs[0].Name)
	assert.Equal(t, 3, defs[0].Priority)
	require.Len(t, defs[0].Columns, 1)
	assert.Equal(t, "desc", defs[0].Columns[0].Sort)

	// index with where
	col, err = parseCol(t, "org_id", "uuid index(name=foo, where='deleted_at IS NULL')")
	require.NoError(t, err)
	defs = col.GetIndexDefs()
	require.Len(t, defs, 1)
	assert.Equal(t, "deleted_at IS NULL", defs[0].Where)

	// index with a double-quoted where containing single-quoted SQL literals
	col, err = parseCol(t, "created_at", `timestamp index(name=idx_x, where="created_at = '2001-01-01'")`)
	require.NoError(t, err)
	defs = col.GetIndexDefs()
	require.Len(t, defs, 1)
	assert.Equal(t, "idx_x", defs[0].Name)
	assert.Equal(t, "created_at = '2001-01-01'", defs[0].Where)

	// index with type
	col, err = parseCol(t, "doc", "json index(name=foo, type=gin)")
	require.NoError(t, err)
	defs = col.GetIndexDefs()
	require.Len(t, defs, 1)
	assert.Equal(t, "gin", defs[0].Type)

	// unique_index
	col, err = parseCol(t, "email", "text unique_index(name=bar)")
	require.NoError(t, err)
	defs = col.GetIndexDefs()
	require.Len(t, defs, 1)
	assert.True(t, defs[0].Unique)
	assert.Equal(t, "bar", defs[0].Name)
}

func TestParseModifiersIndexBacktick(t *testing.T) {
	col, err := parseCol(t, "org_id", "uuid index(name=foo, where=`x is null`)")
	require.NoError(t, err)
	defs := col.GetIndexDefs()
	require.Len(t, defs, 1)
	assert.Equal(t, "x is null", defs[0].Where)
}

func TestParseModifiersReserved(t *testing.T) {
	reserved := []string{
		"int default(now())",
		"int check(value >= 0)",
		"int auto_increment",
		"int identity",
		"int identity(1, 1)",
	}
	for _, ts := range reserved {
		_, err := parseCol(t, "c", ts)
		require.Error(t, err, "expected error for %q", ts)
		assert.Contains(t, err.Error(), "not yet supported", "for %q", ts)
	}
}

func TestParseModifiersNoModifiers(t *testing.T) {
	// plain types must be unaffected
	col, err := parseCol(t, "c", "text")
	require.NoError(t, err)
	assert.Equal(t, TextType, col.Type)
	assert.True(t, col.IsNullable())
	assert.False(t, col.IsDDLExplicit())
	assert.Nil(t, col.GetIndexDefs())

	col, err = parseCol(t, "c", "decimal(10,2)")
	require.NoError(t, err)
	assert.Equal(t, DecimalType, col.Type)
	assert.Equal(t, 10, col.DbPrecision)
	assert.Equal(t, 2, col.DbScale)
}

func TestMakeIndexName(t *testing.T) {
	// short name passes through
	assert.Equal(t, "idx_bar_foo", MakeIndexName("bar", []string{"foo"}, 63))

	// composite
	assert.Equal(t, "idx_t_a_b", MakeIndexName("t", []string{"a", "b"}, 63))

	// truncation with deterministic hash suffix
	long := MakeIndexName("verylongtablename", []string{"verylongcolumnnameone", "verylongcolumnnametwo"}, 30)
	assert.LessOrEqual(t, len(long), 30)
	long2 := MakeIndexName("verylongtablename", []string{"verylongcolumnnameone", "verylongcolumnnametwo"}, 30)
	assert.Equal(t, long, long2, "must be deterministic")

	// distinct inputs that share a truncation prefix get distinct hashes
	a := MakeIndexName("t", []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_x"}, 20)
	b := MakeIndexName("t", []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_y"}, 20)
	assert.NotEqual(t, a, b)
}

func mkCol(t *testing.T, name, typeStr string) Column {
	t.Helper()
	col, err := parseCol(t, name, typeStr)
	require.NoError(t, err)
	return col
}

func TestCollectInlineIndexesComposite(t *testing.T) {
	cols := Columns{
		mkCol(t, "timestamp", "timestamptz index(name=idx_hb, priority=3, sort=desc)"),
		mkCol(t, "project_id", "uuid index(name=idx_hb, priority=1)"),
		mkCol(t, "agent_id", "uuid index(name=idx_hb, priority=2)"),
	}
	defs, err := cols.CollectInlineIndexes("heartbeats", 63)
	require.NoError(t, err)
	require.Len(t, defs, 1)

	d := defs[0]
	assert.Equal(t, "idx_hb", d.Name)
	require.Len(t, d.Columns, 3)
	// ordered by priority
	assert.Equal(t, "project_id", d.Columns[0].Name)
	assert.Equal(t, "agent_id", d.Columns[1].Name)
	assert.Equal(t, "timestamp", d.Columns[2].Name)
	assert.Equal(t, "desc", d.Columns[2].Sort)
}

func TestCollectInlineIndexesConsistencyViolation(t *testing.T) {
	cols := Columns{
		mkCol(t, "a", "uuid index(name=idx_x, where='deleted_at IS NULL')"),
		mkCol(t, "b", "uuid index(name=idx_x, where='created_at IS NULL')"),
	}
	_, err := cols.CollectInlineIndexes("t", 63)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicting where")
}

func TestCollectInlineIndexesDuplicatePriority(t *testing.T) {
	cols := Columns{
		mkCol(t, "a", "uuid index(name=idx_x, priority=1)"),
		mkCol(t, "b", "uuid index(name=idx_x, priority=1)"),
	}
	_, err := cols.CollectInlineIndexes("t", 63)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate priority")
}

func TestCollectInlineIndexesAutoName(t *testing.T) {
	cols := Columns{
		mkCol(t, "search_col", "text index"),
	}
	defs, err := cols.CollectInlineIndexes("bar", 63)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "idx_bar_search_col", defs[0].Name)
	require.Len(t, defs[0].Columns, 1)
	assert.Equal(t, "search_col", defs[0].Columns[0].Name)
}

func TestCollectInlineIndexesUniqueShared(t *testing.T) {
	cols := Columns{
		mkCol(t, "email", "text unique_index(name=idx_email)"),
	}
	defs, err := cols.CollectInlineIndexes("users", 63)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.True(t, defs[0].Unique)
}

func TestTokenizeModifiers(t *testing.T) {
	toks, err := tokenizeModifiers("bigint not_null primary_key")
	require.NoError(t, err)
	assert.Equal(t, []string{"bigint", "not_null", "primary_key"}, toks)

	// parens keep payload together
	toks, err = tokenizeModifiers("text index(name=foo, priority=1)")
	require.NoError(t, err)
	assert.Equal(t, []string{"text", "index(name=foo, priority=1)"}, toks)

	// quotes preserve spaces
	toks, err = tokenizeModifiers("uuid index(where='a b c')")
	require.NoError(t, err)
	assert.Equal(t, []string{"uuid", "index(where='a b c')"}, toks)

	// unbalanced
	_, err = tokenizeModifiers("text index(name=foo")
	require.Error(t, err)
}
