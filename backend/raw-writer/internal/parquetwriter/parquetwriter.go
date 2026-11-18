// Package parquetwriter encodes raw rows as a Snappy Parquet file with the fixed raw
// schema every downstream reader (ClickHouse, dbt, BigQuery) relies on.
package parquetwriter

import (
	"bytes"
	"encoding/base64"
	"errors"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/metadata"
	"github.com/apache/arrow-go/v18/parquet/schema"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/transformer"
)

// StringColumns lists the nullable UTF-8 columns in schema order; nats_sequence (int64)
// follows them.
var StringColumns = []string{
	"event_id", "app_id", "environment", "event_name", "event_timestamp", "received_at",
	"user_id", "user_pseudo_id", "session_id", "platform", "app_version", "os_version",
	"device_model", "locale", "timezone", "event_params_json", "user_properties_json",
	"traffic_source_json", "geo_json", "consent_json", "ingest_request_id",
	"ingest_user_agent", "ingest_ip_hash", "nats_stream",
}

// RawSchema is the Arrow schema of a raw file: every column nullable, strings as UTF-8
// and nats_sequence as int64.
var RawSchema = func() *arrow.Schema {
	fields := make([]arrow.Field, 0, len(StringColumns)+1)
	for _, name := range StringColumns {
		fields = append(fields, arrow.Field{Name: name, Type: arrow.BinaryTypes.String, Nullable: true})
	}
	fields = append(fields, arrow.Field{Name: "nats_sequence", Type: arrow.PrimitiveTypes.Int64, Nullable: true})
	return arrow.NewSchema(fields, nil)
}()

// maxRowGroupLength matches pyarrow's default row group size.
const maxRowGroupLength = 1024 * 1024

// parquetSchema mirrors what pyarrow writes for RawSchema: optional BYTE_ARRAY columns
// annotated as String, and a plain optional INT64 without a logical type annotation.
var parquetSchema = func() *schema.GroupNode {
	nodes := make(schema.FieldList, 0, len(StringColumns)+1)
	for _, name := range StringColumns {
		node, err := schema.NewPrimitiveNodeLogical(name, parquet.Repetitions.Optional,
			schema.StringLogicalType{}, parquet.Types.ByteArray, -1, -1)
		if err != nil {
			panic(err)
		}
		nodes = append(nodes, node)
	}
	seq, err := schema.NewPrimitiveNode("nats_sequence", parquet.Repetitions.Optional, parquet.Types.Int64, -1, -1)
	if err != nil {
		panic(err)
	}
	root, err := schema.NewGroupNode("schema", parquet.Repetitions.Required, append(nodes, seq), -1)
	if err != nil {
		panic(err)
	}
	return root
}()

// arrowSchemaMetadata is the base64 IPC schema pyarrow stores under "ARROW:schema".
var arrowSchemaMetadata = func() string {
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(RawSchema))
	if err := w.Close(); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}()

var writerProps = parquet.NewWriterProperties(
	parquet.WithCompression(compress.Codecs.Snappy),
	parquet.WithDictionaryDefault(true),
	parquet.WithStats(true),
	parquet.WithVersion(parquet.V2_LATEST),
	parquet.WithDataPageVersion(parquet.DataPageV1),
)

// WriteRows encodes rows, in order, into one Parquet file.
func WriteRows(rows []*transformer.Row) ([]byte, error) {
	if len(rows) == 0 {
		return nil, errors.New("rows cannot be empty")
	}
	kv := metadata.NewKeyValueMetadata()
	if err := kv.Append("ARROW:schema", arrowSchemaMetadata); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	writer := file.NewParquetWriter(&buf, parquetSchema, file.WithWriterProps(writerProps), file.WithWriteMetadata(kv))
	for start := 0; start < len(rows); start += maxRowGroupLength {
		if err := writeRowGroup(writer, rows[start:min(start+maxRowGroupLength, len(rows))]); err != nil {
			_ = writer.Close()
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeRowGroup(writer *file.Writer, rows []*transformer.Row) error {
	values := make([][]parquet.ByteArray, len(StringColumns))
	defLevels := make([][]int16, len(StringColumns))
	for c := range defLevels {
		defLevels[c] = make([]int16, len(rows))
	}
	sequences := make([]int64, len(rows))
	for i, row := range rows {
		for c, field := range row.StringFields() {
			if *field != nil {
				values[c] = append(values[c], parquet.ByteArray(**field))
				defLevels[c][i] = 1
			}
		}
		sequences[i] = row.NATSSequence
	}

	rg := writer.AppendRowGroup()
	for c := range StringColumns {
		cw, err := rg.NextColumn()
		if err != nil {
			return err
		}
		if _, err := cw.(*file.ByteArrayColumnChunkWriter).WriteBatch(values[c], defLevels[c], nil); err != nil {
			return err
		}
		if err := cw.Close(); err != nil {
			return err
		}
	}
	allDefined := make([]int16, len(rows))
	for i := range allDefined {
		allDefined[i] = 1
	}
	cw, err := rg.NextColumn()
	if err != nil {
		return err
	}
	if _, err := cw.(*file.Int64ColumnChunkWriter).WriteBatch(sequences, allDefined, nil); err != nil {
		return err
	}
	if err := cw.Close(); err != nil {
		return err
	}
	return rg.Close()
}
