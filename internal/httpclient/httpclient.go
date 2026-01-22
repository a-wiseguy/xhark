package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"xhark/internal/model"
)

type Result struct {
	StatusCode int
	Status     string
	Elapsed    time.Duration
	Headers    map[string]string
	Body       string
}

type RequestSpec struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

const defaultTimeout = 10 * time.Second

func BuildRequest(baseURL string, ep model.Endpoint, pathVals, queryVals, bodyVals map[string]string, bodyRaw string) (RequestSpec, error) {
	path, err := substitutePath(ep.Path, ep.PathParams, pathVals)
	if err != nil {
		return RequestSpec{}, err
	}

	u, err := url.Parse(strings.TrimRight(baseURL, "/") + path)
	if err != nil {
		return RequestSpec{}, err
	}

	q := u.Query()
	for _, p := range ep.QueryParams {
		v := strings.TrimSpace(queryVals[p.Name])
		if v == "" {
			if p.Required {
				return RequestSpec{}, fmt.Errorf("missing required query param: %s", p.Name)
			}
			continue
		}
		// Validation/parsing is best-effort; we still send as string.
		switch p.Type {
		case model.TypeInteger:
			if _, err := strconv.ParseInt(v, 10, 64); err != nil {
				return RequestSpec{}, fmt.Errorf("invalid integer for %s", p.Name)
			}
		case model.TypeNumber:
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				return RequestSpec{}, fmt.Errorf("invalid number for %s", p.Name)
			}
		case model.TypeBoolean:
			if _, err := strconv.ParseBool(v); err != nil {
				return RequestSpec{}, fmt.Errorf("invalid boolean for %s", p.Name)
			}
		}
		q.Set(p.Name, v)
	}
	u.RawQuery = q.Encode()

	headers := map[string]string{}
	for k, v := range epDefaultHeaders(ep, bodyVals) {
		headers[k] = v
	}

	var body []byte
	if shouldSendBody(ep) {
		// If the user provided a raw JSON body (from $EDITOR), prefer that.
		raw := strings.TrimSpace(bodyRaw)
		if raw != "" {
			var check any
			if err := json.Unmarshal([]byte(raw), &check); err != nil {
				return RequestSpec{}, fmt.Errorf("invalid json body: %w", err)
			}
			body = []byte(raw)
		} else {
			b, err := buildJSONBody(ep, bodyVals)
			if err != nil {
				return RequestSpec{}, err
			}
			body = b
		}
		if body != nil {
			headers["Content-Type"] = "application/json"
		}
	}

	return RequestSpec{Method: ep.Method, URL: u.String(), Headers: headers, Body: body}, nil
}

func Execute(ctx context.Context, reqSpec RequestSpec) (Result, error) {
	client := &http.Client{Timeout: defaultTimeout}
	var body io.Reader
	if len(reqSpec.Body) > 0 {
		body = bytes.NewReader(reqSpec.Body)
	}

	req, err := http.NewRequestWithContext(ctx, reqSpec.Method, reqSpec.URL, body)
	if err != nil {
		return Result{}, err
	}
	for k, v := range reqSpec.Headers {
		if strings.TrimSpace(v) != "" {
			req.Header.Set(k, v)
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	bodyStr := formatBody(resp.Header.Get("Content-Type"), b)

	headers := map[string]string{}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		headers["content-type"] = ct
	}

	return Result{StatusCode: resp.StatusCode, Status: resp.Status, Elapsed: elapsed, Headers: headers, Body: bodyStr}, nil
}

func substitutePath(pathTpl string, params []model.Param, vals map[string]string) (string, error) {
	out := pathTpl
	for _, p := range params {
		v := strings.TrimSpace(vals[p.Name])
		if v == "" {
			return "", fmt.Errorf("missing required path param: %s", p.Name)
		}
		esc := url.PathEscape(v)
		out = strings.ReplaceAll(out, "{"+p.Name+"}", esc)
	}
	return out, nil
}

func shouldSendBody(ep model.Endpoint) bool {
	switch strings.ToUpper(ep.Method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return ep.Body != nil
	default:
		return false
	}
}

func buildJSONBody(ep model.Endpoint, vals map[string]string) ([]byte, error) {
	if ep.Body == nil {
		return nil, nil
	}
	if !ep.Body.Supported {
		// MVP: unsupported schema means "no body".
		return nil, nil
	}

	obj := map[string]any{}
	for _, f := range ep.Body.Fields {
		raw := strings.TrimSpace(vals[f.Name])
		if raw == "" {
			if f.Required {
				return nil, fmt.Errorf("missing required body field: %s", f.Name)
			}
			continue
		}

		switch f.Type {
		case model.TypeString:
			obj[f.Name] = raw
		case model.TypeInteger:
			i, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid integer for body field %s", f.Name)
			}
			obj[f.Name] = i
		case model.TypeNumber:
			n, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid number for body field %s", f.Name)
			}
			obj[f.Name] = n
		case model.TypeBoolean:
			b, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid boolean for body field %s", f.Name)
			}
			obj[f.Name] = b
		default:
			// Best-effort: treat as string.
			obj[f.Name] = raw
		}
	}

	if len(obj) == 0 {
		return nil, nil
	}
	return json.Marshal(obj)
}

