package feishu

import "testing"

// TestCheckOutputFormatValid accepts the well-formed envelopes internal/feishu
// depends on: a success envelope carrying data, and an API-error envelope carrying
// error (the format is intact even when the call itself failed).
func TestCheckOutputFormatValid(t *testing.T) {
	valid := []string{
		`{"ok":true,"data":{"user_info":{"name":"bot"}}}`,
		`{"ok":true,"data":{}}`,
		`{"ok":false,"error":{"type":"auth","subtype":"expired","code":99991663,"message":"token expired"}}`,
	}
	for _, v := range valid {
		if err := CheckOutputFormat([]byte(v)); err != nil {
			t.Errorf("CheckOutputFormat(%s) = %v, want nil", v, err)
		}
	}
}

// TestCheckOutputFormatTampered is the key drift-detection case: doctor must
// alarm when lark-cli's output envelope drifts. Each tampered payload must be
// rejected with a non-nil error.
func TestCheckOutputFormatTampered(t *testing.T) {
	tampered := []struct {
		name string
		body string
	}{
		{"not json", `this is not json at all`},
		{"json array not object", `[{"ok":true}]`},
		{"renamed ok field", `{"success":true,"data":{}}`},
		{"ok not boolean", `{"ok":"yes","data":{}}`},
		{"success without data", `{"ok":true,"result":{"x":1}}`},
		{"failure without error", `{"ok":false,"message":"boom"}`},
		{"empty object", `{}`},
	}
	for _, tc := range tampered {
		if err := CheckOutputFormat([]byte(tc.body)); err == nil {
			t.Errorf("CheckOutputFormat(%s) = nil, want an error (format drift must be flagged)", tc.name)
		}
	}
}
