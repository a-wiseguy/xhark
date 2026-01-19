package openapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"xhark/internal/model"
)

const defaultTimeout = 10 * time.Second

func Load(ctx context.Context, baseURL string) (*openapi3.T, error) {
	client := &http.Client{Timeout: defaultTimeout}

	url := strings.TrimRight(baseURL, "/") + "/openapi.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// preprocess to handle openapi 3.1 numeric exclusiveMinimum/exclusiveMaximum
	// convert them to 3.0 boolean style so kin-openapi can parse
	processed := convertExclusiveBounds(rawBody)

	loader := &openapi3.Loader{Context: ctx}
	loader.IsExternalRefsAllowed = true

	doc, err := loader.LoadFromData(processed)
	if err != nil {
		return nil, fmt.Errorf("failed to parse openapi: %w", err)
	}

	// skip strict validation for 3.1 specs
	return doc, nil
}

// convertExclusiveBounds converts openapi 3.1 style numeric exclusiveMinimum/exclusiveMaximum
// to openapi 3.0 boolean style for compat with kin-openapi parser
func convertExclusiveBounds(data []byte) []byte {
	// match "exclusiveMinimum": <number> and convert to "exclusiveMinimum": true
	reMin := regexp.MustCompile(`"exclusiveMinimum"\s*:\s*(\d+(?:\.\d+)?)`)
	data = reMin.ReplaceAll(data, []byte(`"exclusiveMinimum": true, "minimum": $1`))

	reMax := regexp.MustCompile(`"exclusiveMaximum"\s*:\s*(\d+(?:\.\d+)?)`)
	data = reMax.ReplaceAll(data, []byte(`"exclusiveMaximum": true, "maximum": $1`))

	// verify it's still valid json
	var check json.RawMessage
	if json.Unmarshal(data, &check) != nil {
		return data // return original if preprocessing broke something
	}

	return data
}

// LoadFromReader loads from an io.Reader (for testing)
func LoadFromReader(ctx context.Context, r io.Reader) (*openapi3.T, error) {
	rawBody, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	processed := convertExclusiveBounds(rawBody)

	loader := &openapi3.Loader{Context: ctx}
	loader.IsExternalRefsAllowed = true

	doc, err := loader.LoadFromData(processed)
	if err != nil {
		return nil, fmt.Errorf("failed to parse openapi: %w", err)
	}

	return doc, nil
}


func ExtractEndpoints(doc *openapi3.T) []model.Endpoint {
	var out []model.Endpoint
	if doc == nil || doc.Paths == nil {
		return out
	}

	for path, item := range doc.Paths.Map() {
		if item == nil {
			continue
		}

		commonParams := item.Parameters

		addOp := func(method string, op *openapi3.Operation) {
			if op == nil {
				return
			}

			ep := model.Endpoint{
				Method:      strings.ToUpper(method),
				Path:        path,
				Summary:     strings.TrimSpace(op.Summary),
				OperationID: strings.TrimSpace(op.OperationID),
			}

			params := append(openapi3.Parameters{}, commonParams...)
			params = append(params, op.Parameters...)

			for _, p := range params {
				if p == nil || p.Value == nil {
					continue
				}
				mp := model.Param{
					Name:        p.Value.Name,
					Required:    p.Value.Required,
					Description: strings.TrimSpace(p.Value.Description),
					Type:        schemaType(p.Value.Schema),
					Example:     extractParamExample(p.Value),
				}
				switch p.Value.In {
				case "path":
					mp.In = model.ParamInPath
					ep.PathParams = append(ep.PathParams, mp)
				case "query":
					mp.In = model.ParamInQuery
					ep.QueryParams = append(ep.QueryParams, mp)
				}
			}

			ep.Body = extractBody(op)

			out = append(out, ep)
		}

		addOp("get", item.Get)
		addOp("post", item.Post)
		addOp("put", item.Put)
		addOp("patch", item.Patch)
		addOp("delete", item.Delete)
	}

	return out
}

func schemaType(ref *openapi3.SchemaRef) model.ParamType {
	if ref == nil || ref.Value == nil {
		return model.TypeUnknown
	}
	if ref.Value.Type == nil {
		return model.TypeUnknown
	}
	if ref.Value.Type.Is("string") {
		return model.TypeString
	}
	if ref.Value.Type.Is("integer") {
		return model.TypeInteger
	}
	if ref.Value.Type.Is("number") {
		return model.TypeNumber
	}
	if ref.Value.Type.Is("boolean") {
		return model.TypeBoolean
	}
	return model.TypeUnknown
}

func extractParamExample(p *openapi3.Parameter) string {
	if p == nil {
		return ""
	}
	// check param-level example first
	if p.Example != nil {
		return fmt.Sprintf("%v", p.Example)
	}
	// check schema example
	if p.Schema != nil && p.Schema.Value != nil && p.Schema.Value.Example != nil {
		return fmt.Sprintf("%v", p.Schema.Value.Example)
	}
	return ""
}

func extractSchemaExample(ref *openapi3.SchemaRef) string {
	if ref == nil || ref.Value == nil {
		return ""
	}
	if ref.Value.Example != nil {
		return fmt.Sprintf("%v", ref.Value.Example)
	}
	return ""
}

func extractBody(op *openapi3.Operation) *model.BodySchema {
	if op == nil || op.RequestBody == nil || op.RequestBody.Value == nil {
		return nil
	}

	mt := op.RequestBody.Value.Content.Get("application/json")
	if mt == nil || mt.Schema == nil || mt.Schema.Value == nil {
		return nil
	}

	s := mt.Schema.Value
	if s.Type == nil || !s.Type.Is("object") {
		return &model.BodySchema{Supported: false}
	}

	required := map[string]bool{}
	for _, name := range s.Required {
		required[name] = true
	}

	var fields []model.BodyField
	supported := true
	for name, prop := range s.Properties {
		t := schemaType(prop)
		if t == model.TypeUnknown {
			supported = false
		}
		fields = append(fields, model.BodyField{
			Name:     name,
			Required: required[name],
			Type:     t,
			Example:  extractSchemaExample(prop),
		})
	}

	return &model.BodySchema{Supported: supported, Fields: fields}
}
