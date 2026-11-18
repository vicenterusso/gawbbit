package parquetwriter

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/transformer"
)

func strPtr(s string) *string { return &s }

func baseRow() *transformer.Row {
	r := &transformer.Row{NATSSequence: 123}
	values := []string{
		"evt-1", "com.example.app", "prod", "opened", "2026-04-13T10:00:00Z", "2026-04-13T10:00:01Z",
		"u1", "anon", "s1", "web", "1.0.0", "macOS", "Mac", "en-US", "UTC", "{}", "{}", "{}", "{}", "{}",
		"req-1", "ua", "hash", "EVENTS",
	}
	for i, f := range r.StringFields() {
		*f = strPtr(values[i])
	}
	return r
}

func openParquet(t *testing.T, data []byte) *file.Reader {
	t.Helper()
	reader, err := file.NewParquetReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func readTable(t *testing.T, data []byte) arrow.Table {
	t.Helper()
	fr, err := pqarrow.NewFileReader(openParquet(t, data), pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		t.Fatal(err)
	}
	table, err := fr.ReadTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return table
}

// tableRows converts a raw-schema table back to rows.
func tableRows(t *testing.T, table arrow.Table) []*transformer.Row {
	t.Helper()
	rows := make([]*transformer.Row, table.NumRows())
	for i := range rows {
		rows[i] = &transformer.Row{}
	}
	for c := 0; c < int(table.NumCols()); c++ {
		offset := 0
		for _, chunk := range table.Column(c).Data().Chunks() {
			for i := 0; i < chunk.Len(); i++ {
				row := rows[offset+i]
				switch arr := chunk.(type) {
				case *array.String:
					if !arr.IsNull(i) {
						*row.StringFields()[c] = strPtr(arr.Value(i))
					}
				case *array.Int64:
					row.NATSSequence = arr.Value(i)
				default:
					t.Fatalf("unexpected column type %T", chunk)
				}
			}
			offset += chunk.Len()
		}
	}
	return rows
}

// assertRawSchema compares names, types and nullability; readers add field_id metadata.
func assertRawSchema(t *testing.T, got *arrow.Schema) {
	t.Helper()
	if got.NumFields() != RawSchema.NumFields() {
		t.Fatalf("schema %v", got)
	}
	for i, want := range RawSchema.Fields() {
		f := got.Field(i)
		if f.Name != want.Name || !arrow.TypeEqual(f.Type, want.Type) || f.Nullable != want.Nullable {
			t.Errorf("field %d: %v, want %v", i, f, want)
		}
	}
}

func TestWriteRowsUsesExplicitSchema(t *testing.T) {
	data, err := WriteRows([]*transformer.Row{baseRow()})
	if err != nil {
		t.Fatal(err)
	}
	table := readTable(t, data)
	assertRawSchema(t, table.Schema())
	for _, name := range []string{"event_timestamp", "received_at", "geo_json"} {
		f, _ := table.Schema().FieldsByName(name)
		if len(f) != 1 || !arrow.TypeEqual(f[0].Type, arrow.BinaryTypes.String) {
			t.Errorf("%s type %v", name, f)
		}
	}
	if f, _ := table.Schema().FieldsByName("nats_sequence"); !arrow.TypeEqual(f[0].Type, arrow.PrimitiveTypes.Int64) {
		t.Errorf("nats_sequence type %v", f[0].Type)
	}
}

func TestNullHeavyRowsKeepSchema(t *testing.T) {
	row := baseRow()
	keep := map[int]bool{0: true, 1: true, 3: true, 4: true}
	for i, f := range row.StringFields() {
		if !keep[i] {
			*f = nil
		}
	}
	data, err := WriteRows([]*transformer.Row{row})
	if err != nil {
		t.Fatal(err)
	}
	table := readTable(t, data)
	assertRawSchema(t, table.Schema())
	got := tableRows(t, table)[0]
	if got.NATSSequence != 123 || got.Environment != nil || *got.EventID != "evt-1" {
		t.Fatalf("row %+v", got)
	}
}

func TestWriteRowsRejectsEmpty(t *testing.T) {
	if _, err := WriteRows(nil); err == nil {
		t.Fatal("expected error")
	}
}

// TestMatchesPyarrowFile compares against a file written by the original pyarrow writer
// from 400 golden rows: same Parquet schema (physical and logical types, repetition),
// Snappy on every column chunk, a single row group, stored Arrow schema and identical
// row contents in identical order.
func TestMatchesPyarrowFile(t *testing.T) {
	pyData, err := os.ReadFile("../../testdata/python_raw_sample.parquet")
	if err != nil {
		t.Fatal(err)
	}
	pyRows := tableRows(t, readTable(t, pyData))
	goData, err := WriteRows(pyRows)
	if err != nil {
		t.Fatal(err)
	}

	py, gr := openParquet(t, pyData), openParquet(t, goData)
	pySchema, goSchema := py.MetaData().Schema, gr.MetaData().Schema
	if pySchema.NumColumns() != goSchema.NumColumns() {
		t.Fatalf("columns %d, want %d", goSchema.NumColumns(), pySchema.NumColumns())
	}
	for i := 0; i < pySchema.NumColumns(); i++ {
		p, g := pySchema.Column(i), goSchema.Column(i)
		if p.Name() != g.Name() || p.PhysicalType() != g.PhysicalType() ||
			!p.LogicalType().Equals(g.LogicalType()) || p.ConvertedType() != g.ConvertedType() ||
			p.SchemaNode().RepetitionType() != g.SchemaNode().RepetitionType() {
			t.Errorf("column %d: go %s %s %s %s, python %s %s %s %s", i,
				g.Name(), g.PhysicalType(), g.LogicalType(), g.SchemaNode().RepetitionType(),
				p.Name(), p.PhysicalType(), p.LogicalType(), p.SchemaNode().RepetitionType())
		}
	}
	if gr.NumRowGroups() != 1 || py.NumRowGroups() != 1 {
		t.Errorf("row groups go=%d python=%d", gr.NumRowGroups(), py.NumRowGroups())
	}
	if gr.MetaData().Version() != parquet.V2_6 || py.MetaData().Version() != parquet.V2_6 {
		t.Errorf("format version go=%v python=%v", gr.MetaData().Version(), py.MetaData().Version())
	}
	rg := gr.MetaData().RowGroup(0)
	for i := 0; i < rg.NumColumns(); i++ {
		chunk, err := rg.ColumnChunk(i)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Compression() != compress.Codecs.Snappy {
			t.Errorf("column %d compression %v", i, chunk.Compression())
		}
	}
	for _, md := range []*file.Reader{py, gr} {
		if md.MetaData().KeyValueMetadata().FindValue("ARROW:schema") == nil {
			t.Error("ARROW:schema metadata missing")
		}
	}

	goRows := tableRows(t, readTable(t, goData))
	if len(goRows) != len(pyRows) || len(pyRows) != 400 {
		t.Fatalf("rows go=%d python=%d", len(goRows), len(pyRows))
	}
	for i := range pyRows {
		pf, gf := pyRows[i].StringFields(), goRows[i].StringFields()
		for c := range pf {
			if (*pf[c] == nil) != (*gf[c] == nil) || (*pf[c] != nil && **pf[c] != **gf[c]) {
				t.Fatalf("row %d column %s differs", i, StringColumns[c])
			}
		}
		if pyRows[i].NATSSequence != goRows[i].NATSSequence {
			t.Fatalf("row %d nats_sequence differs", i)
		}
	}
}
