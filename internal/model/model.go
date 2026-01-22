package model

type ParamLocation string

type ParamType string

const (
	ParamInPath  ParamLocation = "path"
	ParamInQuery ParamLocation = "query"

	TypeString  ParamType = "string"
	TypeInteger ParamType = "integer"
	TypeNumber  ParamType = "number"
	TypeBoolean ParamType = "boolean"
	TypeUnknown ParamType = "unknown"
)

type Param struct {
	Name        string
	In          ParamLocation
	Required    bool
	Type        ParamType
	Description string
	Example     string
	Enum        []string
	Default     string
}

type BodyField struct {
	Name        string
	Required    bool
	Type        ParamType
	Description string
	Example     string
	Enum        []string
	Default     string
}

type BodySchema struct {
	Supported bool
	Fields    []BodyField
}

type SecurityScheme struct {
	Name        string
	Type        string // http, oauth2, apiKey, openIdConnect
	Description string

	// http
	Scheme       string // bearer, basic, etc.
	BearerFormat string

	// oauth2 password flow (MVP)
	TokenURL string
	Scopes   map[string]string
}

type SecurityRequirement map[string][]string // schemeName -> required scopes

type Endpoint struct {
	Method      string
	Path        string
	Summary     string
	OperationID string

	PathParams  []Param
	QueryParams []Param
	Body        *BodySchema

	// Security are the effective security requirements for this operation.
	// If empty, the endpoint may still inherit global security.
	Security []SecurityRequirement
}
