package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// bitableField is one field (column) of a bitable table.
type bitableField struct {
	FieldName string `json:"field_name"`
	UIType    string `json:"ui_type"`
	IsPrimary bool   `json:"is_primary"`
}

// bitableFieldsData is the data payload of the fields endpoint.
type bitableFieldsData struct {
	Items     []bitableField `json:"items"`
	HasMore   bool           `json:"has_more"`
	PageToken string         `json:"page_token"`
}

// bitableRecord is one record (row); Fields maps field_name -> raw value.
type bitableRecord struct {
	Fields map[string]json.RawMessage `json:"fields"`
}

// bitableRecordsData is the data payload of the records endpoint.
type bitableRecordsData struct {
	Items     []bitableRecord `json:"items"`
	HasMore   bool            `json:"has_more"`
	PageToken string          `json:"page_token"`
	Total     int             `json:"total"`
}

// bitableData is the fetched shape of a bitable table: its columns, its rows
// (up to the fetched cap), and the true total row count.
type bitableData struct {
	fields  []bitableField
	records []bitableRecord
	total   int
}

// bitableRecordPageSize is the per-request record page size.
const bitableRecordPageSize = 200

// fetchBitable reads a bitable table's fields and records via the bitable API,
// paging records to maxRecords+1 (one past the threshold decides the oversize
// branch without draining a huge table). Paced by the fetch limiter, retried
// on rate-limit errors.
func (a *Adapter) fetchBitable(ctx context.Context, appToken, tableID string, maxRecords int) (bitableData, error) {
	fields, err := a.bitableFields(ctx, appToken, tableID)
	if err != nil {
		return bitableData{}, err
	}
	records, total, err := a.bitableRecords(ctx, appToken, tableID, maxRecords)
	if err != nil {
		return bitableData{}, err
	}
	return bitableData{fields: fields, records: records, total: total}, nil
}

func (a *Adapter) bitableFields(ctx context.Context, appToken, tableID string) ([]bitableField, error) {
	var out []bitableField
	pageToken := ""
	for {
		path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/fields", appToken, tableID)
		params := map[string]any{"page_size": 100}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
		data, err := a.bitableGet(ctx, path, params)
		if err != nil {
			return nil, err
		}
		var fd bitableFieldsData
		if err := json.Unmarshal(data, &fd); err != nil {
			return nil, fmt.Errorf("decode bitable fields: %w", err)
		}
		out = append(out, fd.Items...)
		if !fd.HasMore || fd.PageToken == "" {
			break
		}
		pageToken = fd.PageToken
	}
	return out, nil
}

func (a *Adapter) bitableRecords(ctx context.Context, appToken, tableID string, maxRecords int) ([]bitableRecord, int, error) {
	var out []bitableRecord
	total := 0
	pageToken := ""
	for {
		// records/search (POST, pagination via query params, empty filter body)
		// replaces the plain records list, which Feishu has deprecated. Cell
		// values come back in the segment-array shape ([{text,type}...]) instead
		// of plain strings; stringifyCell flattens both identically (verified
		// against live data).
		path := fmt.Sprintf("/open-apis/bitable/v1/apps/%s/tables/%s/records/search", appToken, tableID)
		params := map[string]any{"page_size": bitableRecordPageSize}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
		data, err := a.bitableCall(ctx, "POST", path, params, "{}")
		if err != nil {
			return nil, 0, err
		}
		var rd bitableRecordsData
		if err := json.Unmarshal(data, &rd); err != nil {
			return nil, 0, fmt.Errorf("decode bitable records: %w", err)
		}
		total = rd.Total
		out = append(out, rd.Items...)
		// Stop once we have enough rows to decide the oversize branch, or the
		// pages run out.
		if len(out) > maxRecords || !rd.HasMore || rd.PageToken == "" {
			break
		}
		pageToken = rd.PageToken
	}
	return out, total, nil
}

