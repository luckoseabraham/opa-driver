package drivers

import "github.com/open-policy-agent/opa/rego"

type Response struct {
	Trace   *string
	Input   *string
	Target  string
	Results *rego.ResultSet
}
