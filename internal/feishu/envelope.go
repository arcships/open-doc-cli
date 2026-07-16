package feishu

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SmokeProbeArgs is the cheap, read-only lark-cli invocation `opendoc doctor` runs to
// obtain a live response envelope for the output-format smoke check (a
// countermeasure against lark-cli output-format drift across upgrades). It
// calls the authenticated-user endpoint: it is
// side-effect-free and always answers with the standard {ok, data, error}
// envelope — whether the call succeeds or fails on auth — which is exactly what
// the format check validates. It is a var so the check stays overridable/testable.
var SmokeProbeArgs = []string{"api", "GET", "/open-apis/authen/v1/user_info"}

// CheckOutputFormat validates that raw is a well-formed lark-cli response
// envelope — the {ok, data, error} shape internal/feishu depends on (the same
// contract unwrap consumes). It returns nil when the envelope is structurally
// intact, whether or not the wrapped API call itself succeeded, and a descriptive
// error when lark-cli's output has drifted from that shape. This is the alarm
// `opendoc doctor` raises when a lark-cli upgrade changes its output format.
func CheckOutputFormat(raw []byte) error {
	// Presence must be asserted explicitly: encoding/json silently zero-fills
	// missing keys, so a drifted payload that dropped "ok" would otherwise decode
	// into a valid-looking envelope. Decode into a raw key map first.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("lark-cli output is not a JSON object (envelope format drift): %w", err)
	}
	okRaw, hasOK := fields["ok"]
	if !hasOK {
		return fmt.Errorf(`lark-cli output missing "ok" field (envelope format drift); got keys %v`, sortedKeys(fields))
	}
	var ok bool
	if err := json.Unmarshal(okRaw, &ok); err != nil {
		return fmt.Errorf(`lark-cli "ok" field is not a boolean (envelope format drift): %s`, string(okRaw))
	}
	// On success the typed payload lives under "data"; on failure the structured
	// "error" must be present. Either proves the envelope wrapper is intact — and
	// mirrors exactly which field unwrap reads in each case.
	if ok {
		if _, has := fields["data"]; !has {
			return fmt.Errorf(`lark-cli success envelope missing "data" field (format drift); got keys %v`, sortedKeys(fields))
		}
	} else {
		if _, has := fields["error"]; !has {
			return fmt.Errorf(`lark-cli error envelope missing "error" field (format drift); got keys %v`, sortedKeys(fields))
		}
	}
	return nil
}

// sortedKeys returns the map's keys in deterministic order for stable error text.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// envelope is the common lark-cli JSON response wrapper: {ok, identity, data,
// error}. The typed payload lives under data.
type envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *apiError       `json:"error"`
}

// apiError is the structured error lark-cli reports when ok is false.
type apiError struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) String() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s (code %d): %s", e.Type, e.Subtype, e.Code, e.Message)
}

// Envelope is the parsed lark-cli response wrapper exposed for `opendoc doctor`'s
// probe classification (F2 auth, F3 scope, F5 mirror-scope): callers inspect OK
// and the structured Error to map auth vs permission failures to onboarding
// failure codes. It surfaces only the fields the probes
// need; the typed data payload stays internal.
type Envelope struct {
	// OK is the envelope's ok flag (true ⇒ the wrapped API call succeeded).
	OK bool
	// Error is the structured API error, non-nil only when OK is false.
	Error *APIError
}

// APIError is the structured lark-cli API error, exported for probe classification.
type APIError struct {
	Type    string
	Subtype string
	Code    int
	Message string
}

// String renders the error for probe detail lines.
func (e *APIError) String() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s (code %d): %s", e.Type, e.Subtype, e.Code, e.Message)
}

// IsAuthError reports whether e is a Feishu authentication/token failure — the
// 99991661–99991668 access-token error range (token missing, invalid, or
// expired). These map to F2-NOAUTH.
func (e *APIError) IsAuthError() bool {
	return e != nil && e.Code >= 99991661 && e.Code <= 99991668
}

// IsPermissionError reports whether e denotes an insufficient-scope / permission
// failure, mapping to an F3-SCOPE-<category> code. Feishu signals these with
// access-denied code 99991672 (and neighbours) or a permission/forbidden error
// type; opendoc also keyword-matches the message as a best-effort backstop, since
// lark-cli's scope error surface is not fully catalogued.
func (e *APIError) IsPermissionError() bool {
	if e == nil {
		return false
	}
	if e.Code == 99991672 || e.Code == 99991679 {
		return true
	}
	hay := strings.ToLower(e.Type + " " + e.Subtype + " " + e.Message)
	for _, kw := range []string{"permission", "forbidden", "access denied", "no access", "scope"} {
		if strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

// ParseEnvelope decodes a lark-cli response into an Envelope. It returns an error
// only when raw is not a structurally valid envelope (via CheckOutputFormat), so
// callers can tell a drifted/absent envelope (F2-NOCONFIG / F4-DRIFT territory)
// apart from a well-formed ok:false API error (F2-NOAUTH / F3-SCOPE).
func ParseEnvelope(raw []byte) (Envelope, error) {
	if err := CheckOutputFormat(raw); err != nil {
		return Envelope{}, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode lark-cli envelope: %w", err)
	}
	out := Envelope{OK: env.OK}
	if env.Error != nil {
		out.Error = &APIError{
			Type:    env.Error.Type,
			Subtype: env.Error.Subtype,
			Code:    env.Error.Code,
			Message: env.Error.Message,
		}
	}
	return out, nil
}

// unwrap parses a lark-cli response, returning the raw data payload or an error
// describing the API-level failure.
func unwrap(out []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("decode lark-cli response: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("lark-cli api error: %s", env.Error.String())
	}
	return env.Data, nil
}