// bitableGet performs a rate-limited, backoff-retried GET against the bitable
// API and returns the unwrapped data payload.
func (a *Adapter) bitableGet(ctx context.Context, path string, params map[string]any) (json.RawMessage, error) {
	return a.bitableCall(ctx, "GET", path, params, "")
}

// bitableCall performs a rate-limited, backoff-retried bitable API call and
// returns the unwrapped data payload. params become URL query parameters; a
// non-empty body is sent as the JSON request body (POST search).
func (a *Adapter) bitableCall(ctx context.Context, method, path string, params map[string]any, body string) (json.RawMessage, error) {
	var data json.RawMessage
	err := a.backoff.Retry(ctx, isRateLimited, func() (time.Duration, error) {
		if err := a.limiter.Wait(ctx); err != nil {
			return 0, err
		}
		paramJSON, _ := json.Marshal(params)
		args := []string{"api", method, path, "--params", string(paramJSON)}
		if body != "" {
			args = append(args, "--data", body)
		}
		out, err := a.run.Run(ctx, args...)
		if err != nil {
			return retryAfter(err), err
		}
		d, err := unwrap(out)
		if err != nil {
			return retryAfter(err), err
		}
		data = d
		return 0, nil
	})
	return data, err
}

// renderBitableTable renders the fetched bitable as a GitHub-flavoured markdown
// table: fields define the columns, records fill the rows. Empty cells render as
// a single space so the pipe columns stay aligned.
func renderBitableTable(bd bitableData) string {
	var b strings.Builder
	names := make([]string, len(bd.fields))
	for i, f := range bd.fields {
		names[i] = f.FieldName
	}
	b.WriteString("| " + strings.Join(escapeCells(names), " | ") + " |\n")
	seps := make([]string, len(names))
	for i := range seps {
		seps[i] = "---"
	}
	b.WriteString("| " + strings.Join(seps, " | ") + " |\n")
	for _, rec := range bd.records {
		cells := make([]string, len(bd.fields))
		for i, f := range bd.fields {
			cells[i] = stringifyCell(rec.Fields[f.FieldName])
		}
		b.WriteString("| " + strings.Join(escapeCells(cells), " | ") + " |\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderBitableSchema renders the schema-only degradation for an oversize table:
// the column list and the true row count.
func renderBitableSchema(bd bitableData) string {
	cols := make([]string, len(bd.fields))
	for i, f := range bd.fields {
		t := f.UIType
		if t == "" {
			t = "?"
		}
		cols[i] = fmt.Sprintf("%s (%s)", f.FieldName, t)
	}
	return fmt.Sprintf("Bitable (%d rows, over the inline threshold; showing structure only):\n\n- Columns: %s",
		bd.total, strings.Join(cols, ", "))
}

// escapeCells escapes pipe and newline characters in each cell so they do not
// break the markdown table row.
func escapeCells(cells []string) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		c = strings.ReplaceAll(c, "\\", "\\\\")
		c = strings.ReplaceAll(c, "|", "\\|")
		c = strings.ReplaceAll(c, "\n", " ")
		c = strings.ReplaceAll(c, "\r", " ")
		if c == "" {
			c = " "
		}
		out[i] = c
	}
	return out
}

// stringifyCell flattens a bitable cell value into display text. Bitable cell
// values are polymorphic: a plain string/number/bool, an array of rich-text
// segments ([{text,type}...]), an array of link/attachment/user objects, or a
// nested object. Unknown shapes fall back to compact JSON so nothing is lost.
func stringifyCell(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return flatten(v)
}

func flatten(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// Render integers without a trailing .0.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		var parts []string
		for _, e := range t {
			if s := flatten(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		// Prefer the common human-readable keys used across bitable value shapes.
		for _, key := range []string{"text", "name", "en_name", "link", "url", "full_address"} {
			if s, ok := t[key].(string); ok && s != "" {
				return s
			}
		}
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", t)
	}
}
