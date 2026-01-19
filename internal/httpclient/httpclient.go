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

func BuildRequest(baseURL string, ep model.Endpoint, pathVals, queryVals, bodyVals map[string]string) (RequestSpec, error) {
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
		b, err := buildJSONBody(ep, bodyVals)
		if err != nil {
			return RequestSpec{}, err
		}
		body = b
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

func formatBody(contentType string, body []byte) string {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "application/json") {
		var v any
		if err := json.Unmarshal(body, &v); err == nil {
			pretty, err := json.MarshalIndent(v, "", "  ")
			if err == nil {
				return string(pretty)
			}
		}
	}
	// Best-effort as text.
	return string(body)
}