func epDefaultHeaders(ep model.Endpoint, bodyVals map[string]string) map[string]string {
	h := map[string]string{}
	_ = ep
	_ = bodyVals
	return h
}

type oauthPasswordTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

func FetchOAuthPasswordToken(ctx context.Context, baseURL string, tokenURL string, username string, password string, scope string) (accessToken string, tokenType string, err error) {
	// tokenURL can be absolute or relative (FastAPI commonly uses "/token").
	full := tokenURL
	if u, perr := url.Parse(tokenURL); perr == nil && !u.IsAbs() {
		base, berr := url.Parse(strings.TrimRight(baseURL, "/") + "/")
		if berr == nil {
			full = base.ResolveReference(u).String()
		}
	}

	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", username)
	form.Set("password", password)
	if strings.TrimSpace(scope) != "" {
		form.Set("scope", strings.TrimSpace(scope))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: defaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("token request failed: %s", resp.Status)
	}

	var tr oauthPasswordTokenResponse
	if err := json.Unmarshal(b, &tr); err != nil {
		return "", "", fmt.Errorf("token response not json: %w", err)
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		return "", "", fmt.Errorf("token response missing access_token")
	}

	tt := strings.TrimSpace(tr.TokenType)
	if tt == "" {
		tt = "Bearer"
	} else {
		// Normalize common values for header.
		low := strings.ToLower(tt)
		if low == "bearer" {
			tt = "Bearer"
		}
	}
	return tr.AccessToken, tt, nil
}

func formatBody(contentType string, body []byte) string {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "application/json") {
		var v any
		if err := json.Unmarshal(body, &v); err == nil {
			return colorizeJSON(v, 0)
		}
	}
	return string(body)
}

// ansi color codes
const (
	colorReset   = "\033[0m"
	colorKey     = "\033[36m" // cyan for keys
	colorString  = "\033[32m" // green for strings
	colorNumber  = "\033[33m" // yellow for numbers
	colorBool    = "\033[35m" // magenta for booleans
	colorNull    = "\033[90m" // gray for null
	colorBracket = "\033[37m" // white for brackets
)

func colorizeJSON(v any, indent int) string {
	prefix := strings.Repeat("  ", indent)

	switch val := v.(type) {
	case nil:
		return colorNull + "null" + colorReset
	case bool:
		return colorBool + fmt.Sprintf("%v", val) + colorReset
	case float64:
		if val == float64(int64(val)) {
			return colorNumber + fmt.Sprintf("%.0f", val) + colorReset
		}
		return colorNumber + fmt.Sprintf("%v", val) + colorReset
	case string:
		return colorString + `"` + escapeJSON(val) + `"` + colorReset
	case []any:
		if len(val) == 0 {
			return colorBracket + "[]" + colorReset
		}
		var sb strings.Builder
		sb.WriteString(colorBracket + "[" + colorReset + "\n")
		for i, item := range val {
			sb.WriteString(prefix + "  " + colorizeJSON(item, indent+1))
			if i < len(val)-1 {
				sb.WriteString(",")
			}
			sb.WriteString("\n")
		}
		sb.WriteString(prefix + colorBracket + "]" + colorReset)
		return sb.String()
	case map[string]any:
		if len(val) == 0 {
			return colorBracket + "{}" + colorReset
		}
		var sb strings.Builder
		sb.WriteString(colorBracket + "{" + colorReset + "\n")
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		for i, k := range keys {
			sb.WriteString(prefix + "  " + colorKey + `"` + k + `"` + colorReset + ": ")
			sb.WriteString(colorizeJSON(val[k], indent+1))
			if i < len(keys)-1 {
				sb.WriteString(",")
			}
			sb.WriteString("\n")
		}
		sb.WriteString(prefix + colorBracket + "}" + colorReset)
		return sb.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
