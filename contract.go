package profiles

import _ "embed"

//go:embed contracts/profiles.openapi.json
var contract []byte

func OpenAPI() []byte { return append([]byte(nil), contract...) }
