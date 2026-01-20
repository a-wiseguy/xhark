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
	Name     string
	Required bool
	Type     ParamType
	Example  string
}

type BodySchema struct {
	Supported bool
	Fields    []BodyField
}

type Endpoint struct {
	Method      string
	Path        string
	Summary     string
	OperationID string

	PathParams  []Param
	QueryParams []Param
	Body        *BodySchema
}
