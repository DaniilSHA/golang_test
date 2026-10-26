package docs

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte

//go:embed index.html
var Index []byte
